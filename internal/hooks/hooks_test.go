package hooks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func script(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestRunAndList(t *testing.T) {
	dir := t.TempDir()
	script(t, dir, "ok.sh", `echo "job=$BFB_JOB secret=$NOT_ALLOWED"`, 0o755)
	script(t, dir, "fail.sh", "echo bad >&2; exit 3", 0o755)
	script(t, dir, "noexec.sh", "true", 0o644)
	script(t, dir, "slow.sh", "sleep 5", 0o755)
	r := Runner{Dir: dir}

	names, err := r.List()
	if err != nil || strings.Join(names, ",") != "fail.sh,ok.sh,slow.sh" {
		t.Fatalf("List = %v %v", names, err)
	}

	res, err := r.Run(context.Background(), "ok.sh", map[string]string{"BFB_JOB": "photos", "NOT_ALLOWED": "x"})
	if err != nil || res.ExitCode != 0 || strings.TrimSpace(res.Output) != "job=photos secret=" {
		t.Fatalf("ok: %+v %v", res, err)
	}
	res, err = r.Run(context.Background(), "fail.sh", nil)
	if err != nil || res.ExitCode != 3 || !strings.Contains(res.Output, "bad") {
		t.Fatalf("fail: %+v %v", res, err)
	}
	for _, name := range []string{"noexec.sh", "../ok.sh", "missing", ""} {
		if _, err := r.Run(context.Background(), name, nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: err = %v", name, err)
		}
	}
	r.Timeout = 100 * time.Millisecond
	res, err = r.Run(context.Background(), "slow.sh", nil)
	if err != nil || res.ExitCode == 0 || !strings.Contains(res.Output, "timed out") {
		t.Fatalf("timeout: %+v %v", res, err)
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 4}
	b.Write([]byte("abc"))
	b.Write([]byte("def"))
	if got := b.String(); !strings.HasPrefix(got, "abcd") || !strings.Contains(got, "truncated") {
		t.Fatalf("got %q", got)
	}
}

func TestEmptyDir(t *testing.T) {
	if names, err := (Runner{}).List(); err != nil || names != nil {
		t.Fatal(names, err)
	}
	if names, err := (Runner{Dir: "/does/not/exist"}).List(); err != nil || names != nil {
		t.Fatal(names, err)
	}
}
