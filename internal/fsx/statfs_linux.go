package fsx

import "golang.org/x/sys/unix"

// blockSize is the unit of Blocks, Bfree and Bavail. On Linux that is
// Frsize; Bsize is only the preferred I/O size and can be much larger
// (virtiofs and FUSE report 1 MiB, which inflates a 1 TB disk to 232 TiB).
func blockSize(st *unix.Statfs_t) uint64 {
	if st.Frsize > 0 {
		return uint64(st.Frsize)
	}
	return uint64(st.Bsize)
}
