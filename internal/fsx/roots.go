package fsx

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// containerMarkers exist only inside a Docker or Podman container; tests
// replace them.
var containerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

func inContainer() bool {
	for _, p := range containerMarkers {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// HostRoots guards against roots that are not mapped from the host. Inside a
// container, a root on the container's own file system (the nearest mount
// above it is /) would write into the container image. Such a root is
// replaced by the topmost mounts below it, e.g. BFB_ROOTS=/mnt with only
// /mnt/user/images mapped becomes /mnt/user/images; without any it is an
// error. Outside a container, roots that are or lie below a mount, and the
// root / are left alone. It returns the roots to use and one note per
// replaced root.
func HostRoots(roots []string) ([]string, []string, error) {
	if !inContainer() {
		return roots, nil, nil
	}
	mounts := readMounts()
	if mounts == nil {
		return roots, nil, nil
	}
	return hostRoots(roots, mounts)
}

func hostRoots(roots []string, mounts []mount) ([]string, []string, error) {
	var out, notes []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, r := range roots {
		if r == "/" || containingMount(r, mounts) != "/" {
			add(r)
			continue
		}
		below := topMounts(r, mounts)
		if len(below) == 0 {
			return nil, nil, fmt.Errorf("BFB_ROOTS %s is not mapped from the host, files there would only land inside the container: map it as a path, or set BFB_ROOTS to a mapped folder", r)
		}
		for _, p := range below {
			add(p)
		}
		notes = append(notes, fmt.Sprintf("BFB_ROOTS %s is not mapped from the host, using the mapped folders below it instead: %s", r, strings.Join(below, ", ")))
	}
	return out, notes, nil
}

// containingMount is the mount point p lies on, "" when none is known.
func containingMount(p string, mounts []mount) string {
	best := ""
	for _, m := range mounts {
		if within(p, m.point) && len(m.point) > len(best) {
			best = m.point
		}
	}
	return best
}

// topMounts are the mount points below r that are not below another one.
func topMounts(r string, mounts []mount) []string {
	var below []string
	for _, m := range mounts {
		if m.point != r && within(m.point, r) {
			below = append(below, m.point)
		}
	}
	sort.Strings(below)
	var out []string
	for _, p := range below {
		if n := len(out); n > 0 && within(p, out[n-1]) {
			continue // nested in, or the same as, the previous one
		}
		out = append(out, p)
	}
	return out
}
