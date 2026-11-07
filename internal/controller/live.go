package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/model"
	"bfbackup/internal/rpc"
)

// How long work waits for a peer that dropped its connection. File
// transfers wait as long as the engine says (engine.reconnectWait).
var (
	// callWait: a simple call (stat, list, …) cut off by a drop waits this
	// long for the peer and is then repeated once.
	callWait = 2 * time.Minute
	// pollOnline is how often a waiting call looks for the new connection.
	pollOnline = time.Second
)

// compressTransfers compresses file data and scan listings to and from
// peers (zstd, already compressed file types are sent as they are).
// BFB_COMPRESS=off turns it off, e.g. for a fast LAN with slow CPUs.
var compressTransfers = !isOff(os.Getenv("BFB_COMPRESS"))

func isOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "off", "false", "no":
		return true
	}
	return false
}

// liveEndpoint is a peer's endpoint that always uses the peer's current
// connection, so a run goes on after the peer reconnects. Calls cut off by
// a drop are repeated where that is safe; file transfers are repeated by
// the engine, which calls WaitOnline first.
type liveEndpoint struct {
	c    *Controller
	node string

	mu        sync.Mutex
	downSince time.Time // when this endpoint first saw the peer gone; zero while online
}

var (
	_ endpoint.Endpoint = (*liveEndpoint)(nil)
	_ endpoint.Resumer  = (*liveEndpoint)(nil)
	_ endpoint.Mkdirer  = (*liveEndpoint)(nil)

	_ endpoint.TrashLister = (*liveEndpoint)(nil)
)

func (c *Controller) liveEndpoint(nodeID string) *liveEndpoint {
	return &liveEndpoint{c: c, node: nodeID}
}

// remote returns the peer's current connection or model.ErrOffline.
func (l *liveEndpoint) remote() (*rpc.Remote, error) {
	l.c.mu.Lock()
	p, ok := l.c.peers[l.node]
	l.c.mu.Unlock()
	if !ok || p.sess.IsClosed() {
		return nil, fmt.Errorf("%s: %w", l.node, model.ErrOffline)
	}
	return p.remote, nil
}

func (l *liveEndpoint) ctx(ctx context.Context) context.Context {
	if compressTransfers {
		return rpc.WithCompression(ctx)
	}
	return ctx
}

// WaitOnline waits up to max for the peer to be connected. Once the peer
// has been gone for longer than max, it returns model.ErrOffline at once,
// so the remaining files of a run do not each wait again.
// While it waits, it tells the run through endpoint.WaitNote.
func (l *liveEndpoint) WaitOnline(ctx context.Context, max time.Duration) error {
	note := endpoint.WaitNote(ctx)
	name := ""
	for {
		if _, err := l.remote(); err == nil {
			l.mu.Lock()
			l.downSince = time.Time{}
			l.mu.Unlock()
			if name != "" {
				note("")
			}
			return nil
		}
		l.mu.Lock()
		if l.downSince.IsZero() {
			l.downSince = time.Now()
		}
		gone := time.Since(l.downSince)
		l.mu.Unlock()
		if name == "" {
			name = l.c.nodeName(ctx, l.node)
		}
		if gone >= max {
			note("")
			return fmt.Errorf("%s: %w: no connection for %s", name, model.ErrOffline, gone.Round(time.Second))
		}
		note(fmt.Sprintf("connection to %s lost %s ago, waiting for it to come back (gives up in %s)",
			name, gone.Round(time.Second), (max - gone).Round(time.Second)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(pollOnline, max-gone)):
		}
	}
}

// again runs a call that is safe to repeat, once more after the peer is
// back if the connection broke during the first try.
func again[T any](ctx context.Context, l *liveEndpoint, fn func(r *rpc.Remote) (T, error)) (T, error) {
	r, err := l.remote()
	if err != nil {
		var zero T
		return zero, err
	}
	v, err := fn(r)
	if !errors.Is(err, model.ErrConnLost) || ctx.Err() != nil {
		return v, err
	}
	if l.WaitOnline(ctx, callWait) != nil {
		return v, err
	}
	if r, err = l.remote(); err != nil {
		return v, err
	}
	return fn(r)
}

// once runs a call that must not be repeated blindly (it may have done its
// work before the connection broke).
func once[T any](l *liveEndpoint, fn func(r *rpc.Remote) (T, error)) (T, error) {
	r, err := l.remote()
	if err != nil {
		var zero T
		return zero, err
	}
	return fn(r)
}

func (l *liveEndpoint) Info(ctx context.Context) (model.NodeInfo, error) {
	return again(ctx, l, func(r *rpc.Remote) (model.NodeInfo, error) { return r.Info(ctx) })
}

func (l *liveEndpoint) ListDir(ctx context.Context, path string) ([]model.Entry, error) {
	return again(ctx, l, func(r *rpc.Remote) ([]model.Entry, error) { return r.ListDir(ctx, path) })
}

func (l *liveEndpoint) Stat(ctx context.Context, paths []string) ([]model.StatResult, error) {
	return again(ctx, l, func(r *rpc.Remote) ([]model.StatResult, error) { return r.Stat(ctx, paths) })
}

func (l *liveEndpoint) Hash(ctx context.Context, path string) (string, error) {
	return again(ctx, l, func(r *rpc.Remote) (string, error) { return r.Hash(ctx, path) })
}

func (l *liveEndpoint) StatFS(ctx context.Context, path string) (model.FSUsage, error) {
	return again(ctx, l, func(r *rpc.Remote) (model.FSUsage, error) { return r.StatFS(ctx, path) })
}

func (l *liveEndpoint) PurgeTrash(ctx context.Context, root string, olderThan time.Time) (int, error) {
	return again(ctx, l, func(r *rpc.Remote) (int, error) { return r.PurgeTrash(ctx, root, olderThan) })
}

// Symlink replaces whatever is at path, so repeating it is safe.
func (l *liveEndpoint) Symlink(ctx context.Context, path, target string) error {
	_, err := again(ctx, l, func(r *rpc.Remote) (struct{}, error) { return struct{}{}, r.Symlink(ctx, path, target) })
	return err
}

// Mkdir creates nothing when the folder exists, so repeating it is safe.
func (l *liveEndpoint) Mkdir(ctx context.Context, path string) error {
	_, err := again(ctx, l, func(r *rpc.Remote) (struct{}, error) { return struct{}{}, r.Mkdir(ctx, path) })
	return err
}

func (l *liveEndpoint) TrashInfo(ctx context.Context, root string) ([]model.TrashBatch, error) {
	return again(ctx, l, func(r *rpc.Remote) ([]model.TrashBatch, error) { return r.TrashInfo(ctx, root) })
}

func (l *liveEndpoint) Trash(ctx context.Context, root, rel, batch string) error {
	_, err := once(l, func(r *rpc.Remote) (struct{}, error) { return struct{}{}, r.Trash(ctx, root, rel, batch) })
	return err
}

func (l *liveEndpoint) Remove(ctx context.Context, root, rel string) error {
	_, err := once(l, func(r *rpc.Remote) (struct{}, error) { return struct{}{}, r.Remove(ctx, root, rel) })
	return err
}

func (l *liveEndpoint) RunHook(ctx context.Context, name string, env map[string]string) (model.HookResult, error) {
	return once(l, func(r *rpc.Remote) (model.HookResult, error) { return r.RunHook(ctx, name, env) })
}

// Scan is not repeated here: fn has already seen part of the listing. The
// engine restarts a whole scan after a drop.
func (l *liveEndpoint) Scan(ctx context.Context, req model.ScanRequest, fn func(model.DirListing) error) error {
	_, err := once(l, func(r *rpc.Remote) (struct{}, error) { return struct{}{}, r.Scan(l.ctx(ctx), req, fn) })
	return err
}

func (l *liveEndpoint) Open(ctx context.Context, path string) (endpoint.Reader, error) {
	return once(l, func(r *rpc.Remote) (endpoint.Reader, error) { return r.Open(l.ctx(ctx), path) })
}

func (l *liveEndpoint) OpenAt(ctx context.Context, path string, offset int64) (endpoint.Reader, error) {
	return once(l, func(r *rpc.Remote) (endpoint.Reader, error) { return r.OpenAt(l.ctx(ctx), path, offset) })
}

func (l *liveEndpoint) Create(ctx context.Context, path string, e model.Entry) (endpoint.Writer, error) {
	return once(l, func(r *rpc.Remote) (endpoint.Writer, error) { return r.Create(l.ctx(ctx), path, e) })
}

func (l *liveEndpoint) CreateResumable(ctx context.Context, path string, e model.Entry) (endpoint.Writer, int64, error) {
	r, err := l.remote()
	if err != nil {
		return nil, 0, err
	}
	return r.CreateResumable(l.ctx(ctx), path, e)
}
