// Package endpoint defines what the engine needs from a node's file system.
// Local works on this machine; rpc.Remote implements the same interface for
// a connected peer, so all engine logic is independent of where data lives.
package endpoint

import (
	"context"
	"io"
	"os"
	"time"

	"bfbackup/internal/fsx"
	"bfbackup/internal/hooks"
	"bfbackup/internal/model"
)

// Reader streams one file. Read returns model.ErrChanged instead of io.EOF
// if the file changed during the read. Hash is valid after io.EOF.
type Reader interface {
	io.ReadCloser
	Entry() model.Entry
	Hash() string
}

// Writer receives one file. Commit verifies the data against expect (when
// not empty), moves it into place and returns its SHA-256. Abort discards it.
type Writer interface {
	io.Writer
	Commit(ctx context.Context, expect string) (string, error)
	Abort() error
}

// Endpoint is a node's file system as seen by the engine. All paths are
// absolute paths on that node.
type Endpoint interface {
	Info(ctx context.Context) (model.NodeInfo, error)
	ListDir(ctx context.Context, path string) ([]model.Entry, error)
	Scan(ctx context.Context, req model.ScanRequest, fn func(model.DirListing) error) error
	Open(ctx context.Context, path string) (Reader, error)
	Create(ctx context.Context, path string, e model.Entry) (Writer, error)
	Symlink(ctx context.Context, path, target string) error
	Stat(ctx context.Context, paths []string) ([]model.StatResult, error)
	Hash(ctx context.Context, path string) (string, error)
	// Trash moves root/rel into the trash batch named batch (see
	// model.TrashBatchName); "" names it after the current time.
	Trash(ctx context.Context, root, rel, batch string) error
	// Remove deletes root/rel for good, without the trash.
	Remove(ctx context.Context, root, rel string) error
	PurgeTrash(ctx context.Context, root string, olderThan time.Time) (int, error)
	StatFS(ctx context.Context, path string) (model.FSUsage, error)
	RunHook(ctx context.Context, name string, env map[string]string) (model.HookResult, error)
}

// Resumer is implemented by endpoints that can continue an interrupted
// transfer instead of starting the file over.
type Resumer interface {
	// CreateResumable is Create, but data of an earlier, aborted attempt for
	// the same path and entry (size, mtime) is kept: have is the number of
	// bytes already there, the caller writes only the rest. Abort keeps the
	// data for the next attempt; a failed Commit discards it.
	CreateResumable(ctx context.Context, path string, e model.Entry) (w Writer, have int64, err error)
	// OpenAt is Open, but Read starts at offset. Hash covers the whole file.
	OpenAt(ctx context.Context, path string, offset int64) (Reader, error)
}

// Mkdirer is implemented by endpoints that can create a folder on request
// (the folder picker). Older peers answer model.ErrUnsupported.
type Mkdirer interface {
	Mkdir(ctx context.Context, path string) error
}

// TrashLister is implemented by endpoints that can sum up a root's trash.
// Older peers answer model.ErrUnsupported.
type TrashLister interface {
	TrashInfo(ctx context.Context, root string) ([]model.TrashBatch, error)
}

type waitNoteKey struct{}

// WithWaitNote returns ctx carrying fn. Endpoints call it while a call waits
// for a peer that dropped its connection (msg says for whom and how long)
// and with "" once the wait is over.
func WithWaitNote(ctx context.Context, fn func(msg string)) context.Context {
	return context.WithValue(ctx, waitNoteKey{}, fn)
}

// WaitNote returns the function set with WithWaitNote, or one that does
// nothing.
func WaitNote(ctx context.Context) func(msg string) {
	if fn, ok := ctx.Value(waitNoteKey{}).(func(string)); ok {
		return fn
	}
	return func(string) {}
}

// Local is the Endpoint for this machine.
type Local struct {
	FS      *fsx.FS
	Hooks   hooks.Runner
	Version string
	Now     func() time.Time // for tests; defaults to time.Now
}

var (
	_ Endpoint = (*Local)(nil)
	_ Resumer  = (*Local)(nil)
	_ Mkdirer  = (*Local)(nil)
)

func (l *Local) Mkdir(ctx context.Context, path string) error {
	return l.FS.Mkdir(path)
}

func (l *Local) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Info reports host name, version, roots with capacity, and hooks.
func (l *Local) Info(ctx context.Context) (model.NodeInfo, error) {
	host, _ := os.Hostname()
	info := model.NodeInfo{Hostname: host, Version: l.Version}
	for _, r := range l.FS.Roots() {
		ri := model.RootInfo{Path: r}
		if u, err := l.FS.StatFS(r); err == nil {
			ri.Total, ri.Free = u.Total, u.Free
		}
		info.Roots = append(info.Roots, ri)
	}
	info.Disks = l.FS.Disks()
	hs, err := l.Hooks.List()
	if err != nil {
		return info, err
	}
	info.Hooks = hs
	return info, nil
}

func (l *Local) ListDir(ctx context.Context, path string) ([]model.Entry, error) {
	return l.FS.ListDir(path)
}

func (l *Local) Scan(ctx context.Context, req model.ScanRequest, fn func(model.DirListing) error) error {
	return l.FS.Scan(ctx, req, fn)
}

func (l *Local) Open(ctx context.Context, path string) (Reader, error) {
	return l.FS.Open(path)
}

func (l *Local) Create(ctx context.Context, path string, e model.Entry) (Writer, error) {
	w, err := l.FS.Create(path, e)
	if err != nil {
		return nil, err
	}
	return localWriter{w}, nil
}

func (l *Local) CreateResumable(ctx context.Context, path string, e model.Entry) (Writer, int64, error) {
	w, have, err := l.FS.CreateResumable(path, e)
	if err != nil {
		return nil, 0, err
	}
	return localWriter{w}, have, nil
}

func (l *Local) OpenAt(ctx context.Context, path string, offset int64) (Reader, error) {
	return l.FS.OpenAt(path, offset)
}

type localWriter struct{ *fsx.Writer }

func (w localWriter) Commit(_ context.Context, expect string) (string, error) {
	return w.Writer.Commit(expect)
}

func (l *Local) Symlink(ctx context.Context, path, target string) error {
	return l.FS.Symlink(path, target)
}

// Stat stats each path. Missing paths are reported with Exists=false; other
// errors abort the whole call.
func (l *Local) Stat(ctx context.Context, paths []string) ([]model.StatResult, error) {
	out := make([]model.StatResult, len(paths))
	for i, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out[i].Path = p
		e, err := l.FS.Stat(p)
		switch {
		case err == nil:
			out[i].Exists, out[i].Entry = true, e
		case os.IsNotExist(err):
		default:
			return nil, err
		}
	}
	return out, nil
}

func (l *Local) Hash(ctx context.Context, path string) (string, error) {
	h, _, err := l.FS.HashFile(path)
	return h, err
}

func (l *Local) Trash(ctx context.Context, root, rel, batch string) error {
	return l.FS.Trash(root, rel, batch, l.now())
}

func (l *Local) TrashInfo(ctx context.Context, root string) ([]model.TrashBatch, error) {
	return l.FS.TrashInfo(root)
}

func (l *Local) Remove(ctx context.Context, root, rel string) error {
	return l.FS.Remove(root, rel)
}

func (l *Local) PurgeTrash(ctx context.Context, root string, olderThan time.Time) (int, error) {
	return l.FS.PurgeTrash(root, olderThan)
}

func (l *Local) StatFS(ctx context.Context, path string) (model.FSUsage, error) {
	return l.FS.StatFS(path)
}

func (l *Local) RunHook(ctx context.Context, name string, env map[string]string) (model.HookResult, error) {
	return l.Hooks.Run(ctx, name, env)
}
