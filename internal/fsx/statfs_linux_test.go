package fsx

import (
	"testing"

	"golang.org/x/sys/unix"
)

// virtiofs (Docker Desktop) reports a 1 MiB Bsize with 4 KiB fragments.
func TestBlockSizeUsesFrsize(t *testing.T) {
	st := unix.Statfs_t{Bsize: 1 << 20, Frsize: 4096}
	if got := blockSize(&st); got != 4096 {
		t.Fatalf("blockSize = %d, want 4096", got)
	}
	st.Frsize = 0
	if got := blockSize(&st); got != 1<<20 {
		t.Fatalf("blockSize without Frsize = %d, want Bsize", got)
	}
}
