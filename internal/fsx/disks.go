package fsx

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"bfbackup/internal/model"
)

type mount struct {
	point  string
	fstype string
}

// pseudoFS are file systems that never hold user data.
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devpts": true, "devtmpfs": true, "tmpfs": true, "cgroup": true, "cgroup2": true,
	"mqueue": true, "securityfs": true, "debugfs": true, "tracefs": true, "fusectl": true, "configfs": true,
	"pstore": true, "bpf": true, "autofs": true, "binfmt_misc": true, "hugetlbfs": true, "nsfs": true, "ramfs": true,
}

// parseMountinfo reads /proc/self/mountinfo lines:
// id parent major:minor root mountpoint options [optional...] - fstype source superopts
func parseMountinfo(data string) []mount {
	var out []mount
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(f) {
			continue
		}
		out = append(out, mount{point: unescapeMount(f[4]), fstype: f[sep+1]})
	}
	return out
}

// unescapeMount decodes the octal escapes the kernel uses in mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

func within(p, root string) bool {
	return p == root || root == "/" || strings.HasPrefix(p, root+"/")
}

// diskPaths picks the paths whose capacity describes the storage below the
// roots: the real mounts below each root, or the root itself when nothing is
// mounted below it. Unraid's single disks are left out (/mnt/user already
// counts them), and so is /mnt itself.
func diskPaths(roots []string, mounts []mount) []string {
	hidden := unraidDisks(mounts)
	seen := map[string]bool{}
	var out []string
	for _, r := range roots {
		var found []string
		for _, m := range mounts {
			if within(m.point, r) && !pseudoFS[m.fstype] && m.point != "/" &&
				!onUnraidDisk(m.point, hidden) && !isUnraidMnt(m.point, mounts) {
				found = append(found, m.point)
			}
		}
		if len(found) == 0 {
			found = []string{r}
		}
		for _, p := range found {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Disks reports capacity for the file systems below the allowed roots.
// File systems mounted at several paths are reported once.
func (f *FS) Disks() []model.DiskInfo {
	mounts := readMounts()
	types := map[string]string{}
	for _, m := range mounts {
		types[m.point] = m.fstype
	}
	seen := map[[2]int32]bool{}
	out := []model.DiskInfo{}
	for _, p := range diskPaths(f.Roots(), mounts) {
		var st unix.Statfs_t
		if err := unix.Statfs(p, &st); err != nil || st.Blocks == 0 {
			continue
		}
		if seen[st.Fsid.Val] {
			continue
		}
		seen[st.Fsid.Val] = true
		bs := blockSize(&st)
		out = append(out, model.DiskInfo{
			Path:   p,
			FSType: types[p],
			Total:  st.Blocks * bs,
			Used:   (st.Blocks - st.Bfree) * bs,
			Free:   st.Bavail * bs,
		})
	}
	return out
}

// IsMountPoint reports whether p is a mount point: it is on another file
// system than its parent folder. A USB drive's folder below /mnt/disks that
// stays behind after the drive is unmounted is not one.
func IsMountPoint(p string) bool {
	st, err := os.Lstat(p)
	if err != nil || !st.IsDir() {
		return false
	}
	parent, err := os.Lstat(filepath.Dir(p))
	if err != nil {
		return false
	}
	a, ok1 := st.Sys().(*syscall.Stat_t)
	b, ok2 := parent.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && uint64(a.Dev) != uint64(b.Dev)
}
