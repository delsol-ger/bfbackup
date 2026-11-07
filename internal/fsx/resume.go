package fsx

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"bfbackup/internal/model"
)

// Partial files of interrupted transfers are kept next to the target as
// .BFBackup-r<path key>-<version key>.tmp. The path key finds older partials
// of the same file, the version key (size, mtime) makes sure a partial is only
// continued with the same source file. Every file name of this form is an
// internal name, so scans and listings never show it.
const resumePrefix = model.TempPrefix + "r"

// legacyResumePrefix names partials of older versions (.bfb-r…). They are
// never continued, only cleaned up like partials of another version.
const legacyResumePrefix = model.LegacyTempPrefix + "r"

// stalePartial is how long a partial of another file may sit in a directory
// before a resume in that directory removes it.
const stalePartial = 7 * 24 * time.Hour

func resumeKeys(name string, e model.Entry) (pathKey, versionKey string) {
	p := sha256.Sum256([]byte(name))
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(e.Size))
	binary.BigEndian.PutUint64(b[8:], uint64(e.MTime))
	v := sha256.Sum256(b[:])
	return hex.EncodeToString(p[:8]), hex.EncodeToString(v[:4])
}

// CreateResumable is Create, but the data of an earlier, aborted attempt for
// the same path and entry (size, mtime) is kept. have is the number of bytes
// already written; the caller sends only the rest. Abort keeps the data for
// the next attempt, a failed Commit removes it.
func (f *FS) CreateResumable(abs string, e model.Entry) (w *Writer, have int64, err error) {
	rt, rel, err := f.resolve(abs)
	if err != nil {
		return nil, 0, err
	}
	if rel == "." {
		return nil, 0, fmt.Errorf("%s: cannot write a root: %w", abs, model.ErrInvalidPath)
	}
	dir := path.Dir(rel)
	pk, vk := resumeKeys(path.Base(rel), e)
	name := resumePrefix + pk + "-" + vk + model.TempSuffix
	tmp := path.Join(dir, name)
	if err := f.mkdirAll(rt, dir); err != nil {
		return nil, 0, err
	}
	cleanPartials(rt, dir, resumePrefix+pk+"-", name)

	fh, err := rt.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		f.forgetDirs(rt) // a cached dir was removed behind our back
		if err = f.mkdirAll(rt, dir); err == nil {
			fh, err = rt.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o600)
		}
	}
	if err != nil {
		return nil, 0, err
	}
	w = &Writer{fs: f, rt: rt, rel: rel, tmp: tmp, f: fh, h: sha256.New(), entry: e, keep: true}
	fi, err := fh.Stat()
	if err != nil {
		w.discard()
		return nil, 0, err
	}
	have = fi.Size()
	if have > 0 && have <= e.Size {
		// Hash what is there, so Commit checks the whole file.
		n, err := io.CopyBuffer(w.h, io.LimitReader(fh, have), make([]byte, 1<<20))
		if err != nil || n != have {
			have = 0
		}
	} else {
		have = 0
	}
	if have == 0 {
		w.h = sha256.New()
		if err := fh.Truncate(0); err != nil {
			w.discard()
			return nil, 0, err
		}
	}
	if _, err := fh.Seek(have, io.SeekStart); err != nil {
		w.discard()
		return nil, 0, err
	}
	w.n = have
	return w, have, nil
}

// cleanPartials removes, in dir, partials of the same file with another
// version (prefix) and partials of other files that were not touched for
// stalePartial. Old-style partials (legacyResumePrefix) count the same way.
// Errors are ignored: this is housekeeping.
func cleanPartials(rt *os.Root, dir, prefix, keep string) {
	d, err := rt.Open(dir)
	if err != nil {
		return
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	legacy := legacyResumePrefix + strings.TrimPrefix(prefix, resumePrefix)
	for _, n := range names {
		if n == keep || !strings.HasSuffix(n, model.TempSuffix) ||
			!strings.HasPrefix(n, resumePrefix) && !strings.HasPrefix(n, legacyResumePrefix) {
			continue
		}
		p := path.Join(dir, n)
		if !strings.HasPrefix(n, prefix) && !strings.HasPrefix(n, legacy) {
			fi, err := rt.Lstat(p)
			if err != nil || time.Since(fi.ModTime()) < stalePartial {
				continue
			}
		}
		rt.Remove(p)
	}
}

// OpenAt is Open, but reading starts at offset. The bytes before offset are
// read and hashed locally, so Hash still covers the whole file.
func (f *FS) OpenAt(abs string, offset int64) (*Reader, error) {
	r, err := f.Open(abs)
	if err != nil || offset <= 0 {
		return r, err
	}
	if offset > r.entry.Size {
		r.Close()
		return nil, fmt.Errorf("%s: %w", abs, model.ErrChanged)
	}
	n, err := io.CopyBuffer(r.h, io.LimitReader(r.f, offset), make([]byte, 1<<20))
	r.n = n
	if err == nil && n != offset {
		err = fmt.Errorf("%s: %w", abs, model.ErrChanged)
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
