package fsx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	"bfbackup/internal/model"
)

// Trash moves rootAbs/rel into rootAbs/.bfbackup-trash/<batch>/rel. An
// empty batch is named after now (older controllers send none).
// It returns fs.ErrNotExist when rel does not exist.
func (f *FS) Trash(rootAbs, rel, batch string, now time.Time) error {
	rt, base, err := f.resolve(rootAbs)
	if err != nil {
		return err
	}
	rel, err = model.CleanRel(rel)
	if err != nil {
		return err
	}
	if rel == "" || rel == model.TrashDirName || strings.HasPrefix(rel, model.TrashDirName+"/") {
		return fmt.Errorf("refusing to trash %q: %w", rel, model.ErrInvalidPath)
	}
	if batch == "" {
		batch = model.TrashBatchName(now, 0)
	} else if _, _, ok := model.ParseTrashBatch(batch); !ok {
		return fmt.Errorf("bad trash batch name %q: %w", batch, model.ErrInvalidPath)
	}
	src := joinRel(base, rel)
	if _, err := rt.Lstat(src); err != nil {
		return err
	}
	stamp := batch
	dstRel := path.Join(model.TrashDirName, stamp, rel)
	for i := 1; ; i++ {
		if _, err := rt.Lstat(joinRel(base, dstRel)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dstRel = path.Join(model.TrashDirName, fmt.Sprintf("%s-%d", stamp, i), rel)
	}
	dst := joinRel(base, dstRel)
	rootC, _ := model.CleanAbs(rootAbs)
	if f.trashOnDisks(path.Join(rootC, rel), path.Join(rootC, dstRel)) {
		if _, err := rt.Lstat(src); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		// Parts on disks this node cannot see go the usual way below.
	}
	err = f.renameInto(rt, src, dst)
	if errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) {
		// Unraid user shares refuse renames across disks (EXDEV), and keep a
		// renamed file on its disk, so a full disk refuses the trash folders
		// (ENOSPC) although the share has room. A folder partly moved by
		// trashOnDisks already exists in the trash (EEXIST, ENOTEMPTY).
		return moveTree(rt, src, dst)
	}
	return err
}

// renameInto renames src to dst inside rt, creating dst's parent folders.
func (f *FS) renameInto(rt *os.Root, src, dst string) error {
	if err := f.mkdirAll(rt, path.Dir(dst)); err != nil {
		return err
	}
	err := rename(rt, src, dst)
	if errors.Is(err, fs.ErrNotExist) {
		// The batch folder was cached as existing but has been removed
		// outside BFBackup (e.g. the trash emptied by hand during a run).
		f.forgetDirs(rt)
		if err := f.mkdirAll(rt, path.Dir(dst)); err != nil {
			return err
		}
		err = rename(rt, src, dst)
	}
	return err
}

// trashOnDisks moves srcAbs to dstAbs, both below Unraid's /mnt/user, on
// each single disk (/mnt/disk1, /mnt/cache, …) that holds a part of it. The
// rename stays on that disk, so nothing is copied and neither a full disk
// nor the share's minimum free space gets in the way. It reports whether it
// moved anything; disks it cannot see or rename on are left to the caller.
func (f *FS) trashOnDisks(srcAbs, dstAbs string) bool {
	srcShare, ok1 := strings.CutPrefix(srcAbs, unraidMnt+"/user/")
	dstShare, ok2 := strings.CutPrefix(dstAbs, unraidMnt+"/user/")
	if !ok1 || !ok2 {
		return false
	}
	moved := false
	for _, d := range trashDisks() {
		if d.fstype == "fuse.shfs" {
			continue // /mnt/user0 is a user share view too
		}
		rt, src, err := f.resolve(d.point + "/" + srcShare)
		if err != nil {
			continue // disk outside the roots
		}
		rt2, dst, err := f.resolve(d.point + "/" + dstShare)
		if err != nil || rt2 != rt {
			continue
		}
		if _, err := rt.Lstat(src); err != nil {
			continue
		}
		if f.renameInto(rt, src, dst) == nil {
			moved = true
		}
	}
	return moved
}

// moveTree moves src to dst inside rt where a plain rename is refused: a
// folder is merged into an existing dst entry by entry, a file that cannot
// be renamed is copied and then removed.
func moveTree(rt *os.Root, src, dst string) error {
	err := rename(rt, src, dst)
	if err == nil || !(errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.ENOSPC) ||
		errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY)) {
		return err
	}
	fi, lerr := rt.Lstat(src)
	if lerr != nil {
		return lerr
	}
	if !fi.IsDir() {
		if errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
		if err := copyEntry(rt, src, dst, fi); err != nil {
			rt.Remove(dst)
			return err
		}
		return rt.Remove(src)
	}
	if err := rt.Mkdir(dst, fi.Mode().Perm()|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	entries, err := readDir(rt, src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := moveTree(rt, path.Join(src, e.Name), path.Join(dst, e.Name)); err != nil {
			return err
		}
	}
	return removeTree(rt, src)
}

// removeAll is (*os.Root).RemoveAll and rename is (*os.Root).Rename; tests
// replace them.
var (
	removeAll = (*os.Root).RemoveAll
	rename    = (*os.Root).Rename
)

// removeTries and removeRetryDelay bound how often removeTree tries again.
var (
	removeTries      = 5
	removeRetryDelay = 500 * time.Millisecond
)

// removeTree is RemoveAll that tries again when the folder was not empty at
// the end. On Unraid user shares, deleting a file can uncover a copy of it on
// another disk, and the mover or another writer can add files meanwhile. If
// it still fails, the error names some of the files left behind.
func removeTree(rt *os.Root, name string) error {
	var err error
	for i := 0; i < removeTries; i++ {
		if i > 0 {
			time.Sleep(removeRetryDelay)
		}
		if err = removeAll(rt, name); err == nil || !errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
	}
	if left := leftovers(rt, name, 5); len(left) > 0 {
		return fmt.Errorf("%w (tried %d times; still there: %s)", err, removeTries, strings.Join(left, ", "))
	}
	return fmt.Errorf("%w (tried %d times)", err, removeTries)
}

// leftovers lists up to max files below dir, relative to it; folders count
// only when they are empty.
func leftovers(rt *os.Root, dir string, max int) []string {
	var out []string
	var walk func(rel string)
	walk = func(rel string) {
		entries, err := readDir(rt, joinRel(dir, rel))
		if err != nil {
			return
		}
		if len(entries) == 0 && rel != "" {
			out = append(out, rel+"/")
		}
		for _, e := range entries {
			if len(out) >= max {
				return
			}
			p := model.JoinRel(rel, e.Name)
			if e.Type == model.TypeDir {
				walk(p)
			} else {
				out = append(out, p)
			}
		}
	}
	walk("")
	return out
}

// Remove deletes rootAbs/rel for good, without the trash. The same paths as
// for Trash are refused. It returns fs.ErrNotExist when rel does not exist.
func (f *FS) Remove(rootAbs, rel string) error {
	rt, base, err := f.resolve(rootAbs)
	if err != nil {
		return err
	}
	rel, err = model.CleanRel(rel)
	if err != nil {
		return err
	}
	if rel == "" || rel == model.TrashDirName || strings.HasPrefix(rel, model.TrashDirName+"/") {
		return fmt.Errorf("refusing to delete %q: %w", rel, model.ErrInvalidPath)
	}
	src := joinRel(base, rel)
	if _, err := rt.Lstat(src); err != nil {
		return err
	}
	err = removeTree(rt, src)
	f.forgetDirs(rt)
	return err
}

// PurgeTrash removes trash batches older than olderThan below rootAbs and
// returns how many batches were removed.
func (f *FS) PurgeTrash(rootAbs string, olderThan time.Time) (int, error) {
	rt, base, err := f.resolve(rootAbs)
	if err != nil {
		return 0, err
	}
	dir := joinRel(base, model.TrashDirName)
	entries, err := readDir(rt, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		ts, _, ok := model.ParseTrashBatch(e.Name)
		if !ok || e.Type != model.TypeDir {
			continue // not ours
		}
		if ts.Before(olderThan) {
			if err := removeTree(rt, path.Join(dir, e.Name)); err != nil {
				return n, err
			}
			n++
		}
	}
	f.forgetDirs(rt)
	return n, nil
}

// TrashInfo lists the trash batches below rootAbs with their file count and
// size, oldest first. No trash folder is an empty list.
func (f *FS) TrashInfo(rootAbs string) ([]model.TrashBatch, error) {
	rt, base, err := f.resolve(rootAbs)
	if err != nil {
		return nil, err
	}
	dir := joinRel(base, model.TrashDirName)
	entries, err := readDir(rt, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []model.TrashBatch{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []model.TrashBatch{}
	for _, e := range entries {
		ts, runID, ok := model.ParseTrashBatch(e.Name)
		if !ok || e.Type != model.TypeDir {
			continue // not ours
		}
		b := model.TrashBatch{Name: e.Name, Time: ts.Unix(), RunID: runID}
		if err := countTree(rt, path.Join(dir, e.Name), &b); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// countTree adds the files and bytes below dir to b.
func countTree(rt *os.Root, dir string, b *model.TrashBatch) error {
	entries, err := readDir(rt, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch e.Type {
		case model.TypeDir:
			if err := countTree(rt, path.Join(dir, e.Name), b); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		default:
			b.Files++
			b.Bytes += e.Size
		}
	}
	return nil
}

// copyEntry copies the file or symlink src (described by fi) to dst inside
// one root.
func copyEntry(rt *os.Root, src, dst string, fi fs.FileInfo) error {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := rt.Readlink(src)
		if err != nil {
			return err
		}
		return rt.Symlink(target, dst)
	case fi.Mode().IsRegular():
		in, err := rt.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := rt.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		return rt.Chtimes(dst, fi.ModTime(), fi.ModTime())
	}
	return nil // special files are dropped
}
