package fsx

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bfbackup/internal/model"
)

func partials(t *testing.T, dir string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), resumePrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestCreateResumableContinues(t *testing.T) {
	root := t.TempDir()
	f := newFS(t, root)
	data := []byte("0123456789")
	sum := sha256.Sum256(data)
	e := model.Entry{Name: "f", Type: model.TypeFile, Size: int64(len(data)), MTime: 1e18, Mode: 0o640}
	dst := filepath.Join(root, "d/f")

	w, have, err := f.CreateResumable(dst, e)
	if err != nil || have != 0 {
		t.Fatalf("first create: have=%d err=%v", have, err)
	}
	w.Write(data[:4])
	w.Abort() // keeps the 4 bytes
	if p := partials(t, filepath.Join(root, "d")); len(p) != 1 {
		t.Fatalf("partials after abort = %v", p)
	}

	w, have, err = f.CreateResumable(dst, e)
	if err != nil || have != 4 {
		t.Fatalf("second create: have=%d err=%v", have, err)
	}
	w.Write(data[4:])
	got, err := w.Commit(hex.EncodeToString(sum[:]))
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("commit %q %v", got, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != string(data) {
		t.Fatalf("content %q", b)
	}
	if p := partials(t, filepath.Join(root, "d")); len(p) != 0 {
		t.Fatalf("partials after commit = %v", p)
	}
}

func TestCreateResumableNewVersionStartsOver(t *testing.T) {
	root := t.TempDir()
	f := newFS(t, root)
	e := model.Entry{Name: "f", Type: model.TypeFile, Size: 10, MTime: 1}
	dst := filepath.Join(root, "f")
	w, _, _ := f.CreateResumable(dst, e)
	w.Write([]byte("abcd"))
	w.Abort()

	e.MTime = 2 // the source file changed
	w, have, err := f.CreateResumable(dst, e)
	if err != nil || have != 0 {
		t.Fatalf("have=%d err=%v, want a fresh start", have, err)
	}
	if p := partials(t, root); len(p) != 1 {
		t.Fatalf("old version not removed: %v", p)
	}
	w.Write([]byte("0123456789"))
	w.Commit("")

	// A failed commit discards the partial data.
	e.MTime = 3
	w, _, _ = f.CreateResumable(dst, e)
	w.Write([]byte("0123456789"))
	if _, err := w.Commit("deadbeef"); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if p := partials(t, root); len(p) != 0 {
		t.Fatalf("partial kept after failed commit: %v", p)
	}
}

func TestCreateResumableCleansLegacyPartials(t *testing.T) {
	root := t.TempDir()
	f := newFS(t, root)
	e := model.Entry{Name: "f", Type: model.TypeFile, Size: 10, MTime: 1}
	pk, vk := resumeKeys("f", e)
	same := legacyResumePrefix + pk + "-" + vk + model.TempSuffix // same file, old naming
	other := legacyResumePrefix + "0000000000000000-00000000" + model.TempSuffix
	write(t, root, same, "abcd")
	write(t, root, other, "xyz")
	old := time.Now().Add(-2 * stalePartial)
	os.Chtimes(filepath.Join(root, other), old, old)

	w, have, err := f.CreateResumable(filepath.Join(root, "f"), e)
	if err != nil || have != 0 {
		t.Fatalf("have=%d err=%v, an old-style partial must not be continued", have, err)
	}
	defer w.Abort()
	for _, n := range []string{same, other} {
		if _, err := os.Stat(filepath.Join(root, n)); !os.IsNotExist(err) {
			t.Fatalf("old-style partial %s not removed", n)
		}
	}
}

func TestOpenAtHashesWholeFile(t *testing.T) {
	root := t.TempDir()
	f := newFS(t, root)
	write(t, root, "f", "hello world")
	r, err := f.OpenAt(filepath.Join(root, "f"), 6)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "world" {
		t.Fatalf("read %q %v", b, err)
	}
	sum := sha256.Sum256([]byte("hello world"))
	if r.Hash() != hex.EncodeToString(sum[:]) {
		t.Fatal("hash does not cover the skipped part")
	}
	if _, err := f.OpenAt(filepath.Join(root, "f"), 100); err == nil {
		t.Fatal("offset past the end accepted")
	}
}
