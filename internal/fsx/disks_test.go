package fsx

import (
	"strings"
	"testing"
)

// Trimmed mountinfo of an Unraid container with /mnt/user and an
// unassigned disk mounted below /mnt.
const sampleMountinfo = `
1 0 0:50 / / rw,relatime - overlay overlay rw,lowerdir=/x
2 1 0:52 / /proc rw - proc proc rw
3 1 0:53 / /dev rw - tmpfs tmpfs rw
4 1 0:40 /appdata/bfbackup /config rw - xfs /dev/md1p1 rw
5 1 0:41 / /mnt/user rw - fuse.shfs shfs rw
6 1 0:60 / /mnt/disks rw - tmpfs tmpfs rw
7 6 8:33 / /mnt/disks/USB\040Disk rw - exfat /dev/sdc1 rw
`

func TestParseMountinfo(t *testing.T) {
	m := parseMountinfo(sampleMountinfo)
	if len(m) != 7 || m[6].point != "/mnt/disks/USB Disk" || m[6].fstype != "exfat" || m[4].fstype != "fuse.shfs" {
		t.Fatalf("parsed %+v", m)
	}
}

func TestDiskPaths(t *testing.T) {
	m := parseMountinfo(sampleMountinfo)
	got := diskPaths([]string{"/mnt"}, m)
	if strings.Join(got, ",") != "/mnt/disks/USB Disk,/mnt/user" {
		t.Fatalf("roots /mnt: %v", got)
	}
	// The single disks and pools of the user shares are never listed, nor
	// datasets below them.
	// /mnt mapped as one path is a mount of the host's root file system.
	m = parseMountinfo(sampleMountinfo + `
12 1 0:2 /mnt /mnt rw - rootfs rootfs rw
8 1 9:1 / /mnt/disk1 rw - xfs /dev/md1p1 rw
9 1 0:42 / /mnt/cache rw - zfs cache rw
10 9 0:43 / /mnt/cache/appdata rw - zfs cache/appdata rw
11 1 0:44 / /mnt/user0 rw - fuse.shfs shfs rw
`)
	got = diskPaths([]string{"/mnt"}, m)
	if strings.Join(got, ",") != "/mnt/disks/USB Disk,/mnt/user" {
		t.Fatalf("roots /mnt with single disks: %v", got)
	}
	// A root that is not a mount point itself and has nothing below it.
	got = diskPaths([]string{"/mnt/user/Backups"}, m)
	if strings.Join(got, ",") != "/mnt/user/Backups" {
		t.Fatalf("nested root: %v", got)
	}
}

func TestDisksOnRealSystem(t *testing.T) {
	root := t.TempDir()
	f := newFS(t, root)
	d := f.Disks()
	if len(d) == 0 || d[0].Total == 0 || d[0].Used+d[0].Free > d[0].Total+d[0].Total/10 {
		t.Fatalf("disks %+v", d)
	}
}
