// Package fsx is the only place that touches the file system for backup
// data. Every operation goes through an os.Root opened on one of the
// configured roots, so paths can never escape them (no "..", no symlink
// tricks).
package fsx

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"bfbackup/internal/model"
)

// FS gives confined access to a set of root directories.
type FS struct {
	roots []fsRoot
	uid   int // -1: leave ownership alone
	gid   int

	dirCache sync.Map // "<root>\x00<rel dir>" -> struct{}, dirs known to exist
}

type fsRoot struct {
	path string
	r    *os.Root
}

// Options configure an FS.
type Options struct {
	// UID and GID are applied to created files and dirs when >= 0.
	UID, GID int
}

// New opens the given absolute root directories.
func New(paths []string, opt Options) (*FS, error) {
	if len(paths) == 0 {
		return nil, errors.New("fsx: at least one root is required")
	}
	f := &FS{uid: opt.UID, gid: opt.GID}
	for _, p := range paths {
		c, err := model.CleanAbs(p)
		if err != nil {
			return nil, fmt.Errorf("fsx: root %q: %w", p, err)
		}
		r, err := os.OpenRoot(c)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("fsx: open root %q: %w", c, err)
		}
		f.roots = append(f.roots, fsRoot{path: c, r: r})
	}
	// Longest first, so nested roots win.
	sort.Slice(f.roots, func(i, j int) bool { return len(f.roots[i].path) > len(f.roots[j].path) })
	return f, nil
}

// Close releases all roots.
func (f *FS) Close() error {
	for _, r := range f.roots {
		r.r.Close()
	}
	return nil
}

// Roots returns the configured root paths.
func (f *FS) Roots() []string {
	out := make([]string, len(f.roots))
	for i, r := range f.roots {
		out[i] = r.path
	}
	sort.Strings(out)
	return out
}

// resolve maps an absolute path to its root and the path relative to it
// ("." for the root itself).
func (f *FS) resolve(abs string) (*os.Root, string, error) {
	c, err := model.CleanAbs(abs)
	if err != nil {
		return nil, "", err
	}
	for _, rt := range f.roots {
		if c == rt.path {
			return rt.r, ".", nil
		}
		prefix := rt.path + "/"
		if rt.path == "/" {
			prefix = "/"
		}
		if strings.HasPrefix(c, prefix) {
			return rt.r, c[len(prefix):], nil
		}
	}
	return nil, "", fmt.Errorf("%s: %w", c, model.ErrOutsideRoot)
}

func joinRel(base, rel string) string {
	if base == "." || base == "" {
		if rel == "" {
			return "."
		}
		return rel
	}
	if rel == "" {
		return base
	}
	return base + "/" + rel
}

// entryFromInfo converts an os.FileInfo. ok is false for special files.
func entryFromInfo(name string, fi fs.FileInfo) (model.Entry, bool) {
	e := model.Entry{Name: name, MTime: fi.ModTime().UnixNano(), Mode: uint32(fi.Mode().Perm())}
	switch {
	case fi.Mode().IsRegular():
		e.Type = model.TypeFile
		e.Size = fi.Size()
	case fi.Mode()&fs.ModeSymlink != 0:
		e.Type = model.TypeSymlink
	case fi.IsDir():
		e.Type = model.TypeDir
	default:
		return e, false
	}
	return e, true
}

// readDir lists a directory with one fstatat per entry (no path walking per
// file, which matters on FUSE file systems such as Unraid's /mnt/user).
// Devices, sockets and FIFOs are left out.
func readDir(rt *os.Root, rel string) ([]model.Entry, error) {
	out, _, err := readDirSpecial(rt, rel)
	return out, err
}

// readDirSpecial is readDir that also returns the names of the devices,
// sockets and FIFOs it left out.
func readDirSpecial(rt *os.Root, rel string) ([]model.Entry, []string, error) {
	d, err := rt.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, nil, err
	}
	sc, err := d.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	out := make([]model.Entry, 0, len(names))
	var special []string
	var opErr error
	cerr := sc.Control(func(fd uintptr) {
		buf := make([]byte, 4096)
		for _, name := range names {
			var st unix.Stat_t
			if err := unix.Fstatat(int(fd), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if errors.Is(err, syscall.ENOENT) {
					continue // removed while listing
				}
				opErr = fmt.Errorf("stat %s/%s: %w", rel, name, err)
				return
			}
			e := model.Entry{
				Name:  name,
				MTime: int64(st.Mtim.Sec)*1e9 + int64(st.Mtim.Nsec), // int32 fields on 32-bit ARM
				Mode:  uint32(st.Mode) & 0o7777,                     // uint16 on darwin

			}
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFREG:
				e.Type = model.TypeFile
				e.Size = st.Size
			case unix.S_IFDIR:
				e.Type = model.TypeDir
			case unix.S_IFLNK:
				e.Type = model.TypeSymlink
				n, err := unix.Readlinkat(int(fd), name, buf)
				if err != nil {
					if errors.Is(err, syscall.ENOENT) {
						continue
					}
					opErr = fmt.Errorf("readlink %s/%s: %w", rel, name, err)
					return
				}
				e.Link = string(buf[:n])
			default:
				special = append(special, name) // devices, sockets, fifos are not backed up
				continue
			}
			out = append(out, e)
		}
	})
	if cerr != nil {
		return nil, nil, cerr
	}
	if opErr != nil {
		return nil, nil, opErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Strings(special)
	return out, special, nil
}

// ListDir lists an absolute directory, directories first. Used by the UI's
// folder picker. Internal files and Unraid's single disks are hidden.
func (f *FS) ListDir(abs string) ([]model.Entry, error) {
	rt, rel, err := f.resolve(abs)
	if err != nil {
		return nil, err
	}
	all, err := readDir(rt, rel)
	if err != nil {
		return nil, err
	}
	var disks []mount
	if c, _ := model.CleanAbs(abs); c == unraidMnt {
		disks = unraidDisks(readMounts())
	}
	out := all[:0]
	for _, e := range all {
		if model.IsInternalName(e.Name) || onUnraidDisk(unraidMnt+"/"+e.Name, disks) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := out[i].Type == model.TypeDir, out[j].Type == model.TypeDir
		return di && !dj
	})
	return out, nil
}

// Stat returns the entry at abs without following symlinks.
func (f *FS) Stat(abs string) (model.Entry, error) {
	rt, rel, err := f.resolve(abs)
	if err != nil {
		return model.Entry{}, err
	}
	fi, err := rt.Lstat(rel)
	if err != nil {
		return model.Entry{}, err
	}
	e, ok := entryFromInfo(path.Base(abs), fi)
	if !ok {
		return e, fmt.Errorf("%s: unsupported file type: %w", abs, model.ErrTypeConflict)
	}
	if e.Type == model.TypeSymlink {
		if e.Link, err = rt.Readlink(rel); err != nil {
			return e, err
		}
	}
	return e, nil
}

// StatFS reports the capacity of the file system holding abs.
func (f *FS) StatFS(abs string) (model.FSUsage, error) {
	if _, _, err := f.resolve(abs); err != nil {
		return model.FSUsage{}, err
	}
	c, _ := model.CleanAbs(abs)
	var st unix.Statfs_t
	for {
		err := unix.Statfs(c, &st)
		if err == nil {
			break
		}
		// A target dir that does not exist yet: ask its nearest existing parent.
		if !errors.Is(err, syscall.ENOENT) || c == "/" {
			return model.FSUsage{}, fmt.Errorf("statfs %s: %w", c, err)
		}
		c = path.Dir(c)
	}
	return model.FSUsage{
		Total: st.Blocks * blockSize(&st),
		Free:  st.Bavail * blockSize(&st),
	}, nil
}

// Reader streams a file and hashes it. Read returns model.ErrChanged
// instead of io.EOF when the file changed while it was read.
type Reader struct {
	f     *os.File
	entry model.Entry
	h     hashWriter
	n     int64
	hash  string
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

// Open opens a regular file for reading.
func (f *FS) Open(abs string) (*Reader, error) {
	rt, rel, err := f.resolve(abs)
	if err != nil {
		return nil, err
	}
	if fi, err := rt.Lstat(rel); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file: %w", abs, model.ErrTypeConflict)
	}
	fh, err := rt.Open(rel)
	if err != nil {
		return nil, err
	}
	fi, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		fh.Close()
		return nil, fmt.Errorf("%s: not a regular file: %w", abs, model.ErrTypeConflict)
	}
	e, _ := entryFromInfo(path.Base(abs), fi)
	return &Reader{f: fh, entry: e, h: sha256.New()}, nil
}

// Entry returns the metadata taken when the file was opened.
func (r *Reader) Entry() model.Entry { return r.entry }

// Hash returns the hex SHA-256 once Read has returned io.EOF.
func (r *Reader) Hash() string { return r.hash }

func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	if n > 0 {
		r.h.Write(p[:n])
		r.n += int64(n)
	}
	if err == io.EOF {
		fi, serr := r.f.Stat()
		if serr != nil {
			return n, serr
		}
		if r.n != r.entry.Size || fi.Size() != r.entry.Size || fi.ModTime().UnixNano() != r.entry.MTime {
			return n, fmt.Errorf("%s: %w", r.f.Name(), model.ErrChanged)
		}
		r.hash = hex.EncodeToString(r.h.Sum(nil))
	}
	return n, err
}

// Close closes the file.
func (r *Reader) Close() error { return r.f.Close() }

// HashFile returns the SHA-256 and size of a regular file.
func (f *FS) HashFile(abs string) (string, int64, error) {
	r, err := f.Open(abs)
	if err != nil {
		return "", 0, err
	}
	defer r.Close()
	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(io.Discard, struct{ io.Reader }{r}, buf); err != nil {
		return "", 0, err
	}
	return r.Hash(), r.n, nil
}

// Writer writes a file atomically: data goes to a temp file next to the
// target, which is renamed into place by Commit.
type Writer struct {
	fs    *FS
	rt    *os.Root
	rel   string
	tmp   string
	f     *os.File
	h     hashWriter
	n     int64
	entry model.Entry
	done  bool
	// keep: Abort leaves the temp file for a later resume (CreateResumable).
	keep bool
}

// Create starts writing a regular file at abs with the metadata in e.
func (f *FS) Create(abs string, e model.Entry) (*Writer, error) {
	rt, rel, err := f.resolve(abs)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return nil, fmt.Errorf("%s: cannot write a root: %w", abs, model.ErrInvalidPath)
	}
	dir := path.Dir(rel)
	tmp := path.Join(dir, tempName())
	fh, err := f.createTemp(rt, dir, tmp)
	if err != nil {
		return nil, err
	}
	return &Writer{fs: f, rt: rt, rel: rel, tmp: tmp, f: fh, h: sha256.New(), entry: e}, nil
}

func (f *FS) createTemp(rt *os.Root, dir, tmp string) (*os.File, error) {
	for attempt := 0; ; attempt++ {
		if err := f.mkdirAll(rt, dir); err != nil {
			return nil, err
		}
		fh, err := rt.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return fh, nil
		}
		if errors.Is(err, fs.ErrNotExist) && attempt == 0 {
			f.forgetDirs(rt) // a cached dir was removed behind our back
			continue
		}
		return nil, err
	}
}

func tempName() string {
	var b [8]byte
	rand.Read(b[:])
	return model.TempPrefix + hex.EncodeToString(b[:]) + model.TempSuffix
}

func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.h.Write(p[:n])
	w.n += int64(n)
	return n, err
}

// Commit finishes the file. When expect is not empty, the written data must
// hash to it. It returns the hex SHA-256 of the written data.
func (w *Writer) Commit(expect string) (string, error) {
	if w.done {
		return "", errors.New("fsx: writer already finished")
	}
	sum := hex.EncodeToString(w.h.Sum(nil))
	fail := func(err error) (string, error) {
		w.discard()
		return "", err
	}
	if expect != "" && sum != expect {
		return fail(fmt.Errorf("%s: %w", w.rel, model.ErrHashMismatch))
	}
	if w.n != w.entry.Size {
		return fail(fmt.Errorf("%s: wrote %d of %d bytes: %w", w.rel, w.n, w.entry.Size, model.ErrChanged))
	}
	if err := w.f.Close(); err != nil {
		return fail(err)
	}
	mode := fs.FileMode(w.entry.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	if err := w.rt.Chmod(w.tmp, mode); err != nil {
		return fail(err)
	}
	if err := w.fs.chown(w.rt, w.tmp); err != nil {
		return fail(err)
	}
	mt := time.Unix(0, w.entry.MTime)
	if err := w.rt.Chtimes(w.tmp, mt, mt); err != nil {
		return fail(err)
	}
	if fi, err := w.rt.Lstat(w.rel); err == nil && fi.IsDir() {
		return fail(fmt.Errorf("%s: %w", w.rel, model.ErrTypeConflict))
	}
	if err := w.rt.Rename(w.tmp, w.rel); err != nil {
		return fail(err)
	}
	w.done = true
	return sum, nil
}

// Abort discards the temp file, or keeps it for a resume when the writer
// came from CreateResumable. Safe to call more than once.
func (w *Writer) Abort() error {
	if w.keep && !w.done {
		w.done = true
		return w.f.Close()
	}
	return w.discard()
}

// discard removes the temp file whatever kind of writer this is.
func (w *Writer) discard() error {
	if w.done {
		return nil
	}
	w.done = true
	w.f.Close()
	return w.rt.Remove(w.tmp)
}

// Mkdir creates the folder abs and its missing parents, owned like the
// files this node writes. An existing folder is not an error.
func (f *FS) Mkdir(abs string) error {
	rt, rel, err := f.resolve(abs)
	if err != nil {
		return err
	}
	// The user asks for this folder, so look at the disk: a cached folder
	// may have been deleted outside BFBackup since.
	f.forgetDirs(rt)
	return f.mkdirAll(rt, rel)
}

// Symlink creates (or replaces) a symlink at abs pointing to target.
func (f *FS) Symlink(abs, target string) error {
	rt, rel, err := f.resolve(abs)
	if err != nil {
		return err
	}
	if rel == "." {
		return model.ErrInvalidPath
	}
	dir := path.Dir(rel)
	if err := f.mkdirAll(rt, dir); err != nil {
		return err
	}
	tmp := path.Join(dir, tempName())
	if err := rt.Symlink(target, tmp); err != nil {
		return err
	}
	if fi, err := rt.Lstat(rel); err == nil && fi.IsDir() {
		rt.Remove(tmp)
		return fmt.Errorf("%s: %w", rel, model.ErrTypeConflict)
	}
	if f.uid >= 0 {
		rt.Lchown(tmp, f.uid, f.gid)
	}
	if err := rt.Rename(tmp, rel); err != nil {
		rt.Remove(tmp)
		return err
	}
	return nil
}

func (f *FS) chown(rt *os.Root, rel string) error {
	if f.uid < 0 {
		return nil
	}
	return rt.Lchown(rel, f.uid, f.gid)
}

func (f *FS) cacheKey(rt *os.Root, dir string) string {
	return rt.Name() + "\x00" + dir
}

func (f *FS) forgetDirs(rt *os.Root) {
	prefix := rt.Name() + "\x00"
	f.dirCache.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			f.dirCache.Delete(k)
		}
		return true
	})
}

// mkdirAll creates dir and its parents inside rt. A symlink or file in the
// way is a type conflict: we never write through symlinked directories.
func (f *FS) mkdirAll(rt *os.Root, dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	if _, ok := f.dirCache.Load(f.cacheKey(rt, dir)); ok {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		cur := strings.Join(parts[:i+1], "/")
		if _, ok := f.dirCache.Load(f.cacheKey(rt, cur)); ok {
			continue
		}
		fi, err := rt.Lstat(cur)
		switch {
		case err == nil && fi.IsDir():
		case err == nil:
			return fmt.Errorf("%s: %w", cur, model.ErrTypeConflict)
		case errors.Is(err, fs.ErrNotExist):
			if err := rt.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if err := f.chown(rt, cur); err != nil {
				return err
			}
		default:
			return err
		}
		f.dirCache.Store(f.cacheKey(rt, cur), struct{}{})
	}
	return nil
}

func newHash() hashWriter { return sha256.New() }

// drain reads r to the end.
func drain(r io.Reader) error {
	buf := make([]byte, 1<<20)
	_, err := io.CopyBuffer(io.Discard, struct{ io.Reader }{r}, buf)
	return err
}
