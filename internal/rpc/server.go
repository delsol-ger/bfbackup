package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/model"
	"bfbackup/internal/proto"
)

// Server answers calls on a session by forwarding them to an Endpoint.
type Server struct {
	EP  endpoint.Endpoint
	Log *slog.Logger
}

// StreamAccepter is the part of a yamux session the server needs.
type StreamAccepter interface {
	Accept() (net.Conn, error)
}

// Serve handles streams until the session ends.
func (s *Server) Serve(ctx context.Context, sess StreamAccepter) error {
	for {
		st, err := sess.Accept()
		if err != nil {
			return err
		}
		go s.serveStream(ctx, st)
	}
}

type handler func(ctx context.Context, p json.RawMessage) (any, error)

func simple[P any, R any](fn func(ctx context.Context, p P) (R, error)) handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p P
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, fmt.Errorf("bad params: %w", err)
			}
		}
		return fn(ctx, p)
	}
}

func (s *Server) simpleHandlers() map[string]handler {
	ep := s.EP
	return map[string]handler{
		"info": simple(func(ctx context.Context, _ struct{}) (model.NodeInfo, error) { return ep.Info(ctx) }),
		"listdir": simple(func(ctx context.Context, p pathParams) ([]model.Entry, error) {
			return ep.ListDir(ctx, p.Path)
		}),
		"stat": simple(func(ctx context.Context, p statParams) ([]model.StatResult, error) {
			return ep.Stat(ctx, p.Paths)
		}),
		"hash": simple(func(ctx context.Context, p pathParams) (string, error) { return ep.Hash(ctx, p.Path) }),
		"trash": simple(func(ctx context.Context, p trashParams) (struct{}, error) {
			return struct{}{}, ep.Trash(ctx, p.Root, p.Rel, p.Batch)
		}),
		"trashinfo": simple(func(ctx context.Context, p pathParams) ([]model.TrashBatch, error) {
			l, ok := ep.(endpoint.TrashLister)
			if !ok {
				return nil, model.ErrUnsupported
			}
			return l.TrashInfo(ctx, p.Path)
		}),
		"remove": simple(func(ctx context.Context, p trashParams) (struct{}, error) {
			return struct{}{}, ep.Remove(ctx, p.Root, p.Rel)
		}),
		"purge": simple(func(ctx context.Context, p purgeParams) (int, error) {
			return ep.PurgeTrash(ctx, p.Root, time.Unix(0, p.OlderThan))
		}),
		"statfs": simple(func(ctx context.Context, p pathParams) (model.FSUsage, error) {
			return ep.StatFS(ctx, p.Path)
		}),
		"hook": simple(func(ctx context.Context, p hookParams) (model.HookResult, error) {
			return ep.RunHook(ctx, p.Name, p.Env)
		}),
		"symlink": simple(func(ctx context.Context, p symlinkParams) (struct{}, error) {
			return struct{}{}, ep.Symlink(ctx, p.Path, p.Target)
		}),
		"mkdir": simple(func(ctx context.Context, p pathParams) (struct{}, error) {
			m, ok := ep.(endpoint.Mkdirer)
			if !ok {
				return struct{}{}, model.ErrUnsupported
			}
			return struct{}{}, m.Mkdir(ctx, p.Path)
		}),
	}
}

func (s *Server) serveStream(ctx context.Context, st net.Conn) {
	defer st.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	br := bufio.NewReaderSize(st, bufSize)
	bw := bufio.NewWriterSize(st, bufSize)
	defer bw.Flush()

	var req request
	if err := proto.ReadFrame(br, &req); err != nil {
		return
	}
	switch req.M {
	case "scan":
		s.serveScan(ctx, req.P, bw, req.Z)
		return
	case "read", "readat":
		s.serveRead(ctx, req.P, bw, req.Z, req.M == "readat")
		return
	case "write":
		s.serveWrite(ctx, req.P, br, bw, req.Z)
		return
	}
	h, ok := s.simpleHandlers()[req.M]
	if !ok {
		proto.WriteFrame(bw, response{Err: "unknown method " + req.M})
		return
	}
	// The client sends nothing after the request, so a read returning means
	// the stream was closed: cancel the work.
	go func() {
		var b [1]byte
		br.Read(b[:])
		cancel()
	}()
	res, err := h(ctx, req.P)
	if err != nil {
		proto.WriteFrame(bw, errResponse(err))
		return
	}
	b, err := json.Marshal(res)
	if err != nil {
		proto.WriteFrame(bw, errResponse(err))
		return
	}
	proto.WriteFrame(bw, response{R: b})
}

// scanFlushEvery pushes buffered listings and progress out at least this
// often, so a slow scan keeps data flowing on the stream. Tests shorten it.
var scanFlushEvery = 2 * time.Second

func (s *Server) serveScan(ctx context.Context, raw json.RawMessage, bw *bufio.Writer, z bool) {
	var req model.ScanRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		proto.WriteFrame(bw, scanFrame{Done: true, Err: err.Error()})
		return
	}
	var out io.Writer = bw
	var zw *zWriter
	if z {
		if err := proto.WriteFrame(bw, scanFrame{Z: true}); err != nil {
			return
		}
		zw = newZWriter(bw)
		defer zw.release()
		out = zw
	}
	// Listings and progress frames share the stream.
	var mu sync.Mutex
	flush := func() error {
		if zw != nil {
			if err := zw.Flush(); err != nil {
				return err
			}
		}
		return bw.Flush()
	}
	// Progress frames and a timed flush, so the controller sees a slow scan
	// move and gets listings that would otherwise wait in the buffer until
	// the next folder is done.
	var hashed, dirs atomic.Int64
	var curMu sync.Mutex
	cur := ""
	if req.Hash {
		req.Hashed = func(n int64) { hashed.Add(n) }
	}
	req.Reading = func(dir string, n int64) {
		dirs.Add(n)
		curMu.Lock()
		cur = dir
		curMu.Unlock()
	}
	dirty := false // listings written since the last flush; under mu
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(scanFlushEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			f := scanFrame{H: hashed.Swap(0), D: dirs.Swap(0)}
			if f.D > 0 {
				curMu.Lock()
				f.C = cur
				curMu.Unlock()
			}
			mu.Lock()
			var err error
			if f.H > 0 || f.D > 0 {
				err = proto.WriteFrame(out, f)
				dirty = true
			}
			if err == nil && dirty {
				err = flush()
				dirty = false
			}
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	lastFlush := time.Now()
	err := s.EP.Scan(ctx, req, func(l model.DirListing) error {
		mu.Lock()
		defer mu.Unlock()
		dirty = true
		entries := l.Entries
		for len(entries) > scanBatch {
			part := model.DirListing{Dir: l.Dir, Entries: entries[:scanBatch], More: true}
			if err := proto.WriteFrame(out, scanFrame{L: &part}); err != nil {
				return err
			}
			entries = entries[scanBatch:]
		}
		last := model.DirListing{Dir: l.Dir, Entries: entries, Skipped: l.Skipped}
		if err := proto.WriteFrame(out, scanFrame{L: &last}); err != nil {
			return err
		}
		if time.Since(lastFlush) >= scanFlushEvery {
			lastFlush = time.Now()
			dirty = false
			return flush()
		}
		return nil
	})
	close(stop)
	wg.Wait()
	f := scanFrame{Done: true}
	if err != nil {
		f.Err, f.Code = err.Error(), model.ErrorCode(err)
	}
	proto.WriteFrame(out, f)
	if zw != nil {
		zw.Close()
	}
}

// errWriter remembers write errors so they can be told apart from errors
// of the source.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// serveRead answers "read" and, with at, "readat" (from an offset).
func (s *Server) serveRead(ctx context.Context, raw json.RawMessage, bw *bufio.Writer, z, at bool) {
	var p readAtParams
	if err := json.Unmarshal(raw, &p); err != nil {
		proto.WriteFrame(bw, errResponse(err))
		return
	}
	var r endpoint.Reader
	var err error
	if at {
		rs, ok := s.EP.(endpoint.Resumer)
		if !ok {
			proto.WriteFrame(bw, errResponse(fmt.Errorf("readat: %w", model.ErrUnsupported)))
			return
		}
		r, err = rs.OpenAt(ctx, p.Path, p.Offset)
	} else {
		r, err = s.EP.Open(ctx, p.Path)
	}
	if err != nil {
		proto.WriteFrame(bw, errResponse(err))
		return
	}
	defer r.Close()
	z = z && worthCompressing(p.Path)
	entry, _ := json.Marshal(r.Entry())
	if err := proto.WriteFrame(bw, response{R: entry, Z: z}); err != nil {
		return
	}
	ew := &errWriter{w: bw}
	cw := &proto.ChunkWriter{W: ew}
	var dst io.Writer = cw
	var zw *zWriter
	if z {
		zw = newZWriter(cw)
		defer zw.release()
		dst = zw
	}
	buf := make([]byte, proto.MaxChunk)
	_, err = io.CopyBuffer(dst, struct{ io.Reader }{r}, buf)
	if ew.err != nil {
		return // client went away
	}
	if zw != nil {
		// Also after a source error: the client decodes up to here and then
		// reads the error from the trailer.
		if zw.Close() != nil || ew.err != nil {
			return
		}
	}
	if err := cw.Close(); err != nil {
		return
	}
	tr := trailer{Hash: r.Hash()}
	if err != nil {
		tr = trailer{Err: err.Error(), Code: model.ErrorCode(err)}
	}
	proto.WriteFrame(bw, tr)
}

func (s *Server) serveWrite(ctx context.Context, raw json.RawMessage, br *bufio.Reader, bw *bufio.Writer, z bool) {
	var p writeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		proto.WriteFrame(bw, errResponse(err))
		return
	}
	var w endpoint.Writer
	var have int64
	var err error
	rs, resumable := s.EP.(endpoint.Resumer)
	if p.Resume && resumable {
		w, have, err = rs.CreateResumable(ctx, p.Path, p.Entry)
	} else {
		w, err = s.EP.Create(ctx, p.Path, p.Entry)
	}
	if err != nil {
		proto.WriteFrame(bw, errResponse(err))
		return
	}
	resp := response{Z: z}
	if p.Resume {
		resp.R, _ = json.Marshal(have)
	}
	if err := proto.WriteFrame(bw, resp); err != nil || bw.Flush() != nil {
		w.Abort()
		return
	}
	cr := &proto.ChunkReader{R: br}
	var src io.Reader = cr
	if z {
		zr, err := newZReader(cr)
		if err != nil {
			w.Abort()
			return
		}
		defer zr.Close()
		src = zr
	}
	buf := make([]byte, proto.MaxChunk)
	_, err = io.CopyBuffer(struct{ io.Writer }{w}, src, buf)
	if err == nil && z {
		err = cr.Drain()
	}
	if err != nil {
		w.Abort()
		if !errors.Is(err, io.ErrUnexpectedEOF) && s.Log != nil {
			s.Log.Warn("write stream failed", "path", p.Path, "err", err)
		}
		return
	}
	var tr trailer
	if err := proto.ReadFrame(br, &tr); err != nil || tr.Abort {
		w.Abort()
		return
	}
	sum, err := w.Commit(ctx, tr.Hash)
	if err != nil {
		proto.WriteFrame(bw, errResponse(err))
		return
	}
	b, _ := json.Marshal(sum)
	proto.WriteFrame(bw, response{R: b})
}
