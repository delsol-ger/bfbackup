package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/logbook"
	"bfbackup/internal/match"
	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

// BackupSpec describes one backup or dry run.
type BackupSpec struct {
	RunID    int64
	Job      store.Job
	Target   store.Target
	Src, Dst endpoint.Endpoint
	DryRun   bool
	Approved bool // the user continued after a question: skip the checks and the "ask" stop
	Progress *Progress
	Log      Activity
	// Guards are the anomaly checks (nil: none). History are the stats of
	// the last backups of this target, newest first, for the history check.
	Guards  *Guards
	History []model.RunStats
}

// planItem is one file to copy.
type planItem struct {
	Dir     string      `json:"d"`
	Entry   model.Entry `json:"e"`
	Changed bool        `json:"c,omitempty"`
	OldSize int64       `json:"o,omitempty"`
}

// PlanSample is shown to the user for dry runs and approvals.
type PlanSample struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Reason string `json:"reason"`
}

const sampleSize = 500

// changed decides whether a source entry differs from the index.
func changed(mode string, src model.Entry, idx store.IndexEntry) bool {
	if src.Type != idx.Type {
		return true
	}
	if src.Type == model.TypeSymlink {
		return src.Link != idx.Link
	}
	switch mode {
	case model.CompareSize:
		return src.Size != idx.Size
	case model.CompareChecksum:
		return src.Size != idx.Size || src.Hash == "" || src.Hash != idx.Hash
	default:
		// Tolerance for file systems with coarse times (FAT/exFAT targets,
		// files adopted from a target).
		return src.Size != idx.Size || abs64(src.MTime-idx.MTime) > mtimeTolerance
	}
}

// adoptExisting fills an empty index from the files already in the target
// folder (copied there by hand or by another tool), once. The normal
// comparison then decides with the job's compare mode: files that match are
// not copied again, the others are replaced. It returns how many files the
// target had.
func (e *Engine) adoptExisting(ctx context.Context, spec BackupSpec, prog *Progress) (int64, error) {
	tgt := spec.Target
	if n, _, err := e.Store.CountPrefix(ctx, tgt.ID, ""); err != nil || n > 0 {
		return 0, err
	}
	prog.SetPhase("checking the target for existing files")
	hash := spec.Job.CompareMode == model.CompareChecksum
	if hash {
		logf(spec.Log, logbook.LevelEvent, tgt.NodeID, "", "first backup to this target: checking which files %s already has. The job compares by checksum, so the target reads every file there once; with much data this takes a long time", tgt.Path)
	} else {
		logf(spec.Log, logbook.LevelEvent, tgt.NodeID, "", "first backup to this target: checking which files %s already has", tgt.Path)
	}
	iw := e.Store.NewIndexWriter(tgt.ID)
	now := e.now().Unix()
	var n, bytes atomic.Int64
	// A sign of life in the log, also while one large folder is hashed.
	began := time.Now()
	stopBeat := heartbeat(ctx, func() {
		read := ""
		if hash {
			read = fmt.Sprintf(", %s read for checksums", model.HumanBytes(prog.HashedBytes.Load()))
		}
		logf(spec.Log, logbook.LevelEvent, tgt.NodeID, "", "still checking the target: %d files (%s) so far%s, %.0f files/s%s", n.Load(), model.HumanBytes(bytes.Load()), read, float64(n.Load())/time.Since(began).Seconds(), prog.whereScan())
	})
	req := model.ScanRequest{Root: tgt.Path, Hash: hash, Hashed: func(n int64) { prog.HashedBytes.Add(n) }, Reading: prog.reading}
	err := spec.Dst.Scan(ctx, req, func(l model.DirListing) error {
		prog.listed(l.Dir)
		for _, ent := range l.Entries {
			if ent.Type == model.TypeDir {
				continue
			}
			n.Add(1)
			bytes.Add(ent.Size)
			prog.Scanned.Add(1)
			prog.ScannedBytes.Add(ent.Size)
			if err := iw.Put(ctx, store.IndexEntry{Dir: l.Dir, Name: ent.Name, Type: ent.Type, Size: ent.Size,
				MTime: ent.MTime, Mode: ent.Mode, Hash: ent.Hash, Link: ent.Link, SyncedAt: now}); err != nil {
				return err
			}
			logf(spec.Log, logbook.FileTrace, tgt.NodeID, model.JoinRel(l.Dir, ent.Name), "already on the target (%s): taken into the index, compared with the source in the scan", model.HumanBytes(ent.Size))
		}
		return nil
	})
	stopBeat()
	prog.setCurrent(0, "")
	if cerr := iw.Close(context.WithoutCancel(ctx)); err == nil {
		err = cerr
	}
	if isNotExist(err) {
		logf(spec.Log, logbook.LevelEvent, tgt.NodeID, "", "the target folder does not exist yet; it is created by the first backup")
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("checking the target for existing files: %w", err)
	}
	prog.Scanned.Store(0)
	prog.ScannedBytes.Store(0)
	prog.DirsRead.Store(0)
	prog.HashedBytes.Store(0)
	if n.Load() == 0 {
		logf(spec.Log, logbook.LevelEvent, tgt.NodeID, "", "the target folder is empty")
		return 0, nil
	}
	e.Store.RefreshTargetTotals(ctx, tgt.ID)
	how := map[string]string{model.CompareSize: "size", model.CompareChecksum: "checksum"}[spec.Job.CompareMode]
	if how == "" {
		how = "size and modification time"
	}
	logf(spec.Log, logbook.LevelEvent, tgt.NodeID, "", "the target already has %d files (%s) that BFBackup did not copy; files that match the source by %s are taken over instead of copied again", n.Load(), model.HumanBytes(bytes.Load()), how)
	return n.Load(), nil
}

// logNotInSource lists files that are in the backup but were not seen in
// this scan (deleted, moved or now excluded on the source). They stay in
// the backup; the log says so for each one, up to maxIssueLines.
func (e *Engine) logNotInSource(ctx context.Context, act Activity, targetID, runID, count int64) {
	if count == 0 || act == nil || !act.Enabled(logbook.FileIssue) {
		return
	}
	n := 0
	err := e.Store.WalkNotSeen(ctx, targetID, runID, func(ie store.IndexEntry) error {
		if n++; n > maxIssueLines {
			return store.ErrStopWalk
		}
		act.Log(logbook.FileIssue, "", ie.Rel(), "in the backup, but no longer in the source (deleted, moved or now excluded there). Kept in the backup; delete it under Browse → Not in source if it is not needed")
		return nil
	})
	if err != nil {
		logf(act, logbook.LevelError, "", "", "listing files not in source failed: %v", err)
		return
	}
	if count > maxIssueLines {
		logf(act, logbook.LevelEvent, "", "", "%d files not in source; the file log lists the first %d", count, maxIssueLines)
	}
}

func hookEnv(spec BackupSpec, phase string) map[string]string {
	return map[string]string{
		"BFB_PHASE":       phase,
		"BFB_JOB":         spec.Job.Name,
		"BFB_JOB_ID":      strconv.FormatInt(spec.Job.ID, 10),
		"BFB_RUN_ID":      strconv.FormatInt(spec.RunID, 10),
		"BFB_SOURCE_PATH": spec.Job.SourcePath,
		"BFB_TARGET_NODE": spec.Target.NodeID,
		"BFB_TARGET_PATH": spec.Target.Path,
	}
}

func runHook(ctx context.Context, ep endpoint.Endpoint, name string, env map[string]string) (model.HookResult, error) {
	res, err := ep.RunHook(ctx, name, env)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("hook %s exited with code %d", name, res.ExitCode)
	}
	return res, nil
}

// removal is a file a backup takes off the target.
type removal struct {
	rel  string
	sync bool // gone from the source (sync mode); else it matches the exclusions
}

// planRemovals lists what this backup removes from the target: files that
// match the exclusions (with "remove excluded files") and, in sync mode,
// every other file that is gone from the source. Both are among the files
// this scan did not see.
func (e *Engine) planRemovals(ctx context.Context, spec BackupSpec) ([]removal, error) {
	job, tgt := spec.Job, spec.Target
	if !job.CleanExcluded && job.Mode != store.ModeSync {
		return nil, nil
	}
	m, err := match.New(job.Excludes)
	if err != nil {
		return nil, err
	}
	var out []removal
	err = e.Store.WalkNotSeen(ctx, tgt.ID, spec.RunID, func(ie store.IndexEntry) error {
		switch rel := ie.Rel(); {
		case job.CleanExcluded && m.ExcludedPath(rel, false):
			out = append(out, removal{rel: rel})
		case job.Mode == store.ModeSync:
			out = append(out, removal{rel: rel, sync: true})
		}
		return nil
	})
	return out, err
}

// removeBatch is how many removed files remove drops from the index at once.
const removeBatch = 500

// remove takes the planned files off the target, to its trash or for good
// (the job's "old copies" setting), and out of the index.
func (e *Engine) remove(ctx context.Context, spec BackupSpec, prog *Progress, list []removal, res *Result, errs *errorList) error {
	job, tgt, st := spec.Job, spec.Target, &res.Stats
	batch := e.trashBatch(spec.RunID)
	prog.FilesTotal.Store(int64(len(list)))
	prog.FilesDone.Store(0)
	defer prog.setCurrent(0, "")
	stopBeat := heartbeat(ctx, func() {
		logf(spec.Log, logbook.LevelEvent, tgt.NodeID, "", "still removing files from the target: %d of %d done", prog.FilesDone.Load(), len(list))
	})
	defer stopBeat()
	// The removed files leave the index in batches: one transaction per file
	// costs a commit each. Files removed but not yet dropped from the index
	// when the process dies count as already gone on the next backup.
	var gone []string
	flush := func() error {
		if len(gone) == 0 {
			return nil
		}
		_, err := e.Store.DeletePrefixes(context.WithoutCancel(ctx), tgt.ID, gone)
		gone = gone[:0]
		return err
	}
	for _, r := range list {
		prog.setCurrent(0, "removing "+r.rel)
		prog.FilesDone.Add(1)
		why := "matches the exclusions"
		if r.sync {
			why = "gone from the source (sync)"
		}
		var err error
		if job.OldCopies == store.OldDelete {
			err = spec.Dst.Remove(ctx, tgt.Path, r.rel)
		} else {
			err = spec.Dst.Trash(ctx, tgt.Path, r.rel, batch)
		}
		switch {
		case err == nil && job.OldCopies == store.OldDelete:
			st.Deleted++
			logf(spec.Log, logbook.FileOK, "", r.rel, "%s: deleted on the target", why)
		case err == nil:
			st.Trashed++
			logf(spec.Log, logbook.FileOK, "", r.rel, "%s: moved to the trash on the target", why)
		case isNotExist(err):
			logf(spec.Log, logbook.FileIssue, "", r.rel, "%s and already gone from the target: removed from the index", why)
		default:
			st.Failed++
			errs.add(r.rel, fmt.Errorf("%s, but removing it from the target failed: %w", why, describe(err)))
			continue
		}
		if r.sync {
			st.Removed++
		}
		if gone = append(gone, r.rel); len(gone) >= removeBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	e.Store.PruneEmptyDirs(ctx, tgt.ID)
	return nil
}

// compareReason says why a file counts as unchanged or changed, for the
// detail log level.
func compareReason(mode string, src model.Entry, idx store.IndexEntry, isChanged bool) string {
	if !isChanged {
		switch mode {
		case model.CompareSize:
			return "same size as in the backup"
		case model.CompareChecksum:
			return "same checksum as in the backup"
		}
		return "same size and modification time as in the backup"
	}
	var d []string
	if src.Type != idx.Type {
		d = append(d, fmt.Sprintf("now a %s, was a %s", src.Type, idx.Type))
	}
	if src.Type == model.TypeSymlink && src.Link != idx.Link {
		d = append(d, "symlink target differs")
	}
	if src.Size != idx.Size {
		d = append(d, fmt.Sprintf("size %d → %d bytes", idx.Size, src.Size))
	}
	if mode == model.CompareMTime && src.MTime != idx.MTime {
		d = append(d, fmt.Sprintf("modification time %s → %s", fmtNano(idx.MTime), fmtNano(src.MTime)))
	}
	if mode == model.CompareChecksum && src.Hash != idx.Hash {
		d = append(d, "checksum differs")
	}
	return strings.Join(d, ", ")
}

func fmtNano(ns int64) string { return time.Unix(0, ns).Format("2006-01-02 15:04:05") }

// Backup scans the source, compares it with the index and copies new and
// changed files. In copy mode files that disappeared from the source stay
// on the target; in sync mode they are removed. Before anything changes on
// the target, the anomaly checks run and the job's "ask" setting applies:
// then the run stops with StatusApproval until the user continues it.
func (e *Engine) Backup(ctx context.Context, spec BackupSpec) (res *Result, err error) {
	job, tgt, prog := spec.Job, spec.Target, spec.Progress
	if prog == nil {
		prog = NewProgress()
	}
	ctx = withProgress(ctx, prog)
	res = &Result{Details: map[string]any{}}
	st := &res.Stats
	trace := func(format string, args ...any) { logf(spec.Log, logbook.LevelTrace, "", "", format, args...) }

	if spec.Job.PreHook != "" && !spec.DryRun {
		prog.SetPhase("pre-hook")
		hr, err := runHook(ctx, spec.Src, job.PreHook, hookEnv(spec, "pre"))
		res.Details["pre_hook_output"] = hr.Output
		if err != nil {
			return res, fmt.Errorf("pre-hook failed, backup skipped: %w", err)
		}
		logf(spec.Log, logbook.LevelEvent, job.SourceNode, "", "pre-hook %s finished", job.PreHook)
	}
	// The post-hook runs whatever happens after a successful pre-hook.
	if spec.Job.PostHook != "" && !spec.DryRun {
		defer func() {
			status := store.StatusFailed
			if err == nil {
				status = res.Status
			}
			env := hookEnv(spec, "post")
			env["BFB_RUN_STATUS"] = status
			hr, herr := runHook(context.WithoutCancel(ctx), spec.Src, job.PostHook, env)
			res.Details["post_hook_output"] = hr.Output
			if herr != nil {
				logf(spec.Log, logbook.LevelError, job.SourceNode, "", "post-hook failed: %v", herr)
			} else {
				logf(spec.Log, logbook.LevelEvent, job.SourceNode, "", "post-hook %s finished", job.PostHook)
			}
			if herr != nil && err == nil {
				res.Message = "post-hook failed: " + herr.Error()
				if res.Status == store.StatusSuccess {
					res.Status = store.StatusWarning
				}
			}
		}()
	}

	// ---- files already on the target ----
	adoptable, err := e.adoptExisting(ctx, spec, prog)
	if err != nil {
		return res, err
	}
	if adoptable > 0 {
		tgt.StoredFiles = adoptable
	}

	// ---- scan and diff ----
	prog.ScanEstimate.Store(tgt.StoredFiles)
	prog.SetPhase("scanning")
	trace("scanning %s (compare by %s, %s mode, existing files: %s)", job.SourcePath, job.CompareMode, job.Mode, job.OnConflict)
	if err := os.MkdirAll(e.TmpDir, 0o700); err != nil {
		return res, err
	}
	planPath := filepath.Join(e.TmpDir, fmt.Sprintf("plan-%d.jsonl", spec.RunID))
	pf, err := os.OpenFile(planPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return res, err
	}
	defer func() {
		pf.Close()
		os.Remove(planPath)
	}()
	pw := bufio.NewWriterSize(pf, 1<<20)
	enc := json.NewEncoder(pw)

	iw := e.Store.NewIndexWriter(tgt.ID)
	var sample, changedSample []PlanSample
	var changedOldBytes, toCopy, toCopyBytes int64
	specials := 0
	facts := newScanFacts()
	future := e.now().Add(24 * time.Hour).UnixNano()
	keep := job.OnConflict == store.ConflictKeep

	scanReq := model.ScanRequest{
		Root:     job.SourcePath,
		Includes: job.Includes,
		Excludes: job.Excludes,
		Hash:     job.CompareMode == model.CompareChecksum,
		Hashed:   func(n int64) { prog.HashedBytes.Add(n) },
		Reading:  prog.reading,
	}
	// Where the scan time goes, for the heartbeat: the rest is waiting for
	// the source to list (and hash) its files.
	var spentIndex, spentWork atomic.Int64
	scanDir := func(l model.DirListing) error {
		began := time.Now()
		defer func() { spentWork.Add(int64(time.Since(began))) }()
		prog.listed(l.Dir)
		idx, err := e.Store.DirIndex(ctx, tgt.ID, l.Dir)
		spentIndex.Add(int64(time.Since(began)))
		if err != nil {
			return err
		}
		for _, name := range l.Skipped {
			if specials++; specials <= maxIssueLines {
				logf(spec.Log, logbook.FileIssue, job.SourceNode, model.JoinRel(l.Dir, name), "skipped: a device, socket or FIFO on the source; these are never backed up")
			}
		}
		present := map[string]bool{}
		for _, ent := range l.Entries {
			present[ent.Name] = true
		}
		for name := range idx {
			facts.knownExt[ext(name)] = true
		}
		for _, ent := range l.Entries {
			if ent.Type == model.TypeDir {
				continue
			}
			rel := model.JoinRel(l.Dir, ent.Name)
			st.Scanned++
			st.ScannedBytes += ent.Size
			prog.Scanned.Add(1)
			prog.ScannedBytes.Add(ent.Size)
			if ent.MTime > future {
				facts.future++
				facts.futureEg = addExample(facts.futureEg, rel)
			}
			old, known := idx[ent.Name]
			var item *planItem
			switch {
			case !known:
				st.New++
				st.NewBytes += ent.Size
				item = &planItem{Dir: l.Dir, Entry: ent}
				// a.docx → a.docx.locked: the old name is gone, a longer one is new.
				if stem := strings.TrimSuffix(ent.Name, path.Ext(ent.Name)); stem != ent.Name {
					if _, was := idx[stem]; was && !present[stem] {
						facts.renamed++
						facts.renamedEg = addExample(facts.renamedEg, rel)
					}
				}
				// Only in folders the backup already had: a new folder of
				// photos is not suspicious, renamed documents in an old one are.
				if e := ext(ent.Name); e != "" && len(idx) > 0 && !facts.knownExt[e] {
					facts.newExt[e]++
					if facts.newExtEg[e] == "" {
						facts.newExtEg[e] = rel
					}
				}
				logf(spec.Log, logbook.FileTrace, "", rel, "new: not in the backup yet, will be copied")
			case changed(job.CompareMode, ent, old):
				st.Changed++
				st.ChangedBytes += ent.Size
				changedOldBytes += old.Size
				if ent.Size == 0 && old.Size > 0 {
					facts.truncated++
					facts.truncEg = addExample(facts.truncEg, rel)
				}
				if ent.Type == model.TypeFile && knownType(ent.Name) && len(facts.candidates) < 1000 {
					facts.candidates = append(facts.candidates, rel)
				}
				if len(changedSample) < sampleSize {
					changedSample = append(changedSample, PlanSample{Path: rel, Size: ent.Size, Reason: "changed"})
				}
				why := compareReason(job.CompareMode, ent, old, true)
				if keep {
					st.Kept++
					logf(spec.Log, logbook.FileIssue, "", rel, "changed (%s), but the job never replaces existing files: the target keeps its copy", why)
				} else {
					item = &planItem{Dir: l.Dir, Entry: ent, Changed: true, OldSize: old.Size}
					logf(spec.Log, logbook.FileTrace, "", rel, "changed (%s): will be copied", why)
				}
			default:
				st.Unchanged++
				logf(spec.Log, logbook.FileTrace, "", rel, "unchanged (%s): not copied", compareReason(job.CompareMode, ent, old, false))
			}
			if known {
				t := time.Now()
				err := iw.Seen(ctx, l.Dir, ent.Name, spec.RunID)
				spentIndex.Add(int64(time.Since(t)))
				if err != nil {
					return err
				}
			}
			if item != nil {
				toCopy++
				toCopyBytes += ent.Size
				if err := enc.Encode(item); err != nil {
					return err
				}
				if len(sample) < sampleSize {
					reason := "new"
					if item.Changed {
						reason = "changed"
					}
					sample = append(sample, PlanSample{Path: rel, Size: ent.Size, Reason: reason})
				}
			}
		}
		return nil
	}
	how := "size and modification time"
	if scanReq.Hash {
		how = "checksum: every file is read on the source"
	} else if job.CompareMode == model.CompareSize {
		how = "size"
	}
	logf(spec.Log, logbook.LevelEvent, job.SourceNode, "", "scanning the source %s and comparing it with the backup (by %s)", job.SourcePath, how)
	// The progress counters, not st: the heartbeat runs on its own goroutine.
	scanStart := time.Now()
	stopBeat := heartbeat(ctx, func() {
		el := time.Since(scanStart)
		files := prog.Scanned.Load()
		idx, work := time.Duration(spentIndex.Load()), time.Duration(spentWork.Load())
		pct := func(d time.Duration) int { return int(100 * d / el) }
		read := ""
		if scanReq.Hash {
			read = fmt.Sprintf(", %s read for checksums", model.HumanBytes(prog.HashedBytes.Load()))
		}
		logf(spec.Log, logbook.LevelEvent, job.SourceNode, "", "still scanning the source: %d files (%s) so far%s, %.0f files/s%s. Time spent: %d%% waiting for the source to list%s its files, %d%% in the backup index, %d%% comparing and logging",
			files, model.HumanBytes(prog.ScannedBytes.Load()), read, float64(files)/el.Seconds(), prog.whereScan(),
			pct(el-work), map[bool]string{true: " and read"}[scanReq.Hash], pct(idx), pct(work-idx))

	})
	scanAttempt := 0
	scanErr := retryScan(ctx, spec.Src, func() error {
		if scanAttempt++; scanAttempt > 1 {
			// The connection broke during the scan: start it over.
			logf(spec.Log, logbook.LevelEvent, job.SourceNode, "", "the connection to the source broke during the scan; scanning again")
			if err := iw.Close(ctx); err != nil {
				return err
			}
			iw = e.Store.NewIndexWriter(tgt.ID)
			pw.Reset(pf)
			if err := pf.Truncate(0); err != nil {
				return err
			}
			if _, err := pf.Seek(0, 0); err != nil {
				return err
			}
			st.Scanned, st.ScannedBytes, st.New, st.NewBytes = 0, 0, 0, 0
			st.Changed, st.ChangedBytes, st.Unchanged, st.Kept = 0, 0, 0, 0
			prog.Scanned.Store(0)
			prog.ScannedBytes.Store(0)
			prog.HashedBytes.Store(0)
			prog.DirsRead.Store(0)
			sample, changedSample = nil, nil
			changedOldBytes, toCopy, toCopyBytes = 0, 0, 0
			specials = 0
			facts = newScanFacts()
		}
		return spec.Src.Scan(ctx, scanReq, scanDir)
	})
	stopBeat()
	prog.setCurrent(0, "")
	prog.SetPhase("saving the scan to the index")
	if err := iw.Close(ctx); err != nil && scanErr == nil {
		scanErr = err
	}
	if scanErr != nil {
		return res, fmt.Errorf("scanning source: %w", scanErr)
	}
	if err := pw.Flush(); err != nil {
		return res, err
	}
	prog.SetPhase("comparing with the backup")
	if st.Scanned == 0 && tgt.StoredFiles > 0 {
		return res, fmt.Errorf("the source has no files but the backup has %d; is the source disk or share mounted? Nothing was changed", tgt.StoredFiles)
	}
	if err := e.Store.SetTargetScan(ctx, tgt.ID, spec.RunID); err != nil {
		return res, err
	}
	if st.NotInSource, err = e.Store.CountNotSeen(ctx, tgt.ID, spec.RunID); err != nil {
		return res, err
	}
	if st.Gone, err = e.Store.CountGone(ctx, tgt.ID, tgt.LastScanRun, spec.RunID); err != nil {
		return res, err
	}
	if adoptable > 0 {
		st.Adopted = st.Unchanged // matched files that were already there
	}
	res.Details["plan_sample"] = sample
	logf(spec.Log, logbook.LevelEvent, job.SourceNode, "", "scan finished: %d files (%s), %d new, %d changed, %d unchanged, %d not in source",
		st.Scanned, model.HumanBytes(st.ScannedBytes), st.New, st.Changed, st.Unchanged, st.NotInSource)
	if specials > maxIssueLines {
		logf(spec.Log, logbook.LevelEvent, job.SourceNode, "", "%d devices, sockets or FIFOs skipped; the file log lists the first %d", specials, maxIssueLines)
	}
	if job.CleanExcluded || job.Mode == store.ModeSync {
		prog.SetPhase("listing files to remove from the target")
	}
	removals, err := e.planRemovals(ctx, spec)
	if err != nil {
		return res, err
	}
	if len(removals) > 0 {
		// Shown for dry runs and questions, so the user sees what would go.
		var rs []PlanSample
		for _, r := range removals {
			if len(rs) == sampleSize {
				break
			}
			reason := "excluded"
			if r.sync {
				reason = "gone from source"
			}
			rs = append(rs, PlanSample{Path: r.rel, Reason: reason})
		}
		res.Details["removal_sample"] = rs
		res.Details["removals"] = len(removals)
	}
	trace("plan: copy %d files (%s), remove %d files from the target, %d disappeared from the source since the last backup",
		toCopy, model.HumanBytes(toCopyBytes), len(removals), st.Gone)

	// ---- checks and questions ----
	prog.SetPhase("anomaly checks")
	if !spec.Approved {
		anomalies := spec.Guards.check(ctx, *st, st.Gone, int64(len(removals)), tgt.StoredFiles, spec.History, facts, spec.Src, job.SourcePath)
		ask := job.OnConflict == store.ConflictAsk && st.Changed > 0
		switch {
		case len(anomalies) > 0 && spec.DryRun:
			res.Details["anomalies"] = anomalies
		case len(anomalies) > 0:
			var msgs []string
			for _, a := range anomalies {
				msgs = append(msgs, a.Message)
				logf(spec.Log, logbook.LevelError, "", "", "anomaly (%s): %s", a.Check, a.Message)
			}
			res.Status = store.StatusApproval
			res.Message = "Unusual changes, the backup stopped before changing anything: " + strings.Join(msgs, "; ") + ". Continue only if this is expected."
			res.Details["decision"] = map[string]any{"kind": "anomaly", "anomalies": anomalies}
			return res, nil
		case ask && !spec.DryRun:
			res.Status = store.StatusApproval
			res.Message = fmt.Sprintf("%d files that already exist on the target changed. The job asks before replacing them: continue to replace them, or stop.", st.Changed)
			res.Details["decision"] = map[string]any{"kind": "conflict", "changed": st.Changed}
			res.Details["plan_sample"] = changedSample
			return res, nil
		}
		trace("anomaly checks passed")
	} else {
		trace("continued by the user: anomaly checks and questions skipped for this run")
	}

	prog.SetPhase("checking free space on the target")
	usage, err := spec.Dst.StatFS(ctx, tgt.Path)
	if err != nil {
		return res, fmt.Errorf("checking free space on target: %w", err)
	}
	st.TargetFree, st.TargetTotal = usage.Free, usage.Total
	need := st.NewBytes + max(0, st.ChangedBytes-changedOldBytes)
	reserve := e.Reserve
	if reserve == 0 {
		reserve = 1 << 30
	}
	res.Details["bytes_needed"] = need
	trace("free space on the target: %s, needed %s plus %s reserve", model.HumanBytes(int64(usage.Free)), model.HumanBytes(need), model.HumanBytes(int64(reserve)))
	if uint64(need)+reserve > usage.Free {
		if spec.DryRun {
			res.Message = fmt.Sprintf("not enough space on the target: needs %s plus %s reserve, only %s free", model.HumanBytes(need), model.HumanBytes(int64(reserve)), model.HumanBytes(int64(usage.Free)))
		} else {
			return res, fmt.Errorf("not enough space on the target: needs %s plus %s reserve, only %s free", model.HumanBytes(need), model.HumanBytes(int64(reserve)), model.HumanBytes(int64(usage.Free)))
		}
	}
	if spec.DryRun {
		for i, r := range removals {
			if i == maxIssueLines {
				break
			}
			why := "matches the exclusions"
			if r.sync {
				why = "gone from the source (sync)"
			}
			logf(spec.Log, logbook.FileIssue, "", r.rel, "%s: a backup removes it from the target", why)
		}
		res.Status = store.StatusSuccess
		return res, nil
	}

	// ---- removals ----
	errs := &errorList{max: e.maxErrors(), act: spec.Log}
	if len(removals) > 0 {
		prog.SetPhase("removing")
		if err := e.remove(ctx, spec, prog, removals, res, errs); err != nil {
			return res, err
		}
		logf(spec.Log, logbook.LevelEvent, tgt.NodeID, "", "%d files removed from the target (%d gone from the source, %d excluded)", len(removals), st.Removed, int64(len(removals))-st.Removed)
		if st.NotInSource, err = e.Store.CountNotSeen(ctx, tgt.ID, spec.RunID); err != nil {
			return res, err
		}
	}
	e.logNotInSource(ctx, spec.Log, tgt.ID, spec.RunID, st.NotInSource)

	// ---- transfer ----
	prog.SetPhase("transferring")
	prog.FilesTotal.Store(toCopy)
	prog.BytesTotal.Store(toCopyBytes)
	prog.FilesDone.Store(0)
	prog.BytesDone.Store(0)
	if _, err := pf.Seek(0, 0); err != nil {
		return res, err
	}
	iw = e.Store.NewIndexWriter(tgt.ID)
	var transferred, transferredBytes, failed, skipped, kept atomic.Int64

	// A peer that stays away ends the run with one error, not one per file.
	// Set up before the feeder starts: it reads ctx.
	ctx, stopGuard := guardOffline(ctx)
	defer stopGuard()
	items := make(chan planItem, 64)
	var feedErr error
	go func() {
		defer close(items)
		dec := json.NewDecoder(bufio.NewReaderSize(pf, 1<<20))
		for {
			var it planItem
			if err := dec.Decode(&it); err != nil {
				if !errors.Is(err, io.EOF) {
					feedErr = err
				}
				return
			}
			select {
			case items <- it:
			case <-ctx.Done():
				return
			}
		}
	}()

	newer := job.OnConflict == store.ConflictNewer && !spec.Approved
	bufs := make([][]byte, max(job.Workers, 1))
	logf(spec.Log, logbook.LevelEvent, tgt.NodeID, "", "copying %d files (%s) with %d workers", toCopy, model.HumanBytes(toCopyBytes), max(job.Workers, 1))
	stopBeat = heartbeat(ctx, func() { prog.logTransfer(spec.Log, tgt.NodeID) })
	defer stopBeat()
	runPool(ctx, job.Workers, items, func(worker int, it planItem) {
		if bufs[worker] == nil {
			bufs[worker] = make([]byte, 1<<20)
		}
		rel := model.JoinRel(it.Dir, it.Entry.Name)
		ctx := withFile(ctx, prog.startFile(worker, rel, it.Entry.Size))
		defer prog.endFile(worker)
		defer prog.FilesDone.Add(1)

		srcPath := path.Join(job.SourcePath, rel)
		dstPath := path.Join(tgt.Path, rel)
		if it.Changed && newer {
			// The target's copy is replaced in place (the verified temp file
			// is renamed over it), so it does not take up space in the trash.
			sr, serr := spec.Dst.Stat(ctx, []string{dstPath})
			if serr == nil && len(sr) == 1 && sr[0].Exists && sr[0].Entry.MTime > it.Entry.MTime+mtimeTolerance {
				kept.Add(1)
				prog.BytesDone.Add(it.Entry.Size)
				logf(spec.Log, logbook.FileIssue, "", rel, "changed on the source, but the target's copy is newer (%s, source %s): kept", fmtNano(sr[0].Entry.MTime), fmtNano(it.Entry.MTime))
				return
			}
		}
		ent, sum := it.Entry, ""
		var err error
		switch it.Entry.Type {
		case model.TypeSymlink:
			err = spec.Dst.Symlink(ctx, dstPath, it.Entry.Link)
		default:
			for attempt := 0; attempt < 2; attempt++ {
				ent, sum, err = copyFileHint(ctx, spec.Src, spec.Dst, srcPath, dstPath, bufs[worker], prog, nil, &it.Entry)
				if !errors.Is(err, model.ErrChanged) {
					break
				}
				logf(spec.Log, logbook.FileTrace, "", rel, "changed while copying, trying again")
			}
		}
		if err != nil {
			if offline(ctx, err) || ctx.Err() != nil {
				return
			}
			if isNotExist(err) {
				// Removed from the source since the scan: not an error.
				if sr, serr := spec.Src.Stat(ctx, []string{srcPath}); serr == nil && len(sr) == 1 && !sr[0].Exists {
					skipped.Add(1)
					logf(spec.Log, logbook.FileIssue, "", rel, "skipped: removed from the source after the scan")
					return
				}
			}
			failed.Add(1)
			errs.add(rel, describe(err))
			return
		}
		transferred.Add(1)
		transferredBytes.Add(ent.Size)
		ie := store.IndexEntry{Dir: it.Dir, Name: ent.Name, Type: ent.Type, Size: ent.Size, MTime: ent.MTime,
			Mode: ent.Mode, Hash: sum, Link: ent.Link, SyncedAt: e.now().Unix(), SeenRun: spec.RunID}
		if ent.Type == model.TypeSymlink {
			ie.Link = it.Entry.Link
		}
		if err := iw.Put(ctx, ie); err != nil {
			failed.Add(1)
			errs.add(rel, fmt.Errorf("copied, but updating the index failed: %w", err))
			return
		}
		reason := "new"
		if it.Changed {
			reason = "changed"
		}
		if ent.Type == model.TypeSymlink {
			logf(spec.Log, logbook.FileOK, "", rel, "symlink copied (%s) → %s", reason, it.Entry.Link)
		} else {
			logf(spec.Log, logbook.FileOK, "", rel, "copied (%s, %s), checksum verified", reason, model.HumanBytes(ent.Size))
		}
	})
	stopBeat()
	prog.SetPhase("saving the index")
	closeErr := iw.Close(context.WithoutCancel(ctx))
	st.Transferred, st.TransferredBytes = transferred.Load(), transferredBytes.Load()
	st.Failed += failed.Load() // after failures of the removals, if any
	st.Skipped = skipped.Load()
	st.Kept += kept.Load()
	res.Errors = append(res.Errors, errs.items()...)
	trace("transfer finished: %d copied (%s), %d failed, %d kept, %d skipped", st.Transferred, model.HumanBytes(st.TransferredBytes), st.Failed, st.Kept, st.Skipped)

	e.Store.RefreshTargetTotals(context.WithoutCancel(ctx), tgt.ID)
	if u, err := spec.Dst.StatFS(context.WithoutCancel(ctx), tgt.Path); err == nil {
		st.TargetFree, st.TargetTotal = u.Free, u.Total
	}
	if err := offlineErr(ctx); err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if feedErr != nil {
		return res, fmt.Errorf("reading plan: %w", feedErr)
	}
	if closeErr != nil {
		return res, fmt.Errorf("updating index: %w", closeErr)
	}
	res.Status = finalStatus(st.Failed)
	delete(res.Details, "plan_sample")
	delete(res.Details, "removal_sample")
	return res, nil
}
