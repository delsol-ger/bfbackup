// Package engine implements the operations on backup data: backup (with
// dry run and protection), verify, restore, delete and index rebuild. It
// only talks to endpoint.Endpoint and the store, so it works the same for
// local, pushed and pulled backups and is tested with local endpoints.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

// Engine runs operations. One Engine is shared by all runs.
type Engine struct {
	Store  *store.Store
	TmpDir string // plan files
	Log    *slog.Logger
	Now    func() time.Time
	// MaxErrors caps the per-file errors kept for one run.
	MaxErrors int
	// Reserve is the free space that must remain on a target.
	Reserve uint64
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// trashBatch names the trash folder for what one run moves to the trash, so
// a run fills one batch however long it takes.
func (e *Engine) trashBatch(runID int64) string {
	return model.TrashBatchName(e.now(), runID)
}

func (e *Engine) maxErrors() int {
	if e.MaxErrors > 0 {
		return e.MaxErrors
	}
	return 1000
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Result is the outcome of a run that did not fail outright.
type Result struct {
	Status  string
	Stats   model.RunStats
	Errors  []store.RunError
	Details map[string]any
	Message string
}

// Progress is updated while a run works and read by the UI.
type Progress struct {
	FilesTotal atomic.Int64
	BytesTotal atomic.Int64
	FilesDone  atomic.Int64
	BytesDone  atomic.Int64
	Scanned    atomic.Int64
	// ScannedBytes is the size of the scanned files.
	ScannedBytes atomic.Int64
	// HashedBytes is what a scan read for checksums so far. It moves while
	// a folder of large files is hashed and no file is counted yet.
	HashedBytes atomic.Int64
	// ScanEstimate is the expected number of files of a scan (from the
	// index); 0 when unknown. Used for the percentage while scanning.
	ScanEstimate atomic.Int64
	// DirsRead is the folders a scan started reading. It moves while a
	// peer walks folders whose listings are not back yet.
	DirsRead atomic.Int64
	// Steps counts small units of work of steps without files or bytes to
	// count (e.g. the files an anomaly check reads).
	Steps atomic.Int64

	mu      sync.Mutex
	phase   string
	since   time.Time
	current map[int]string
	files   map[int]*fileProgress
	waiting string
	phases  []PhaseSpan
	// For IdleSeconds: the counters as last seen and when they last moved.
	mark  int64
	moved time.Time
}

// PhaseSpan is one step of a run, in Unix seconds; End is 0 while it runs.
type PhaseSpan struct {
	Name  string `json:"name"`
	Start int64  `json:"start"`
	End   int64  `json:"end,omitempty"`
}

// maxPhases caps the timeline (retries can repeat steps).
const maxPhases = 100

// fileProgress is the file one transfer worker copies.
type fileProgress struct {
	path  string
	size  int64
	start time.Time
	done  atomic.Int64
}

// NewProgress returns an empty progress tracker.
func NewProgress() *Progress {
	now := time.Now()
	return &Progress{phase: "starting", since: now, moved: now, current: map[int]string{}, files: map[int]*fileProgress{}}
}

// SetPhase names the current step and resets the rate clock.
func (p *Progress) SetPhase(phase string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.phase = phase
	p.since = time.Now()
	p.moved = p.since
	now := p.since.Unix()
	if n := len(p.phases); n > 0 && p.phases[n-1].End == 0 {
		p.phases[n-1].End = now
	}
	if len(p.phases) < maxPhases {
		p.phases = append(p.phases, PhaseSpan{Name: phase, Start: now})
	}
	p.mu.Unlock()
}

// Phases returns the steps so far; the running one ends now.
func (p *Progress) Phases() []PhaseSpan {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]PhaseSpan(nil), p.phases...)
	if n := len(out); n > 0 && out[n-1].End == 0 {
		out[n-1].End = time.Now().Unix()
	}
	return out
}

// SetWaiting says what the run waits for ("" = nothing) and returns what
// it waited for before.
func (p *Progress) SetWaiting(msg string) (was string) {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	was, p.waiting = p.waiting, msg
	return was
}

// reading is ScanRequest.Reading: the folder a scan is reading now.
func (p *Progress) reading(dir string, n int64) {
	if p == nil {
		return
	}
	p.DirsRead.Add(n)
	p.setCurrent(0, "reading "+dirLabel(dir))
}

// whereScan is the heartbeat's part about the folders a scan reads, e.g.
// ", 1200 folders read, now reading folder /Photos".
func (p *Progress) whereScan() string {
	if p == nil {
		return ""
	}
	n := p.DirsRead.Load()
	if n == 0 {
		return ""
	}
	p.mu.Lock()
	cur := p.current[0]
	p.mu.Unlock()
	out := fmt.Sprintf(", %d folders read", n)
	if strings.HasPrefix(cur, "reading ") {
		out += ", now " + cur
	}
	return out
}

// listed shows the folder whose listing arrived, for scans whose source
// does not report the folder it reads (older peers).
func (p *Progress) listed(dir string) {
	if p != nil && p.DirsRead.Load() == 0 {
		p.setCurrent(0, dirLabel(dir))
	}
}

// startFile shows the file a transfer worker copies; endFile clears it.
func (p *Progress) startFile(worker int, path string, size int64) *fileProgress {
	f := &fileProgress{path: path, size: size, start: time.Now()}
	if p == nil {
		return f
	}
	p.mu.Lock()
	p.files[worker] = f
	p.mu.Unlock()
	return f
}

func (p *Progress) endFile(worker int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.files, worker)
	p.mu.Unlock()
}

type fileKey struct{}

// withFile makes the copies under ctx count their bytes for f as well.
func withFile(ctx context.Context, f *fileProgress) context.Context {
	return context.WithValue(ctx, fileKey{}, f)
}

func fileOf(ctx context.Context) *fileProgress {
	f, _ := ctx.Value(fileKey{}).(*fileProgress)
	return f
}

type progressKey struct{}

// withProgress lets helpers deep in a run count Steps.
func withProgress(ctx context.Context, p *Progress) context.Context {
	return context.WithValue(ctx, progressKey{}, p)
}

// stepDone counts one step for the run of ctx, if it has a progress.
func stepDone(ctx context.Context) {
	if p, _ := ctx.Value(progressKey{}).(*Progress); p != nil {
		p.Steps.Add(1)
	}
}

func (p *Progress) setCurrent(worker int, path string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if path == "" {
		delete(p.current, worker)
	} else {
		p.current[worker] = path
	}
	p.mu.Unlock()
}

// Snapshot is a copy of the progress for JSON.
type Snapshot struct {
	Phase        string  `json:"phase"`
	FilesTotal   int64   `json:"files_total"`
	BytesTotal   int64   `json:"bytes_total"`
	FilesDone    int64   `json:"files_done"`
	BytesDone    int64   `json:"bytes_done"`
	Scanned      int64   `json:"scanned"`
	ScannedBytes int64   `json:"scanned_bytes"`
	HashedBytes  int64   `json:"hashed_bytes"`
	DirsRead     int64   `json:"dirs_read"`
	Steps        int64   `json:"steps"`
	BytesPerSec  float64 `json:"bytes_per_sec"`
	// PhaseSeconds is how long the current phase runs.
	PhaseSeconds int64 `json:"phase_seconds"`
	// IdleSeconds is how long no counter has moved (as of the snapshots
	// taken; the UI and the watchdog take one at least every minute).
	IdleSeconds int64 `json:"idle_seconds"`
	// Percent of the current phase, 0-100; -1 when unknown.
	Percent float64 `json:"percent"`
	// EtaSeconds estimates the rest of the current phase; -1 when unknown.
	EtaSeconds int64 `json:"eta_seconds"`
	// Current is what the run works on besides files, e.g. the folder a
	// scan reads.
	Current []string `json:"current"`
	// Files are the files being copied now.
	Files []FileSnapshot `json:"files"`
	// Waiting says what the run waits for, e.g. a peer to reconnect.
	Waiting string      `json:"waiting,omitempty"`
	Phases  []PhaseSpan `json:"phases"`

	// Set by the controller.
	StallSeconds int64  `json:"stall_seconds,omitempty"` // the watchdog stops the run after this idle time; 0 = never
	Latest       string `json:"latest,omitempty"`        // the newest run-level log line
	LatestAt     int64  `json:"latest_at,omitempty"`
}

// FileSnapshot is one file being copied.
type FileSnapshot struct {
	Path        string  `json:"path"`
	Size        int64   `json:"size"`
	Done        int64   `json:"done"`
	Seconds     int64   `json:"seconds"`
	BytesPerSec float64 `json:"bytes_per_sec"`
}

// Snapshot returns the current state.
func (p *Progress) Snapshot() Snapshot {
	if p == nil {
		return Snapshot{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Snapshot{
		Phase:        p.phase,
		FilesTotal:   p.FilesTotal.Load(),
		BytesTotal:   p.BytesTotal.Load(),
		FilesDone:    p.FilesDone.Load(),
		BytesDone:    p.BytesDone.Load(),
		Scanned:      p.Scanned.Load(),
		ScannedBytes: p.ScannedBytes.Load(),
		HashedBytes:  p.HashedBytes.Load(),
		DirsRead:     p.DirsRead.Load(),
		Steps:        p.Steps.Load(),
		Current:      []string{},
		Files:        []FileSnapshot{},
		Waiting:      p.waiting,
		Phases:       append([]PhaseSpan{}, p.phases...),
		Percent:      -1,
		EtaSeconds:   -1,
	}
	now := time.Now()
	if mark := s.Scanned + s.FilesDone + s.BytesDone + s.HashedBytes + s.DirsRead + s.Steps; mark != p.mark {
		p.mark, p.moved = mark, now
	}
	s.IdleSeconds = int64(now.Sub(p.moved).Seconds())
	s.PhaseSeconds = int64(now.Sub(p.since).Seconds())
	el := now.Sub(p.since).Seconds()
	scanning := strings.HasPrefix(p.phase, "scanning")
	frac := -1.0
	switch {
	case scanning:
		if est := p.ScanEstimate.Load(); est > 0 {
			frac = min(float64(s.Scanned)/float64(est), 0.99)
		}
		if el > 1 {
			s.BytesPerSec = float64(s.HashedBytes) / el
		}
	case s.BytesTotal > 0:
		frac = float64(s.BytesDone) / float64(s.BytesTotal)
		if el > 1 {
			s.BytesPerSec = float64(s.BytesDone) / el
		}
	case s.FilesTotal > 0:
		frac = float64(s.FilesDone) / float64(s.FilesTotal)
	}
	if frac >= 0 {
		frac = min(frac, 1)
		s.Percent = frac * 100
		// No estimate while nothing moves: it would only grow with the clock.
		if frac > 0.001 && el > 3 && s.IdleSeconds < 60 {
			s.EtaSeconds = int64(el * (1 - frac) / frac)
		}
	}
	for _, c := range p.current {
		s.Current = append(s.Current, c)
	}
	sort.Strings(s.Current)
	for _, f := range p.files {
		fs := FileSnapshot{Path: f.path, Size: f.size, Done: f.done.Load()}
		d := now.Sub(f.start).Seconds()
		fs.Seconds = int64(d)
		if d > 1 {
			fs.BytesPerSec = float64(fs.Done) / d
		}
		s.Files = append(s.Files, fs)
	}
	sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path })
	return s
}

// heartbeatEvery is how often a long step writes a "still working" line.
var heartbeatEvery = 2 * time.Minute

// heartbeat calls beat every heartbeatEvery until stop is called or ctx
// ends, so long steps without other output still show up in the log.
func heartbeat(ctx context.Context, beat func()) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				beat()
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}

// logTransfer is the heartbeat of a transfer: how far it is and which
// files it copies now.
func (p *Progress) logTransfer(act Activity, node string) {
	s := p.Snapshot()
	msg := fmt.Sprintf("still copying: %d of %d files, %s of %s", s.FilesDone, s.FilesTotal,
		model.HumanBytes(s.BytesDone), model.HumanBytes(s.BytesTotal))
	if s.BytesPerSec > 0 {
		msg += fmt.Sprintf(", %s/s", model.HumanBytes(int64(s.BytesPerSec)))
	}
	if s.EtaSeconds >= 0 {
		msg += fmt.Sprintf(", about %s left", (time.Duration(s.EtaSeconds) * time.Second).String())
	}
	var parts []string
	for i, f := range s.Files {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("%d more", len(s.Files)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%s of %s)", f.Path, model.HumanBytes(f.Done), model.HumanBytes(f.Size)))
	}
	if len(parts) > 0 {
		msg += "; now: " + strings.Join(parts, ", ")
	}
	logf(act, logbook.LevelEvent, node, "", "%s", msg)
}

// dirLabel names a scanned directory for the progress ("" is the root).
func dirLabel(dir string) string { return "folder /" + dir }

// Activity receives what a run does, for the activity log (levels as in
// package logbook). node is where it happened; "" means the run's default.
// A nil Activity logs nothing.
type Activity interface {
	Enabled(level int) bool
	Log(level int, node, path, msg string)
}

// logf formats and logs only when the level is enabled, so file-level
// messages cost nothing when they are not kept.
func logf(a Activity, level int, node, path, format string, args ...any) {
	if a == nil || !a.Enabled(level) {
		return
	}
	a.Log(level, node, path, fmt.Sprintf(format, args...))
}

// errorList collects per-file errors up to a cap. Every error is also
// logged, even beyond the cap.
type errorList struct {
	mu    sync.Mutex
	max   int
	list  []store.RunError
	count int64
	act   Activity
	node  string // where errors happen unless add says otherwise; "" = not known
}

func (l *errorList) add(path string, err error) { l.addAt(l.node, path, err) }

// addAt records an error that happened on node.
func (l *errorList) addAt(node, path string, err error) {
	logf(l.act, logbook.FileError, node, path, "%s", err.Error())
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++
	if len(l.list) < l.max {
		l.list = append(l.list, store.RunError{Path: path, Message: err.Error(), Node: node})
	}
}

func (l *errorList) addMsg(path, msg string) { l.add(path, errors.New(msg)) }

func (l *errorList) items() []store.RunError {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]store.RunError(nil), l.list...)
}

// countingWriter adds written bytes to the progress, to the file's
// progress (f) and to n.
type countingWriter struct {
	w io.Writer
	p *Progress
	n *int64
	f *fileProgress
}

func (c countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.count(int64(n))
	return n, err
}

func (c countingWriter) count(n int64) {
	if c.p != nil {
		c.p.BytesDone.Add(n)
	}
	if c.f != nil {
		c.f.done.Add(n)
	}
	if c.n != nil {
		*c.n += n
	}
}

// How copies ride out a dropped or stalled connection.
var (
	// copyAttempts: tries per file when the connection breaks.
	copyAttempts = 3
	// reconnectWait: how long a file waits for a dropped peer before the
	// run gives up on that peer.
	reconnectWait = 10 * time.Minute
	// resumeMin: files at least this large continue where an interrupted
	// attempt stopped instead of starting over.
	resumeMin int64 = 64 << 20
	// scanAttempts: tries of a scan cut off by a drop.
	scanAttempts = 3
)

// onlineWaiter is implemented by endpoints of peers that can reconnect.
type onlineWaiter interface {
	WaitOnline(ctx context.Context, max time.Duration) error
}

// waitOnline waits until all eps are reachable again, or returns an error
// wrapping model.ErrOffline when a peer stays away for reconnectWait.
func waitOnline(ctx context.Context, eps ...endpoint.Endpoint) error {
	for _, ep := range eps {
		if w, ok := ep.(onlineWaiter); ok {
			if err := w.WaitOnline(ctx, reconnectWait); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyFile streams one regular file from src to dst and returns the entry
// as written plus its SHA-256. When meta is set, its mtime and mode are
// written instead of the source file's (restores use the values recorded at
// backup time, because targets may store coarser timestamps).
func copyFile(ctx context.Context, src, dst endpoint.Endpoint, srcPath, dstPath string, buf []byte, p *Progress, meta *model.Entry) (model.Entry, string, error) {
	return copyFileHint(ctx, src, dst, srcPath, dstPath, buf, p, meta, meta)
}

// copyFileHint is copyFile with what the file is expected to look like
// (the scan's or the index's entry, may be nil). A large file with a hint
// continues the partial data of an earlier, interrupted attempt.
//
// When the connection to a peer breaks or stalls, copyFileHint waits for
// the peer to come back and tries again, up to copyAttempts times. If a
// peer stays away, the error wraps model.ErrOffline (see offline).
func copyFileHint(ctx context.Context, src, dst endpoint.Endpoint, srcPath, dstPath string, buf []byte, p *Progress, meta, hint *model.Entry) (model.Entry, string, error) {
	for attempt := 1; ; attempt++ {
		ent, sum, resumed, err := copyOnce(ctx, src, dst, srcPath, dstPath, buf, p, meta, hint)
		switch {
		case err == nil || ctx.Err() != nil || attempt >= copyAttempts:
			return ent, sum, err
		case errors.Is(err, model.ErrHashMismatch) && resumed && attempt == 1:
			// The kept part was bad; it is gone now, start over once.
		case model.IsConnError(err):
			if werr := waitOnline(ctx, src, dst); werr != nil {
				return ent, sum, werr
			}
		default:
			return ent, sum, err
		}
	}
}

// copyOnce is one attempt of copyFileHint. resumed tells whether it
// continued partial data.
func copyOnce(ctx context.Context, src, dst endpoint.Endpoint, srcPath, dstPath string, buf []byte, p *Progress, meta, hint *model.Entry) (ent model.Entry, sum string, resumed bool, err error) {
	var counted int64
	cw := countingWriter{p: p, n: &counted, f: fileOf(ctx)}
	defer func() {
		if err != nil {
			cw.count(-counted) // a retry counts its bytes again
		}
	}()

	sr, sok := src.(endpoint.Resumer)
	dr, dok := dst.(endpoint.Resumer)
	if sok && dok && hint != nil && hint.Size >= resumeMin {
		ent, sum, resumed, err, done := copyResumable(ctx, sr, dr, srcPath, dstPath, buf, cw, meta, hint)
		if done {
			return ent, sum, resumed, err
		}
	}

	r, err := src.Open(ctx, srcPath)
	if err != nil {
		return model.Entry{}, "", false, err
	}
	defer r.Close()
	ent = r.Entry()
	if meta != nil {
		ent.MTime, ent.Mode = meta.MTime, meta.Mode
	}
	w, err := dst.Create(ctx, dstPath, ent)
	if err != nil {
		return ent, "", false, err
	}
	cw.w = w
	if _, err := io.CopyBuffer(cw, ctxReader{ctx: ctx, r: r}, buf); err != nil {
		w.Abort()
		return ent, "", false, err
	}
	sum, err = w.Commit(ctx, r.Hash())
	return ent, sum, false, err
}

// copyResumable copies a large file and keeps what arrived if the attempt
// breaks. done is false when the plain copy should be used instead: the
// source changed since the hint was taken, or a peer is too old to resume.
func copyResumable(ctx context.Context, src, dst endpoint.Resumer, srcPath, dstPath string, buf []byte, cw countingWriter, meta, hint *model.Entry) (ent model.Entry, sum string, resumed bool, err error, done bool) {
	ent = *hint
	ent.Type = model.TypeFile
	if meta != nil {
		ent.MTime, ent.Mode = meta.MTime, meta.Mode
	}
	w, have, err := dst.CreateResumable(ctx, dstPath, ent)
	if err != nil {
		return ent, "", false, err, true
	}
	r, err := src.OpenAt(ctx, srcPath, have)
	if err != nil {
		w.Abort()
		if errors.Is(err, model.ErrUnsupported) || errors.Is(err, model.ErrChanged) {
			return ent, "", false, nil, false
		}
		return ent, "", false, err, true
	}
	defer r.Close()
	got := r.Entry()
	if got.Size != hint.Size || (meta == nil && got.MTime != hint.MTime) {
		w.Abort() // changed since the scan: copy it as it is now
		return ent, "", false, nil, false
	}
	cw.w = w
	cw.count(have)
	if _, err := io.CopyBuffer(cw, ctxReader{ctx: ctx, r: r}, buf); err != nil {
		w.Abort()
		return ent, "", have > 0, err, true
	}
	sum, err = w.Commit(ctx, r.Hash())
	return ent, sum, have > 0, err, true
}

// offlineError ends a run whose peer stayed away longer than reconnectWait.
type offlineError struct{ err error }

func (e *offlineError) Error() string {
	return e.err.Error() + "; the run was stopped. Files copied so far are kept, the next run continues with the rest"
}
func (e *offlineError) Unwrap() error { return e.err }

type guardKey struct{}

// guardOffline returns a context for the workers of a run that offline
// cancels when a peer is given up. Call stop when the workers are done.
func guardOffline(ctx context.Context) (gctx context.Context, stop func()) {
	gctx, cancel := context.WithCancelCause(ctx)
	return context.WithValue(gctx, guardKey{}, cancel), func() { cancel(nil) }
}

// offline reports whether err means a peer was given up as offline. If so,
// it stops the run guarded by ctx, so the remaining files do not each fail
// with the same error; offlineErr then returns the reason.
func offline(ctx context.Context, err error) bool {
	if !errors.Is(err, model.ErrOffline) {
		return false
	}
	if cancel, ok := ctx.Value(guardKey{}).(context.CancelCauseFunc); ok {
		cancel(&offlineError{err})
	}
	return true
}

// offlineErr returns why a guarded run was stopped by offline, or nil.
func offlineErr(ctx context.Context) error {
	var oe *offlineError
	if errors.As(context.Cause(ctx), &oe) {
		return oe
	}
	return nil
}

// retryScan runs scan, and again after src reconnects if the connection
// broke during it (up to scanAttempts). scan must start from scratch each
// time: reset everything a previous attempt collected.
func retryScan(ctx context.Context, src endpoint.Endpoint, scan func() error) error {
	for attempt := 1; ; attempt++ {
		err := scan()
		if err == nil || !model.IsConnError(err) || ctx.Err() != nil || attempt >= scanAttempts {
			return err
		}
		if werr := waitOnline(ctx, src); werr != nil {
			return werr
		}
	}
}

// ctxReader stops a copy as soon as ctx ends, also in the middle of a
// large file.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// runPool runs fn for every item from next with n workers. next returns
// false when there are no more items. It stops early when ctx ends.
func runPool[T any](ctx context.Context, n int, items <-chan T, fn func(worker int, item T)) {
	if n <= 0 {
		n = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for it := range items {
				if ctx.Err() != nil {
					continue // drain
				}
				fn(worker, it)
			}
		}(i)
	}
	wg.Wait()
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

func describe(err error) error {
	switch {
	case errors.Is(err, model.ErrChanged):
		return fmt.Errorf("file changed while copying, will retry next run: %w", err)
	case errors.Is(err, model.ErrHashMismatch):
		return fmt.Errorf("data arrived corrupted (checksum mismatch): %w", err)
	case errors.Is(err, model.ErrUnsupported):
		return fmt.Errorf("this node runs an older BFBackup version that cannot do this; update it to the controller's version: %w", err)
	}
	return err
}

func finalStatus(failed int64) string {
	if failed > 0 {
		return store.StatusWarning
	}
	return store.StatusSuccess
}
