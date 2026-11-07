//go:build !linux

package fsx

import "golang.org/x/sys/unix"

// blockSize is the unit of Blocks, Bfree and Bavail (Bsize outside Linux).
func blockSize(st *unix.Statfs_t) uint64 { return uint64(st.Bsize) }
