package fsx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"bfbackup/internal/match"
	"bfbackup/internal/model"
)

// DefaultScanWorkers is the number of directories read in parallel.
const DefaultScanWorkers = 8

// Scan walks req.Root (or only req.Includes below it) and calls emit once per
// directory, from a single goroutine. Directories are read in parallel, so
// the order of listings is not defined. Excluded paths are not descended
// into. The trash dir at the root and internal temp files are skipped.
func (f *FS) Scan(ctx context.Context, req model.ScanRequest, emit func(model.DirListing) error) error {
	rt, base, err := f.resolve(req.Root)
	if err != nil {
		return err
	}
	m, err := match.New(req.Excludes)
	if err != nil {
		return err
	}
	starts, err := normalizeIncludes(req.Includes)
	if err != nil {
		return err
	}
	for _, s := range starts {
		fi, err := rt.Lstat(joinRel(base, s))
		if err != nil {
			return fmt.Errorf("include %q: %w", s, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("include %q is not a directory: %w", s, model.ErrTypeConflict)
		}
	}
	workers := req.Workers
	if workers <= 0 {
		workers = DefaultScanWorkers
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w := &walker{queue: starts, pending: len(starts)}
	w.cond = sync.NewCond(&w.mu)
	results := make(chan model.DirListing, workers*2)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				dir, ok := w.next()
				if !ok {
					return
				}
				if req.Reading != nil {
					req.Reading(dir, 1)
				}
				listing, subdirs, err := f.scanDir(ctx, rt, base, dir, m, req.Hash, req.Hashed)
				if err != nil {
					w.fail(err)
					return
				}
				w.done(subdirs)
				select {
				case results <- listing:
				case <-ctx.Done():
					w.fail(ctx.Err())
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var emitErr error
	for l := range results {
		if emitErr != nil {
			continue // drain so workers can exit
		}
		if err := emit(l); err != nil {
			emitErr = err
			cancel()
			w.fail(err)
		}
	}
	if emitErr != nil {
		return emitErr
	}
	if err := w.error(); err != nil {
		return err
	}
	return ctx.Err()
}

// normalizeIncludes cleans include dirs and drops those nested in others.
func normalizeIncludes(in []string) ([]string, error) {
	if len(in) == 0 {
		return []string{""}, nil
	}
	var cleaned []string
	for _, s := range in {
		c, err := model.CleanRel(s)
		if err != nil {
			return nil, err
		}
		if c == "" {
			return []string{""}, nil
		}
		cleaned = append(cleaned, c)
	}
	sort.Strings(cleaned)
	var out []string
	for _, c := range cleaned {
		nested := false
		for _, o := range out {
			if c == o || strings.HasPrefix(c, o+"/") {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *FS) scanDir(ctx context.Context, rt *os.Root, base, dir string, m *match.Matcher, hash bool, hashed func(int64)) (model.DirListing, []string, error) {
	entries, special, err := readDirSpecial(rt, joinRel(base, dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && dir != "" {
			// Removed during the scan: report it as empty.
			return model.DirListing{Dir: dir}, nil, nil
		}
		return model.DirListing{}, nil, err
	}
	out := entries[:0]
	var subdirs []string
	for _, e := range entries {
		if model.IsInternalName(e.Name) || (dir == "" && e.Name == model.TrashDirName) {
			continue
		}
		rel := model.JoinRel(dir, e.Name)
		if m.Excluded(rel, e.Type == model.TypeDir) {
			continue
		}
		if e.Type == model.TypeDir {
			subdirs = append(subdirs, rel)
		}
		if hash && e.Type == model.TypeFile {
			if err := ctx.Err(); err != nil {
				return model.DirListing{}, nil, err
			}
			sum, size, err := f.hashRel(ctx, rt, path.Join(base, rel), hashed)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if !errors.Is(err, model.ErrChanged) {
					return model.DirListing{}, nil, err
				}
				// Changed while hashing: leave the hash empty; the diff
				// treats that as changed and the copy re-checks it.
			} else {
				e.Hash = sum
				e.Size = size
			}
		}
		out = append(out, e)
	}
	var skipped []string
	for _, name := range special {
		if rel := model.JoinRel(dir, name); !m.Excluded(rel, false) {
			skipped = append(skipped, name)
		}
	}
	return model.DirListing{Dir: dir, Entries: out, Skipped: skipped}, subdirs, nil
}

func (f *FS) hashRel(ctx context.Context, rt *os.Root, rel string, hashed func(int64)) (string, int64, error) {
	fh, err := rt.Open(rel)
	if err != nil {
		return "", 0, err
	}
	fi, err := fh.Stat()
	if err != nil {
		fh.Close()
		return "", 0, err
	}
	e, _ := entryFromInfo(path.Base(rel), fi)
	r := &Reader{f: fh, entry: e, h: newHash()}
	defer r.Close()
	if err := drain(ctxReader{ctx: ctx, r: r, n: hashed}); err != nil {
		return "", 0, err
	}
	return r.Hash(), r.n, nil
}

type walker struct {
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []string
	pending int // dirs queued or being read
	err     error
}

func (w *walker) next() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.queue) == 0 && w.pending > 0 && w.err == nil {
		w.cond.Wait()
	}
	if w.err != nil || len(w.queue) == 0 {
		return "", false
	}
	d := w.queue[len(w.queue)-1]
	w.queue = w.queue[:len(w.queue)-1]
	return d, true
}

func (w *walker) done(subdirs []string) {
	w.mu.Lock()
	w.queue = append(w.queue, subdirs...)
	w.pending += len(subdirs) - 1
	w.mu.Unlock()
	w.cond.Broadcast()
}

func (w *walker) fail(err error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
	w.cond.Broadcast()
}

func (w *walker) error() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// ctxReader makes long hashes stop when ctx ends and reports the bytes
// read to n, if set.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
	n   func(int64)
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if n > 0 && c.n != nil {
		c.n(int64(n))
	}
	return n, err
}
