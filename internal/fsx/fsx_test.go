package fsx

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"bfbackup/internal/model"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func newFS(t *testing.T, roots ...string) *FS {
	t.Helper()
	f, err := New(roots, Options{UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func scanAll(t *testing.T, f *FS, req model.ScanRequest) map[string]model.Entry {
	t.Helper()
	got := map[string]model.Entry{}
	err := f.Scan(context.Background(), req, func(l model.DirListing) error {
		for _, e := range l.Entries {
			got[model.JoinRel(l.Dir, e.Name)] = e
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func keys(m map[string]model.Entry) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestScanExcludesAndIncludes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/1.txt", "one")
	write(t, root, "a/cache/x", "x")
	write(t, root, "b/2.tmp", "two")
	write(t, root, "b/3.txt", "three")
	write(t, root, "c/4.txt", "four")
	write(t, root, ".bfbackup-trash/old/5.txt", "trash")
	write(t, root, "a/.bfb-123.tmp", "partial")
	write(t, root, "c/.BFBackup-456.tmp", "partial")
	write(t, root, "c/.bfb-r789.tmp", "old partial")
	os.Symlink("1.txt", filepath.Join(root, "a/link"))
	f := newFS(t, root)

	got := scanAll(t, f, model.ScanRequest{Root: root, Excludes: []string{"*.tmp", "cache/"}})
	want := []string{"a", "a/1.txt", "a/link", "b", "b/3.txt", "c", "c/4.txt"}
	if strings.Join(keys(got), ",") != strings.Join(want, ",") {
		t.Fatalf("scan = %v, want %v", keys(got), want)
	}
	if got["a/1.txt"].Size != 3 || got["a/1.txt"].Type != model.TypeFile || got["a/1.txt"].Mode != 0o640 {
		t.Fatalf("bad entry %+v", got["a/1.txt"])
	}
	if got["a/link"].Type != model.TypeSymlink || got["a/link"].Link != "1.txt" {
		t.Fatalf("bad link %+v", got["a/link"])
	}

	got = scanAll(t, f, model.ScanRequest{Root: root, Includes: []string{"b", "c", "c/"}, Hash: true})
	if strings.Join(keys(got), ",") != "b/2.tmp,b/3.txt,c/4.txt" {
		t.Fatalf("include scan = %v", keys(got))
	}
	// sha256("four")
	if got["c/4.txt"].Hash != "04efaf080f5a3e74e1c29d1ca6a48569382cbbcd324e8d59d2b83ef21c039f00" {
		t.Fatalf("hash = %s", got["c/4.txt"].Hash)
	}

	err := f.Scan(context.Background(), model.ScanRequest{Root: root, Includes: []string{"missing"}}, func(model.DirListing) error { return nil })
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing include err = %v", err)
	}
}

func TestScanStopsOnEmitError(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		write(t, root, filepath.Join("d", string(rune('a'+i%26)), "f"), "x")
	}
	f := newFS(t, root)
	boom := errors.New("boom")
	err := f.Scan(context.Background(), model.ScanRequest{Root: root, Workers: 4}, func(model.DirListing) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestConfinement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	write(t, outside, "secret", "s")
	os.MkdirAll(root, 0o755)
	os.Symlink(outside, filepath.Join(root, "escape"))
	f := newFS(t, root)

	if _, err := f.Open(filepath.Join(outside, "secret")); !errors.Is(err, model.ErrOutsideRoot) {
		t.Fatalf("outside open err = %v", err)
	}
	if _, err := f.Open(root + "/../outside/secret"); err == nil {
		t.Fatal("dot-dot open succeeded")
	}
	if _, err := f.Open(filepath.Join(root, "escape/secret")); err == nil {
		t.Fatal("open through escaping symlink succeeded")
	}
	if _, err := f.Create(filepath.Join(root, "escape/new"), model.Entry{Size: 0}); err == nil {
		t.Fatal("create through symlinked dir succeeded")
	}
	if _, err := f.ListDir("/etc"); !errors.Is(err, model.ErrOutsideRoot) {
		t.Fatalf("listdir outside err = %v", err)
	}
}

func TestWriterCommitAndAbort(t *testing.T) {
	root := t.TempDir()
	f := newFS(t, root)
	mt := time.Date(2020, 1, 2, 3, 4, 5, 6000, time.UTC)
	e := model.Entry{Size: 5, MTime: mt.UnixNano(), Mode: 0o600}

	w, err := f.Create(filepath.Join(root, "x/y/file"), e)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "hello")
	sum, err := w.Commit("2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
	if err != nil {
		t.Fatal(err)
	}
	if sum == "" {
		t.Fatal("empty sum")
	}
	got, err := f.Stat(filepath.Join(root, "x/y/file"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 5 || got.MTime != mt.UnixNano() || got.Mode != 0o600 {
		t.Fatalf("stat = %+v", got)
	}

	// Wrong hash: nothing changes, temp file removed.
	w, _ = f.Create(filepath.Join(root, "x/y/file"), e)
	io.WriteString(w, "HELLO")
	if _, err := w.Commit(sum); !errors.Is(err, model.ErrHashMismatch) {
		t.Fatalf("err = %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "x/y/file"))
	if string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}
	// Short write.
	w, _ = f.Create(filepath.Join(root, "x/y/other"), e)
	io.WriteString(w, "hi")
	if _, err := w.Commit(""); !errors.Is(err, model.ErrChanged) {
		t.Fatalf("short write err = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "x/y"))
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}

	// A directory in the way is a conflict.
	os.MkdirAll(filepath.Join(root, "dir"), 0o755)
	w, _ = f.Create(filepath.Join(root, "dir"), model.Entry{})
	if _, err := w.Commit(""); !errors.Is(err, model.ErrTypeConflict) {
		t.Fatalf("dir conflict err = %v", err)
	}
}

func TestReaderDetectsChange(t *testing.T) {
	root := t.TempDir()
	write(t, root, "f", "12345")
	f := newFS(t, root)
	r, err := f.Open(filepath.Join(root, "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	os.WriteFile(filepath.Join(root, "f"), []byte("1234567"), 0o644)
	if _, err := io.ReadAll(r); !errors.Is(err, model.ErrChanged) {
		t.Fatalf("err = %v", err)
	}
}

func TestSymlinkCreate(t *testing.T) {
	root := t.TempDir()
	f := newFS(t, root)
	p := filepath.Join(root, "a/l")
	if err := f.Symlink(p, "../target"); err != nil {
		t.Fatal(err)
	}
	if err := f.Symlink(p, "other"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(p); got != "other" {
		t.Fatalf("link = %q", got)
	}
}

func TestTrashAndPurge(t *testing.T) {
	root := t.TempDir()
	write(t, root, "share/dir/a.txt", "a")
	write(t, root, "share/b.txt", "b")
	f := newFS(t, root)
	share := filepath.Join(root, "share")
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if err := f.Trash(share, "dir", "", t1); err != nil {
		t.Fatal(err)
	}
	if err := f.Trash(share, "b.txt", "", t1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, ".bfbackup-trash/20260101T000000Z/dir/a.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, "dir")); !os.IsNotExist(err) {
		t.Fatal("source dir still exists")
	}
	// Same name twice in one batch gets a suffix instead of clobbering.
	write(t, root, "share/b.txt", "b2")
	if err := f.Trash(share, "b.txt", "", t1); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(share, ".bfbackup-trash/20260101T000000Z-1/b.txt")); string(b) != "b2" {
		t.Fatalf("second trash = %q", b)
	}
	if err := f.Trash(share, "missing", "", t1); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing err = %v", err)
	}
	for _, bad := range []string{"", ".bfbackup-trash", "../x"} {
		if err := f.Trash(share, bad, "", t1); err == nil {
			t.Fatalf("trash %q accepted", bad)
		}
	}
	// Trash is never scanned.
	if got := scanAll(t, f, model.ScanRequest{Root: share}); len(got) != 0 {
		t.Fatalf("trash scanned: %v", keys(got))
	}

	n, err := f.PurgeTrash(share, t1.Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if n, _ := f.PurgeTrash(share, t1.Add(time.Hour)); n != 0 {
		t.Fatal("second purge removed something")
	}
}

// TestRemoveRetriesNotEmpty fakes a user share where deleting uncovers more
// files: RemoveAll ends with ENOTEMPTY until the last try.
func TestRemoveRetriesNotEmpty(t *testing.T) {
	root := t.TempDir()
	share := filepath.Join(root, "share")
	f := newFS(t, root)
	oldTries, oldDelay, oldRemove := removeTries, removeRetryDelay, removeAll
	t.Cleanup(func() { removeTries, removeRetryDelay, removeAll = oldTries, oldDelay, oldRemove })
	removeTries, removeRetryDelay = 3, 0
	calls := 0
	failFirst := 0
	removeAll = func(rt *os.Root, name string) error {
		calls++
		if calls <= failFirst {
			return &fs.PathError{Op: "RemoveAll", Path: name, Err: syscall.ENOTEMPTY}
		}
		return oldRemove(rt, name)
	}

	// Gone on the last try: no error.
	write(t, root, "share/dir/a.txt", "a")
	failFirst = 2
	if err := f.Remove(share, "dir"); err != nil || calls != 3 {
		t.Fatalf("remove = %v after %d calls", err, calls)
	}
	if _, err := os.Stat(filepath.Join(share, "dir")); !os.IsNotExist(err) {
		t.Fatal("dir still exists")
	}

	// Never empty: the error names what is left.
	write(t, root, "share/dir/sub/b.txt", "b")
	write(t, root, "share/dir/c.txt", "c")
	os.MkdirAll(filepath.Join(share, "dir/empty"), 0o755)
	calls, failFirst = 0, 99
	err := f.Remove(share, "dir")
	if !errors.Is(err, syscall.ENOTEMPTY) || calls != 3 {
		t.Fatalf("remove = %v after %d calls", err, calls)
	}
	for _, want := range []string{"tried 3 times", "c.txt", "empty/", "sub/b.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}

	// Emptying the trash retries too.
	write(t, root, "share/d.txt", "d")
	now := time.Date(2026, 11, 2, 21, 58, 54, 0, time.UTC)
	if err := f.Trash(share, "d.txt", model.TrashBatchName(now, 45), now); err != nil {
		t.Fatal(err)
	}
	calls, failFirst = 0, 2
	if n, err := f.PurgeTrash(share, now.Add(time.Hour)); err != nil || n != 1 || calls != 3 {
		t.Fatalf("purge = %d, %v after %d calls", n, err, calls)
	}

	// Other errors are not retried.
	calls, failFirst = 0, 0
	removeAll = func(rt *os.Root, name string) error {
		calls++
		return &fs.PathError{Op: "RemoveAll", Path: name, Err: syscall.EACCES}
	}
	if err := f.Remove(share, "dir"); !errors.Is(err, syscall.EACCES) || calls != 1 {
		t.Fatalf("remove = %v after %d calls", err, calls)
	}
}

// failRenames makes renames whose source starts with prefix fail with errno.
func failRenames(t *testing.T, prefix string, errno syscall.Errno) {
	old := rename
	t.Cleanup(func() { rename = old })
	rename = func(rt *os.Root, from, to string) error {
		if strings.HasPrefix(from, prefix) {
			return &os.LinkError{Op: "renameat", Old: from, New: to, Err: errno}
		}
		return old(rt, from, to)
	}
}

// TestTrashCopiesOnRenameRefused fakes a user share whose disk holding the
// file is full (ENOSPC) or another disk (EXDEV): Trash copies the tree and
// removes the original.
func TestTrashCopiesOnRenameRefused(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EXDEV} {
		root := t.TempDir()
		share := filepath.Join(root, "share")
		write(t, root, "share/d/a.txt", "aaa")
		write(t, root, "share/d/sub/b.txt", "bb")
		f := newFS(t, root)
		failRenames(t, "share/", errno)
		if err := f.Trash(share, "d", model.TrashBatchName(time.Unix(1e9, 0), 7), time.Now()); err != nil {
			t.Fatalf("%v: trash = %v", errno, err)
		}
		if _, err := os.Stat(filepath.Join(share, "d")); !os.IsNotExist(err) {
			t.Fatalf("%v: source still exists", errno)
		}
		batches, err := f.TrashInfo(share)
		if err != nil || len(batches) != 1 || batches[0].Files != 2 || batches[0].Bytes != 5 {
			t.Fatalf("%v: trash info = %+v, %v", errno, batches, err)
		}
	}
}

// fakeUnraid makes a temp folder look like Unraid's /mnt with user shares
// on disk1 and disk2.
func fakeUnraid(t *testing.T) string {
	mnt := t.TempDir()
	info := filepath.Join(t.TempDir(), "mountinfo")
	lines := "5 1 0:41 / " + mnt + "/user rw - fuse.shfs shfs rw\n" +
		"6 1 0:44 / " + mnt + "/user0 rw - fuse.shfs shfs rw\n" +
		"7 1 9:1 / " + mnt + "/disk1 rw - xfs /dev/md1p1 rw\n" +
		"8 1 9:2 / " + mnt + "/disk2 rw - xfs /dev/md2p1 rw\n"
	if err := os.WriteFile(info, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	oldMnt, oldInfo := unraidMnt, mountinfoPath
	t.Cleanup(func() { unraidMnt, mountinfoPath = oldMnt, oldInfo })
	unraidMnt, mountinfoPath = mnt, info
	return mnt
}

// TestTrashOnUnraidDisks moves each disk's part of a folder into the trash
// on that disk. Renames through the user share fail as on a full disk; what
// the fake user share still shows is copied.
func TestTrashOnUnraidDisks(t *testing.T) {
	mnt := fakeUnraid(t)
	write(t, mnt, "user/share/d/a.txt", "aaa")
	write(t, mnt, "user/share/d/b.txt", "bb")
	write(t, mnt, "disk1/share/d/a.txt", "aaa")
	write(t, mnt, "disk2/share/d/b.txt", "bb")
	f := newFS(t, mnt)
	failRenames(t, "user/", syscall.ENOSPC)
	batch := model.TrashBatchName(time.Unix(1e9, 0), 7)
	if err := f.Trash(mnt+"/user/share", "d", batch, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"disk1/share/d", "disk2/share/d", "user/share/d"} {
		if _, err := os.Stat(filepath.Join(mnt, p)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists", p)
		}
	}
	for _, p := range []string{"disk1/share/.bfbackup-trash/" + batch + "/d/a.txt", "disk2/share/.bfbackup-trash/" + batch + "/d/b.txt",
		"user/share/.bfbackup-trash/" + batch + "/d/a.txt", "user/share/.bfbackup-trash/" + batch + "/d/b.txt"} {
		if _, err := os.Stat(filepath.Join(mnt, p)); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

// TestListDirHidesUnraidDisks shows /mnt/user but not the single disks.
func TestListDirHidesUnraidDisks(t *testing.T) {
	mnt := fakeUnraid(t)
	for _, d := range []string{"user/share", "user0/share", "disk1/share", "disk2", "disks/usb"} {
		os.MkdirAll(filepath.Join(mnt, d), 0o755)
	}
	f := newFS(t, mnt)
	list, err := f.ListDir(mnt)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "disks,user" {
		t.Fatalf("listed %v", names)
	}
	if list, err := f.ListDir(mnt + "/user"); err != nil || len(list) != 1 {
		t.Fatalf("user = %v, %v", list, err)
	}
}

func TestTrashBatchAndInfo(t *testing.T) {
	root := t.TempDir()
	write(t, root, "share/a.txt", "aaa")
	write(t, root, "share/d/b.txt", "bb")
	f := newFS(t, root)
	share := filepath.Join(root, "share")
	if got, err := f.TrashInfo(share); err != nil || len(got) != 0 {
		t.Fatalf("empty trash = %v, %v", got, err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	batch := model.TrashBatchName(start, 42)
	// A run keeps its batch, however late the file is trashed.
	if err := f.Trash(share, "a.txt", batch, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.Trash(share, "d", batch, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, ".bfbackup-trash/20260101T000000Z-run42/d/b.txt")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../x", "x", "20260101T000000Z/..", "20260101T000000Z-runx"} {
		write(t, root, "share/c.txt", "c")
		if err := f.Trash(share, "c.txt", bad, start); err == nil {
			t.Fatalf("batch %q accepted", bad)
		}
	}
	got, err := f.TrashInfo(share)
	if err != nil || len(got) != 1 {
		t.Fatalf("info = %v, %v", got, err)
	}
	if b := got[0]; b.Name != batch || b.RunID != 42 || b.Files != 2 || b.Bytes != 5 || b.Time != start.Unix() {
		t.Fatalf("batch = %+v", b)
	}
	if n, err := f.PurgeTrash(share, start.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}
}

// TestMoveTree copies where renames are refused and merges into a dst that
// already holds part of the tree.
func TestMoveTree(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a/b.txt", "b")
	write(t, root, "dst/a/old.txt", "o")
	os.Symlink("a/b.txt", filepath.Join(root, "src/l"))
	rt, _ := os.OpenRoot(root)
	defer rt.Close()
	failRenames(t, "src", syscall.EXDEV)
	if err := moveTree(rt, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "dst/a/b.txt")); string(b) != "b" {
		t.Fatal("copy content")
	}
	if l, _ := os.Readlink(filepath.Join(root, "dst/l")); l != "a/b.txt" {
		t.Fatal("copy link")
	}
	if _, err := os.Stat(filepath.Join(root, "dst/a/old.txt")); err != nil {
		t.Fatal("merge lost old.txt")
	}
	if _, err := os.Stat(filepath.Join(root, "src")); !os.IsNotExist(err) {
		t.Fatal("src still exists")
	}
}

func TestStatFSAndListDir(t *testing.T) {
	root := t.TempDir()
	write(t, root, "z.txt", "z")
	write(t, root, "adir/x", "x")
	f := newFS(t, root)
	u, err := f.StatFS(root)
	if err != nil || u.Total == 0 {
		t.Fatalf("statfs %+v %v", u, err)
	}
	l, err := f.ListDir(root)
	if err != nil || len(l) != 2 || l[0].Name != "adir" {
		t.Fatalf("listdir %+v %v", l, err)
	}
}

// Folders deleted outside BFBackup must not stay "known" in the folder
// cache: Mkdir creates them again, Trash recreates its batch folder.
func TestStaleDirCache(t *testing.T) {
	root := t.TempDir()
	write(t, root, "share/a.txt", "a")
	write(t, root, "share/b.txt", "b")
	f := newFS(t, root)
	share := filepath.Join(root, "share")
	dir := filepath.Join(share, "x/y")
	if err := f.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(share, "x"))
	if err := f.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("folder not created again: %v", err)
	}
	now := time.Now()
	batch := model.TrashBatchName(now, 3)
	if err := f.Trash(share, "a.txt", batch, now); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(share, model.TrashDirName))
	if err := f.Trash(share, "b.txt", batch, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, model.TrashDirName, batch, "b.txt")); err != nil {
		t.Fatal(err)
	}
}
