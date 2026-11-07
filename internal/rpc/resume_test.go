package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bfbackup/internal/model"
)

func TestRemoteResumeWithCompression(t *testing.T) {
	r, root := pair(t)
	ctx := WithCompression(context.Background())
	data := bytes.Repeat([]byte("resume me, compress me. "), 200_000) // ~4.8 MB
	src := filepath.Join(root, "src.bin")
	os.WriteFile(src, data, 0o640)
	want := sha256.Sum256(data)
	rd, err := r.Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	e := rd.Entry()
	rd.Close()
	dst := filepath.Join(root, "copy/dst.bin")

	// First attempt (uncompressed) breaks after half the file.
	w, have, err := r.CreateResumable(context.Background(), dst, e)
	if err != nil || have != 0 {
		t.Fatalf("create: have=%d err=%v", have, err)
	}
	half := len(data) / 2
	if _, err := w.Write(data[:half]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	w.Abort()

	// The peer keeps what arrived; wait until it has written it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		w, have, err = r.CreateResumable(ctx, dst, e)
		if err != nil {
			t.Fatal(err)
		}
		if have > 0 || time.Now().After(deadline) {
			break
		}
		w.Abort()
		time.Sleep(20 * time.Millisecond)
	}
	if have <= 0 || have > int64(half) {
		t.Fatalf("have = %d, want 1..%d", have, half)
	}
	rd, err = r.OpenAt(ctx, src, have)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(w, rd)
	if err != nil || n != int64(len(data))-have {
		t.Fatalf("copied %d (want %d) %v", n, int64(len(data))-have, err)
	}
	rd.Close()
	if rd.Hash() != hex.EncodeToString(want[:]) {
		t.Fatal("source hash does not cover the whole file")
	}
	sum, err := w.Commit(ctx, rd.Hash())
	if err != nil || sum != rd.Hash() {
		t.Fatalf("commit %q %v", sum, err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) {
		t.Fatal("content mismatch")
	}

	// Compressed reads deliver the same data and errors as plain ones.
	rd, err = r.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !rd.(*remoteReader).s.ctx.Value(compressKey{}).(bool) || rd.(*remoteReader).zr == nil {
		t.Fatal("read was not compressed")
	}
	back, err := io.ReadAll(rd)
	rd.Close()
	if err != nil || !bytes.Equal(back, data) || rd.Hash() != hex.EncodeToString(want[:]) {
		t.Fatalf("compressed read: %d bytes, %v", len(back), err)
	}
	if _, err := r.OpenAt(ctx, src, int64(len(data))+1); err == nil {
		t.Fatal("offset past the end accepted")
	}
}

func TestIncompressibleTypesAreSentPlain(t *testing.T) {
	r, root := pair(t)
	ctx := WithCompression(context.Background())
	os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("not really a movie"), 0o644)
	rd, err := r.Open(ctx, filepath.Join(root, "movie.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	if rd.(*remoteReader).zr != nil {
		t.Fatal("mkv was compressed")
	}
	if b, err := io.ReadAll(rd); err != nil || string(b) != "not really a movie" {
		t.Fatalf("read %q %v", b, err)
	}
}

func TestCompressedScan(t *testing.T) {
	r, root := pair(t)
	n := scanBatch + 5
	for i := 0; i < n; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("f%05d", i)), nil, 0o644)
	}
	total := 0
	err := r.Scan(WithCompression(context.Background()), model.ScanRequest{Root: root}, func(l model.DirListing) error {
		total += len(l.Entries)
		return nil
	})
	if err != nil || total != n {
		t.Fatalf("total=%d err=%v", total, err)
	}
}

func TestConnErrorsAreMarked(t *testing.T) {
	if !model.IsConnError(ErrStalled) {
		t.Fatal("a stall must count as a lost connection")
	}
	s := &stream{ctx: context.Background(), idle: time.Second}
	if err := s.fix(io.ErrUnexpectedEOF); !errors.Is(err, model.ErrConnLost) {
		t.Fatalf("broken stream: %v, want ErrConnLost", err)
	}
	if err := s.fix(os.ErrDeadlineExceeded); !errors.Is(err, ErrStalled) || !model.IsConnError(err) {
		t.Fatalf("deadline: %v, want ErrStalled", err)
	}
	if s.fix(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.ctx = ctx
	if err := s.fix(io.ErrUnexpectedEOF); !errors.Is(err, context.Canceled) {
		t.Fatalf("own cancel: %v, want context.Canceled", err)
	}

	// Errors of the peer are not connection errors.
	r, root := pair(t)
	if _, err := r.Hash(context.Background(), filepath.Join(root, "missing")); model.IsConnError(err) {
		t.Fatalf("peer error marked as connection error: %v", err)
	}
}
