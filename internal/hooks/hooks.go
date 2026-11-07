// Package hooks runs pre/post backup scripts.
//
// Scripts live in a directory on the node that runs them (/config/hooks)
// and are referenced by file name only. The controller can choose which
// script runs, never what it contains, so a compromised controller or UI
// cannot run arbitrary commands on a peer.
package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"

	"bfbackup/internal/model"
)

// DefaultTimeout bounds a hook when Runner.Timeout is zero.
const DefaultTimeout = time.Hour

// MaxOutput is how much combined output is kept.
const MaxOutput = 64 << 10

var (
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	envKeyRE = regexp.MustCompile(`^BFB_[A-Z0-9_]{1,64}$`)
)

// ErrNotFound is returned for unknown or non-executable hooks.
var ErrNotFound = errors.New("hook not found or not executable")

// Runner runs scripts from Dir.
type Runner struct {
	Dir     string
	Timeout time.Duration
}

// List returns the names of executable regular files in Dir.
func (r Runner) List() ([]string, error) {
	if r.Dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(r.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !nameRE.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// Run executes the named hook with extra BFB_* environment variables.
// A non-zero exit code is reported in the result, not as an error.
func (r Runner) Run(ctx context.Context, name string, env map[string]string) (model.HookResult, error) {
	if r.Dir == "" || !nameRE.MatchString(name) {
		return model.HookResult{}, fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	p := filepath.Join(r.Dir, name)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return model.HookResult{}, fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p)
	cmd.Dir = r.Dir
	cmd.Env = os.Environ()
	for k, v := range env {
		if envKeyRE.MatchString(k) {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	out := &limitedBuffer{max: MaxOutput}
	cmd.Stdout = out
	cmd.Stderr = out
	// Own process group, so a timeout kills the script and its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second

	start := time.Now()
	err = cmd.Run()
	res := model.HookResult{Output: out.String(), DurationMs: time.Since(start).Milliseconds()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		if ctx.Err() != nil {
			res.Output += "\n[bfbackup] hook timed out or was cancelled"
		}
	default:
		return res, fmt.Errorf("hook %q: %w", name, err)
	}
	return res, nil
}

type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room < len(p) {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return b.buf.String() + "\n[bfbackup] output truncated"
	}
	return b.buf.String()
}
