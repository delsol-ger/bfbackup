package rpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/fsx"
	"bfbackup/internal/hooks"
	"bfbackup/internal/model"
)

// pair returns a Remote talking to a Server that serves a Local endpoint on
// a temp dir. The connection is real TCP on loopback.
func pair(t *testing.T) (*Remote, string) {
	t.Helper()
	root := t.TempDir()
	hookDir := t.TempDir()
	os.WriteFile(filepath.Join(hookDir, "hi.sh"), []byte("#!/bin/sh\necho hi $BFB_X\n"), 0o755)
	f, err := fsx.New([]string{root}, fsx.Options{UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	local := &endpoint.Local{FS: f, Hooks: hooks.Runner{Dir: hookDir}, Version: "test"}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		sess, _ := yamux.Client(c, SessionConfig(nil))
		(&Server{EP: local}).Serve(ctx, sess)
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := yamux.Server(c, SessionConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		sess.Close()
		ln.Close()
		f.Close()
	})
	return &Remote{Session: sess}, root
}

func TestRemoteSimpleCalls(t *testing.T) {
	r, root := pair(t)
	ctx := context.Background()
	os.MkdirAll(filepath.Join(root, "d"), 0o755)
	os.WriteFile(filepath.Join(root, "d/f.txt"), []byte("abc"), 0o644)

	info, err := r.Info(ctx)
	if err != nil || info.Version != "test" || len(info.Roots) != 1 || len(info.Hooks) != 1 {
		t.Fatalf("info = %+v %v", info, err)
	}
	l, err := r.ListDir(ctx, root)
	if err != nil || len(l) != 1 || l[0].Name != "d" {
		t.Fatalf("listdir = %+v %v", l, err)
	}
	st, err := r.Stat(ctx, []string{filepath.Join(root, "d/f.txt"), filepath.Join(root, "nope")})
	if err != nil || !st[0].Exists || st[0].Entry.Size != 3 || st[1].Exists {
		t.Fatalf("stat = %+v %v", st, err)
	}
	h, err := r.Hash(ctx, filepath.Join(root, "d/f.txt"))
	if err != nil || h != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("hash = %s %v", h, err)
	}
	if _, err := r.Hash(ctx, filepath.Join(root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing hash err = %v (want ErrNotExist across the wire)", err)
	}
	if _, err := r.ListDir(ctx, "/etc"); !errors.Is(err, model.ErrOutsideRoot) {
		t.Fatalf("outside err = %v", err)
	}
	if u, err := r.StatFS(ctx, root); err != nil || u.Total == 0 {
		t.Fatalf("statfs %+v %v", u, err)
	}
	res, err := r.RunHook(ctx, "hi.sh", map[string]string{"BFB_X": "there"})
	if err != nil || res.Output != "hi there\n" {
		t.Fatalf("hook %+v %v", res, err)
	}
	if err := r.Symlink(ctx, filepath.Join(root, "lnk"), "d/f.txt"); err != nil {
		t.Fatal(err)
	}
	if err := r.Trash(ctx, root, "d", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := r.PurgeTrash(ctx, root, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("purge %d %v", n, err)
	}
	if err := r.Remove(ctx, root, "lnk"); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(ctx, root, "lnk"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removing a missing path: %v, want not exist", err)
	}
}

func TestRemoteScanSplitsLargeDirs(t *testing.T) {
	r, root := pair(t)
	n := scanBatch*2 + 17
	for i := 0; i < n; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("f%05d", i)), nil, 0o644)
	}
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub/x"), []byte("x"), 0o644)
	if err := syscall.Mkfifo(filepath.Join(root, "zz-pipe"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := map[string]int{}
	total := 0
	var skipped []string
	err := r.Scan(context.Background(), model.ScanRequest{Root: root}, func(l model.DirListing) error {
		calls[l.Dir]++
		total += len(l.Entries)
		skipped = append(skipped, l.Skipped...)
		if l.More {
			t.Error("partial listing leaked to caller")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls[""] != 1 || calls["sub"] != 1 || total != n+2 {
		t.Fatalf("calls=%v total=%d", calls, total)
	}
	// Special files survive the split into parts.
	if strings.Join(skipped, ",") != "zz-pipe" {
		t.Fatalf("skipped = %v", skipped)
	}

	stop := errors.New("stop")
	err = r.Scan(context.Background(), model.ScanRequest{Root: root}, func(model.DirListing) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v", err)
	}
	err = r.Scan(context.Background(), model.ScanRequest{Root: root, Includes: []string{"nope"}}, func(model.DirListing) error { return nil })
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing include err = %v", err)
	}
}

func TestRemoteReadWrite(t *testing.T) {
	r, root := pair(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("bfbackup!"), 500_000) // ~4.5 MB, several chunks
	src := filepath.Join(root, "src.bin")
	os.WriteFile(src, data, 0o640)

	rd, err := r.Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	e := rd.Entry()
	if e.Size != int64(len(data)) {
		t.Fatalf("entry %+v", e)
	}
	w, err := r.Create(ctx, filepath.Join(root, "copy/dst.bin"), e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, rd); err != nil {
		t.Fatal(err)
	}
	rd.Close()
	sum, err := w.Commit(ctx, rd.Hash())
	if err != nil || sum != rd.Hash() || sum == "" {
		t.Fatalf("commit %q %q %v", sum, rd.Hash(), err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "copy/dst.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("content mismatch")
	}

	// Hash mismatch is reported with its code.
	w, _ = r.Create(ctx, filepath.Join(root, "copy/bad.bin"), model.Entry{Size: 3})
	w.Write([]byte("abc"))
	if _, err := w.Commit(ctx, "deadbeef"); !errors.Is(err, model.ErrHashMismatch) {
		t.Fatalf("mismatch err = %v", err)
	}

	// Abort leaves no temp files behind.
	w, _ = r.Create(ctx, filepath.Join(root, "copy/aborted.bin"), model.Entry{Size: 10})
	w.Write([]byte("abc"))
	w.Abort()
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _ := os.ReadDir(filepath.Join(root, "copy"))
		if len(entries) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("temp file left: %v", entries)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := r.Open(ctx, filepath.Join(root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open missing err = %v", err)
	}
}

func TestRemoteCancel(t *testing.T) {
	r, root := pair(t)
	os.WriteFile(filepath.Join(root, "f"), make([]byte, 8<<20), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	rd, err := r.Open(ctx, filepath.Join(root, "f"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	rd.Read(buf)
	cancel()
	_, err = io.Copy(io.Discard, rd)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.Info(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("info after cancel err = %v", err)
	}
}

func TestRemoteOffline(t *testing.T) {
	r, _ := pair(t)
	r.Session.(*yamux.Session).Close()
	if _, err := r.Info(context.Background()); !errors.Is(err, model.ErrOffline) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownMethodMeansUnsupported(t *testing.T) {
	// What a peer of an older version answers for a call it does not know.
	if err := (response{Err: "unknown method remove"}).error(); !errors.Is(err, model.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if err := (response{Err: "boom"}).error(); errors.Is(err, model.ErrUnsupported) {
		t.Fatal("other errors are not unsupported")
	}
}
