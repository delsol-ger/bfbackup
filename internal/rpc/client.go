package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/model"
	"bfbackup/internal/proto"
)

// StreamOpener is the part of a yamux session the client needs.
type StreamOpener interface {
	Open() (net.Conn, error)
	IsClosed() bool
}

// DefaultIdleTimeout stops a file transfer that moved no data for this long.
const DefaultIdleTimeout = 5 * time.Minute

// ErrStalled is returned when a file transfer moved no data for the idle
// timeout. The stream is closed; the session and other transfers go on.
// It is a model.ErrConnLost, so the transfer is retried like after a drop.
var ErrStalled error = stalledError{}

type stalledError struct{}

func (stalledError) Error() string { return "the transfer stalled" }
func (stalledError) Unwrap() error { return model.ErrConnLost }

// Remote is an endpoint.Endpoint backed by a peer.
type Remote struct {
	Session StreamOpener
	// IdleTimeout applies to file reads and writes (not to calls such as
	// hash or hook, which may legitimately work for a long time without
	// sending anything). 0 means DefaultIdleTimeout.
	IdleTimeout time.Duration
}

var (
	_ endpoint.Endpoint = (*Remote)(nil)
	_ endpoint.Resumer  = (*Remote)(nil)
)

type stream struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	stop func() bool
	ctx  context.Context
	idle time.Duration
}

func (s *stream) close() {
	s.stop()
	s.conn.Close()
}

// arm gives the next reads and writes of a file transfer the idle timeout.
func (s *stream) arm() {
	s.conn.SetDeadline(time.Now().Add(s.idle))
}

// armReply waits up to d (at least the idle timeout) for the next answer.
func (s *stream) armReply(d time.Duration) {
	s.conn.SetDeadline(time.Now().Add(max(d, s.idle)))
}

// fix turns errors caused by our own cancellation into ctx.Err(), deadline
// errors into ErrStalled, and other transport errors into
// model.ErrConnLost, so callers can tell them from errors of the peer.
func (s *stream) fix(err error) error {
	if err == nil {
		return nil
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if errors.Is(err, model.ErrConnLost) {
		return err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%w: no data for %s, the transfer was stopped", ErrStalled, s.idle)
	}
	return fmt.Errorf("%w: %v", model.ErrConnLost, err)
}

func (r *Remote) open(ctx context.Context, method string, params any, z bool) (*stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.Session == nil || r.Session.IsClosed() {
		return nil, model.ErrOffline
	}
	conn, err := r.Session.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", model.ErrOffline, err)
	}
	s := &stream{
		conn: conn,
		br:   bufio.NewReaderSize(conn, bufSize),
		bw:   bufio.NewWriterSize(conn, bufSize),
		ctx:  ctx,
		idle: r.IdleTimeout,
	}
	if s.idle <= 0 {
		s.idle = DefaultIdleTimeout
	}
	s.stop = context.AfterFunc(ctx, func() { conn.Close() })
	raw, err := json.Marshal(params)
	if err != nil {
		s.close()
		return nil, err
	}
	if err := proto.WriteFrame(s.bw, request{M: method, P: raw, Z: z}); err != nil {
		s.close()
		return nil, s.fix(err)
	}
	if err := s.bw.Flush(); err != nil {
		s.close()
		return nil, s.fix(err)
	}
	return s, nil
}

// readResp reads one response frame; errors of the peer are returned as
// such, transport errors as model.ErrConnLost.
func (s *stream) readResp() (response, error) {
	var resp response
	if err := proto.ReadFrame(s.br, &resp); err != nil {
		return resp, s.fix(err)
	}
	return resp, resp.error()
}

func (s *stream) readResponse(result any) error {
	resp, err := s.readResp()
	if err != nil {
		return err
	}
	if result != nil && len(resp.R) > 0 {
		return json.Unmarshal(resp.R, result)
	}
	return nil
}

func (r *Remote) call(ctx context.Context, method string, params, result any) error {
	s, err := r.open(ctx, method, params, false)
	if err != nil {
		return err
	}
	defer s.close()
	return s.readResponse(result)
}

func (r *Remote) Info(ctx context.Context) (model.NodeInfo, error) {
	var out model.NodeInfo
	err := r.call(ctx, "info", struct{}{}, &out)
	return out, err
}

func (r *Remote) ListDir(ctx context.Context, path string) ([]model.Entry, error) {
	var out []model.Entry
	err := r.call(ctx, "listdir", pathParams{Path: path}, &out)
	return out, err
}

func (r *Remote) Stat(ctx context.Context, paths []string) ([]model.StatResult, error) {
	var out []model.StatResult
	err := r.call(ctx, "stat", statParams{Paths: paths}, &out)
	return out, err
}

func (r *Remote) Hash(ctx context.Context, path string) (string, error) {
	var out string
	err := r.call(ctx, "hash", pathParams{Path: path}, &out)
	return out, err
}

func (r *Remote) Trash(ctx context.Context, root, rel, batch string) error {
	return r.call(ctx, "trash", trashParams{Root: root, Rel: rel, Batch: batch}, nil)
}

func (r *Remote) TrashInfo(ctx context.Context, root string) ([]model.TrashBatch, error) {
	var out []model.TrashBatch
	err := r.call(ctx, "trashinfo", pathParams{Path: root}, &out)
	return out, err
}

func (r *Remote) Remove(ctx context.Context, root, rel string) error {
	return r.call(ctx, "remove", trashParams{Root: root, Rel: rel}, nil)
}

func (r *Remote) Mkdir(ctx context.Context, path string) error {
	return r.call(ctx, "mkdir", pathParams{Path: path}, nil)
}

func (r *Remote) PurgeTrash(ctx context.Context, root string, olderThan time.Time) (int, error) {
	var n int
	err := r.call(ctx, "purge", purgeParams{Root: root, OlderThan: olderThan.UnixNano()}, &n)
	return n, err
}

func (r *Remote) StatFS(ctx context.Context, path string) (model.FSUsage, error) {
	var out model.FSUsage
	err := r.call(ctx, "statfs", pathParams{Path: path}, &out)
	return out, err
}

func (r *Remote) RunHook(ctx context.Context, name string, env map[string]string) (model.HookResult, error) {
	var out model.HookResult
	err := r.call(ctx, "hook", hookParams{Name: name, Env: env}, &out)
	return out, err
}

func (r *Remote) Symlink(ctx context.Context, path, target string) error {
	return r.call(ctx, "symlink", symlinkParams{Path: path, Target: target}, nil)
}

// Scan reassembles split listings so fn always sees whole directories.
func (r *Remote) Scan(ctx context.Context, req model.ScanRequest, fn func(model.DirListing) error) error {
	s, err := r.open(ctx, "scan", req, compressOn(ctx))
	if err != nil {
		return err
	}
	defer s.close()
	var in io.Reader = s.br
	first := true
	var pending *model.DirListing
	for {
		var f scanFrame
		if err := proto.ReadFrame(in, &f); err != nil {
			return s.fix(err)
		}
		if first && f.Z {
			// The peer compresses the rest of the stream.
			zr, err := newZReader(s.br)
			if err != nil {
				return err
			}
			defer zr.Close()
			in = bufio.NewReaderSize(zr, 64<<10)
		}
		first = false
		if f.H > 0 && req.Hashed != nil {
			req.Hashed(f.H)
		}
		if f.D > 0 && req.Reading != nil {
			req.Reading(f.C, f.D)
		}
		if f.Done {
			return model.ErrorFromCode(f.Code, f.Err)
		}
		if f.L == nil {
			continue
		}
		if pending != nil && pending.Dir == f.L.Dir {
			pending.Entries = append(pending.Entries, f.L.Entries...)
			pending.Skipped = append(pending.Skipped, f.L.Skipped...)
		} else {
			pending = f.L
		}
		if f.L.More {
			continue
		}
		pending.More = false
		if err := fn(*pending); err != nil {
			return err
		}
		pending = nil
	}
}

// Open streams a file from the peer.
func (r *Remote) Open(ctx context.Context, path string) (endpoint.Reader, error) {
	return r.openRead(ctx, "read", pathParams{Path: path}, 0)
}

// OpenAt streams a file from offset on. Older peers do not know "readat"
// and answer with model.ErrUnsupported.
func (r *Remote) OpenAt(ctx context.Context, path string, offset int64) (endpoint.Reader, error) {
	return r.openRead(ctx, "readat", readAtParams{Path: path, Offset: offset}, ReplyTimeout)
}

func (r *Remote) openRead(ctx context.Context, method string, params any, reply time.Duration) (endpoint.Reader, error) {
	s, err := r.open(ctx, method, params, compressOn(ctx))
	if err != nil {
		return nil, err
	}
	s.armReply(reply)
	resp, err := s.readResp()
	if err != nil {
		s.close()
		return nil, err
	}
	var e model.Entry
	if err := json.Unmarshal(resp.R, &e); err != nil {
		s.close()
		return nil, s.fix(err)
	}
	rd := &remoteReader{s: s, cr: &proto.ChunkReader{R: s.br}, entry: e}
	if resp.Z {
		if rd.zr, err = newZReader(rd.cr); err != nil {
			s.close()
			return nil, err
		}
	}
	return rd, nil
}

type remoteReader struct {
	s     *stream
	cr    *proto.ChunkReader
	zr    *zReader // set when the peer compresses the data
	entry model.Entry
	hash  string
	err   error
}

func (r *remoteReader) Entry() model.Entry { return r.entry }
func (r *remoteReader) Hash() string       { return r.hash }

func (r *remoteReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if err := r.s.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	r.s.arm()
	var n int
	var err error
	if r.zr != nil {
		n, err = r.zr.Read(p)
		if err == io.EOF {
			err = r.cr.Drain() // up to the terminator
			if err == nil {
				err = io.EOF
			}
		}
	} else {
		n, err = r.cr.Read(p)
	}
	if err == io.EOF {
		var tr trailer
		if terr := proto.ReadFrame(r.s.br, &tr); terr != nil {
			r.err = r.s.fix(terr)
			return n, r.err
		}
		if e := model.ErrorFromCode(tr.Code, tr.Err); e != nil {
			r.err = e
			return n, e
		}
		r.hash = tr.Hash
		r.err = io.EOF
		return n, io.EOF
	}
	if err != nil {
		r.err = r.s.fix(err)
		return n, r.err
	}
	return n, nil
}

func (r *remoteReader) Close() error {
	if r.zr != nil {
		r.zr.Close()
	}
	r.s.close()
	return nil
}

// Create streams a file to the peer. The peer confirms it could create the
// temp file before any data is sent.
func (r *Remote) Create(ctx context.Context, path string, e model.Entry) (endpoint.Writer, error) {
	w, _, err := r.create(ctx, path, e, false)
	return w, err
}

// CreateResumable is Create that continues the partial data of an earlier
// attempt. Older peers do not keep partial data and always report 0 bytes.
func (r *Remote) CreateResumable(ctx context.Context, path string, e model.Entry) (endpoint.Writer, int64, error) {
	return r.create(ctx, path, e, true)
}

func (r *Remote) create(ctx context.Context, path string, e model.Entry, resume bool) (endpoint.Writer, int64, error) {
	s, err := r.open(ctx, "write", writeParams{Path: path, Entry: e, Resume: resume}, compressOn(ctx) && worthCompressing(path))
	if err != nil {
		return nil, 0, err
	}
	if resume {
		s.armReply(ReplyTimeout)
	} else {
		s.arm()
	}
	resp, err := s.readResp()
	if err != nil {
		s.close()
		return nil, 0, err
	}
	var have int64
	if resume && len(resp.R) > 0 {
		if err := json.Unmarshal(resp.R, &have); err != nil {
			s.close()
			return nil, 0, s.fix(err)
		}
	}
	w := &remoteWriter{s: s, cw: &proto.ChunkWriter{W: s.bw}}
	w.out = w.cw
	if resp.Z {
		w.zw = newZWriter(w.cw)
		w.out = w.zw
	}
	return w, have, nil
}

type remoteWriter struct {
	s    *stream
	cw   *proto.ChunkWriter
	zw   *zWriter  // set when the data is compressed
	out  io.Writer // cw or zw
	done bool
}

func (w *remoteWriter) Write(p []byte) (int, error) {
	if err := w.s.ctx.Err(); err != nil {
		return 0, err
	}
	w.s.arm()
	n, err := w.out.Write(p)
	return n, w.s.fix(err)
}

func (w *remoteWriter) Commit(ctx context.Context, expect string) (string, error) {
	if w.done {
		return "", errors.New("rpc: writer already finished")
	}
	w.done = true
	defer w.s.close()
	w.s.arm()
	if w.zw != nil {
		if err := w.zw.Close(); err != nil {
			return "", w.s.fix(err)
		}
	}
	if err := w.cw.Close(); err != nil {
		return "", w.s.fix(err)
	}
	if err := proto.WriteFrame(w.s.bw, trailer{Hash: expect}); err != nil {
		return "", w.s.fix(err)
	}
	if err := w.s.bw.Flush(); err != nil {
		return "", w.s.fix(err)
	}
	w.s.arm()
	var sum string
	err := w.s.readResponse(&sum)
	return sum, err
}

// Abort closes the stream; the peer removes its temp file, or keeps it for
// a resume.
func (w *remoteWriter) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	if w.zw != nil {
		w.zw.release()
	}
	w.s.close()
	return nil
}
