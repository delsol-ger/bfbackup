package fsx

import (
	"os"
	"path"
	"sync"
	"time"
)

// unraidMnt is where Unraid mounts the user shares (user/) and the single
// disks and pools they are made of (disk1/, cache/, …); tests replace it.
var unraidMnt = "/mnt"

// mountinfoPath is read for the current mounts; tests replace it.
var mountinfoPath = "/proc/self/mountinfo"

// notUnraidDisks are the folders below /mnt that are no single disk of the
// user shares: the shares themselves and the containers of unassigned,
// remote and plugin mounts.
var notUnraidDisks = map[string]bool{"user": true, "disks": true, "remotes": true, "addons": true, "rootshare": true}

// readMounts returns the current mounts, none when they cannot be read.
func readMounts() []mount {
	b, err := os.ReadFile(mountinfoPath)
	if err != nil {
		return nil
	}
	return parseMountinfo(string(b))
}

// disksTTL is how long trashDisks keeps the disks it found.
const disksTTL = 10 * time.Second

var disksCache struct {
	sync.Mutex
	key   string // unraidMnt and mountinfoPath it was read for
	at    time.Time
	disks []mount
}

// trashDisks is unraidDisks(readMounts()), kept for a few seconds: a sync
// trashes many files in a row, and each would read the mounts again.
func trashDisks() []mount {
	c := &disksCache
	c.Lock()
	defer c.Unlock()
	key := unraidMnt + "\x00" + mountinfoPath
	if c.key != key || time.Since(c.at) > disksTTL {
		c.key, c.at, c.disks = key, time.Now(), unraidDisks(readMounts())
	}
	return c.disks
}

// unraidDisks returns the single disks and pools behind Unraid's user shares
// (/mnt/disk1, /mnt/cache, …, and /mnt/user0) that are visible here. They
// are never shown: BFBackup works on /mnt/user, and the disks are only used
// to move files into the trash without copying them. Empty when /mnt/user
// is no user share mount.
func unraidDisks(mounts []mount) []mount {
	shfs := false
	for _, m := range mounts {
		if m.point == unraidMnt+"/user" && m.fstype == "fuse.shfs" {
			shfs = true
		}
	}
	if !shfs {
		return nil
	}
	var out []mount
	for _, m := range mounts {
		if path.Dir(m.point) == unraidMnt && !notUnraidDisks[path.Base(m.point)] && !pseudoFS[m.fstype] {
			out = append(out, m)
		}
	}
	return out
}

// onUnraidDisk reports whether p is one of the disks or below one.
func onUnraidDisk(p string, disks []mount) bool {
	for _, d := range disks {
		if within(p, d.point) {
			return true
		}
	}
	return false
}

// isUnraidMnt reports whether p is /mnt itself on Unraid. Mapped into the
// container as one path, /mnt is a mount of the host's RAM root file system
// that only holds the mount points, never data.
func isUnraidMnt(p string, mounts []mount) bool {
	if p != unraidMnt {
		return false
	}
	for _, m := range mounts {
		if m.point == unraidMnt+"/user" && m.fstype == "fuse.shfs" {
			return true
		}
	}
	return false
}
