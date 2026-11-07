package model

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
)

func TestCleanRel(t *testing.T) {
	ok := map[string]string{
		"":          "",
		".":         "",
		"a":         "a",
		"a/b/":      "a/b",
		"a//b":      "a/b",
		"./a/./b":   "a/b",
		"Movies/HD": "Movies/HD",
	}
	for in, want := range ok {
		got, err := CleanRel(in)
		if err != nil || got != want {
			t.Errorf("CleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/abs", "..", "a/../b", "a/..", "a\x00b"} {
		if _, err := CleanRel(bad); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanRel(%q) err = %v; want ErrInvalidPath", bad, err)
		}
	}
}

func TestCleanAbs(t *testing.T) {
	if got, err := CleanAbs("/mnt/user//x/"); err != nil || got != "/mnt/user/x" {
		t.Fatalf("got %q %v", got, err)
	}
	for _, bad := range []string{"rel", "/mnt/../etc", ""} {
		if _, err := CleanAbs(bad); err == nil {
			t.Errorf("CleanAbs(%q) accepted", bad)
		}
	}
}

func TestErrorCodesRoundTrip(t *testing.T) {
	for _, sentinel := range []error{fs.ErrNotExist, ErrChanged, ErrHashMismatch, ErrOutsideRoot, ErrTypeConflict} {
		wrapped := fmt.Errorf("op: %w", sentinel)
		back := ErrorFromCode(ErrorCode(wrapped), wrapped.Error())
		if !errors.Is(back, sentinel) {
			t.Errorf("%v did not survive round trip", sentinel)
		}
		if back.Error() != wrapped.Error() {
			t.Errorf("message changed: %q", back.Error())
		}
	}
	if ErrorFromCode("", "") != nil {
		t.Error("empty code and message must be nil")
	}
	if err := ErrorFromCode("", "plain"); err == nil || err.Error() != "plain" {
		t.Errorf("plain error = %v", err)
	}
}

func TestSplitJoinRel(t *testing.T) {
	if d, n := SplitRel("a/b/c.txt"); d != "a/b" || n != "c.txt" {
		t.Fatal(d, n)
	}
	if d, n := SplitRel("c.txt"); d != "" || n != "c.txt" {
		t.Fatal(d, n)
	}
	if JoinRel("", "x") != "x" || JoinRel("a", "x") != "a/x" {
		t.Fatal("JoinRel")
	}
	if !IsInternalName(".BFBackup-abc.tmp") || !IsInternalName(".bfb-abc.tmp") || IsInternalName(".BFBackup-abc") ||
		IsInternalName(".bfb-abc") || IsInternalName("x.tmp") {
		t.Fatal("IsInternalName")
	}
}
