package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/model"
)

// flakyEP is a target whose first resumable writes break after failAfter
// bytes, like a connection that drops in the middle of a large file.
type flakyEP struct {
	*endpoint.Local
	failAfter int64
	fails     int
	haves     []int64
	waits     int
	waitErr   error
}

func (f *flakyEP) CreateResumable(ctx context.Context, path string, e model.Entry) (endpoint.Writer, int64, error) {
	w, have, err := f.Local.CreateResumable(ctx, path, e)
	f.haves = append(f.haves, have)
	if err == nil && f.fails > 0 {
		f.fails--
		return &breakingWriter{Writer: w, left: f.failAfter}, have, nil
	}
	return w, have, err
}

func (f *flakyEP) WaitOnline(context.Context, time.Duration) error {
	f.waits++
	return f.waitErr
}

type breakingWriter struct {
	endpoint.Writer
	left int64
}

func (b *breakingWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > b.left {
		n, _ := b.Writer.Write(p[:b.left])
		b.left = 0
		return n, fmt.Errorf("%w: test drop", model.ErrConnLost)
	}
	b.left -= int64(len(p))
	return b.Writer.Write(p)
}

func TestCopyResumesAfterDrop(t *testing.T) {
	e := setup(t)
	old := resumeMin
	resumeMin = 1 << 10
	t.Cleanup(func() { resumeMin = old })

	data := bytes.Repeat([]byte("0123456789abcdef"), 40_000) // 640 KB
	e.write(e.src, "big.bin", string(data))
	src, dst := filepath.Join(e.src, "big.bin"), filepath.Join(e.dst, "big.bin")
	hint, err := e.ep.FS.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakyEP{Local: e.ep, failAfter: 200 << 10, fails: 1}
	prog := NewProgress()
	ent, sum, err := copyFileHint(e.ctx, e.ep, flaky, src, dst, make([]byte, 64<<10), prog, nil, &hint)
	if err != nil {
		t.Fatal(err)
	}
	if len(flaky.haves) != 2 || flaky.haves[0] != 0 || flaky.haves[1] != 200<<10 {
		t.Fatalf("haves = %v, want [0 %d]: the second attempt must continue", flaky.haves, 200<<10)
	}
	if flaky.waits != 1 {
		t.Fatalf("waits = %d, want 1", flaky.waits)
	}
	if ent.Size != int64(len(data)) || sum == "" || prog.BytesDone.Load() != int64(len(data)) {
		t.Fatalf("ent=%+v sum=%q bytes=%d", ent, sum, prog.BytesDone.Load())
	}
	if got := e.read(e.dst, "big.bin"); got != string(data) {
		t.Fatal("content mismatch")
	}
}

func TestPeerThatStaysAwayStopsTheRun(t *testing.T) {
	e := setup(t)
	e.write(e.src, "a", "abc")
	gone := fmt.Errorf("peer: %w: no connection for 10m", model.ErrOffline)
	flaky := &flakyEP{Local: e.ep, failAfter: 1, fails: 1, waitErr: gone}
	old := resumeMin
	resumeMin = 1
	t.Cleanup(func() { resumeMin = old })

	hint, _ := e.ep.FS.Stat(filepath.Join(e.src, "a"))
	_, _, err := copyFileHint(e.ctx, e.ep, flaky, filepath.Join(e.src, "a"), filepath.Join(e.dst, "a"), make([]byte, 1024), nil, nil, &hint)
	if !errors.Is(err, model.ErrOffline) {
		t.Fatalf("err = %v, want offline", err)
	}

	ctx, stop := guardOffline(e.ctx)
	defer stop()
	if offline(ctx, errors.New("disk full")) || ctx.Err() != nil {
		t.Fatal("other errors must not stop the run")
	}
	if !offline(ctx, err) || ctx.Err() == nil {
		t.Fatal("offline did not stop the run")
	}
	if oe := offlineErr(ctx); !errors.Is(oe, model.ErrOffline) {
		t.Fatalf("offlineErr = %v", oe)
	}
}

func TestRetryScanAfterDrop(t *testing.T) {
	waiter := &flakyEP{}
	calls := 0
	err := retryScan(context.Background(), waiter, func() error {
		if calls++; calls < 3 {
			return fmt.Errorf("%w: test drop", model.ErrConnLost)
		}
		return nil
	})
	if err != nil || calls != 3 || waiter.waits != 2 {
		t.Fatalf("err=%v calls=%d waits=%d", err, calls, waiter.waits)
	}
	calls = 0
	err = retryScan(context.Background(), waiter, func() error { calls++; return os.ErrPermission })
	if !errors.Is(err, os.ErrPermission) || calls != 1 {
		t.Fatalf("other errors are not retried: err=%v calls=%d", err, calls)
	}
}
