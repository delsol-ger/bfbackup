package fsx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const containerRoot = "1 0 0:50 / / rw - btrfs /dev/loop2 rw\n2 1 0:52 / /proc rw - proc proc rw\n"

func TestHostRoots(t *testing.T) {
	cases := []struct {
		name, mountinfo, roots, want string
		notes                        int
		err                          bool
	}{
		{"whole /mnt mapped", "3 1 0:2 /mnt /mnt rw - rootfs rootfs rw\n4 3 0:41 / /mnt/user rw - fuse.shfs shfs rw\n", "/mnt", "/mnt", 0, false},
		{"root below a mount", "3 1 0:41 / /mnt/user rw - fuse.shfs shfs rw\n", "/mnt/user/Backups", "/mnt/user/Backups", 0, false},
		{"one share mapped", "3 1 0:41 /images /mnt/user/images rw - fuse.shfs shfs rw\n", "/mnt", "/mnt/user/images", 1, false},
		{"topmost mounts only", "3 1 0:41 / /mnt/user rw - fuse.shfs shfs rw\n4 1 0:60 / /mnt/disks rw - tmpfs tmpfs rw\n5 4 8:33 / /mnt/disks/USB rw - exfat /dev/sdc1 rw\n",
			"/mnt", "/mnt/disks,/mnt/user", 1, false},
		{"duplicates dropped", "3 1 0:41 /images /mnt/user/images rw - fuse.shfs shfs rw\n", "/mnt,/mnt/user/images", "/mnt/user/images", 1, false},
		{"root / left alone", "", "/", "/", 0, false},
		{"nothing mapped", "3 1 0:41 / /data rw - xfs /dev/sdb1 rw\n", "/mnt", "", 0, true},
	}
	for _, c := range cases {
		got, notes, err := hostRoots(strings.Split(c.roots, ","), parseMountinfo(containerRoot+c.mountinfo))
		if c.err {
			if err == nil || !strings.Contains(err.Error(), "not mapped from the host") {
				t.Errorf("%s: err %v, want not mapped", c.name, err)
			}
			continue
		}
		if err != nil || strings.Join(got, ",") != c.want || len(notes) != c.notes {
			t.Errorf("%s: got %v, notes %q, err %v; want %s with %d notes", c.name, got, notes, err, c.want, c.notes)
		}
	}
}

// TestHostRootsOutsideContainer leaves the roots alone without a container
// marker, even on the root file system.
func TestHostRootsOutsideContainer(t *testing.T) {
	dir := t.TempDir()
	info := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(info, []byte(containerRoot), 0o644); err != nil {
		t.Fatal(err)
	}
	oldMarkers, oldInfo := containerMarkers, mountinfoPath
	t.Cleanup(func() { containerMarkers, mountinfoPath = oldMarkers, oldInfo })
	containerMarkers, mountinfoPath = []string{filepath.Join(dir, ".dockerenv")}, info

	if got, _, err := HostRoots([]string{"/mnt"}); err != nil || len(got) != 1 || got[0] != "/mnt" {
		t.Fatalf("outside: %v, %v", got, err)
	}
	if err := os.WriteFile(containerMarkers[0], nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := HostRoots([]string{"/mnt"}); err == nil {
		t.Fatal("inside a container: want an error for an unmapped root")
	}
}
