package engine

import (
	"context"
	"fmt"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/logbook"
	"bfbackup/internal/match"
	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

// VerifySpec describes a check of one target against the index.
type VerifySpec struct {
	RunID    int64
	Target   store.Target
	Dst      endpoint.Endpoint
	Deep     bool // re-hash every file
	Repair   bool // drop broken entries from the index so the next backup re-copies them
	Progress *Progress
	Log      Activity
	// Excludes of the job; unknown files on the target that match them are
	// called junk in the log (e.g. .DS_Store written by Finder on the target).
	Excludes []string
	// Clean moves that junk to the target's trash, unless there is more of
	// it than CleanLimit (0 = no limit).
	Clean      bool
	CleanLimit int64
}

// maxIssueLines caps how many files of one kind of issue a run lists in the
// file log and the run details; the counts are always complete.
const maxIssueLines = 1000

// untrackedSample is how many unknown files the run details list.
const untrackedSample = 200

// mtimeTolerance allows for file systems with coarse timestamps (FAT/exFAT).
const mtimeTolerance = int64(2e9)

// Verify checks that every indexed file exists on the target with the
// recorded size and mtime (quick) or checksum (deep).
func (e *Engine) Verify(ctx context.Context, spec VerifySpec) (*Result, error) {
	tgt, prog := spec.Target, spec.Progress
	if prog == nil {
		prog = NewProgress()
	}
	res := &Result{Details: map[string]any{"deep": spec.Deep, "repair": spec.Repair}}
	st := &res.Stats
	errs := &errorList{max: e.maxErrors(), act: spec.Log, node: tgt.NodeID}
	iw := e.Store.NewIndexWriter(tgt.ID)
	visited := map[string]bool{}
	junk, _ := match.New(spec.Excludes) // nil on bad patterns: then nothing counts as junk
	var unknown, toClean []string
	untracked := func(dir string, ent model.Entry) {
		st.Untracked++
		rel := model.JoinRel(dir, ent.Name)
		isJunk := junk != nil && junk.ExcludedPath(rel, false)
		if spec.Clean && isJunk {
			toClean = append(toClean, rel) // logged with the outcome below
			return
		}
		if len(unknown) < untrackedSample {
			unknown = append(unknown, rel)
		}
		if st.Untracked > maxIssueLines {
			return
		}
		if isJunk {
			logf(spec.Log, logbook.FileIssue, "", rel, "unknown file on the target: not copied by BFBackup and it matches the job's exclusions (junk such as .DS_Store, created on the target e.g. by Finder). Left untouched")
		} else {
			logf(spec.Log, logbook.FileIssue, "", rel, "unknown file on the target (%s): not in the index, so BFBackup did not copy it, or the index was reset. Left untouched", model.HumanBytes(ent.Size))
		}
	}

	files, bytes, err := e.Store.CountPrefix(ctx, tgt.ID, "")
	if err != nil {
		return res, err
	}
	prog.FilesTotal.Store(files)
	prog.BytesTotal.Store(bytes)
	prog.SetPhase("checking target")

	broken := func(dir string, ie store.IndexEntry, msg string, missing bool) error {
		if missing {
			st.Missing++
		} else {
			st.Mismatched++
		}
		rel := model.JoinRel(dir, ie.Name)
		errs.addMsg(rel, msg)
		if spec.Repair {
			st.Repaired++
			logf(spec.Log, logbook.FileIssue, "", rel, "removed from the index, the next backup copies it again")
			return iw.Delete(ctx, dir, ie.Name)
		}
		return nil
	}

	workers := 8
	if spec.Deep {
		workers = 2 // sequential reads are faster on spinning disks
	}
	req := model.ScanRequest{Root: tgt.Path, Hash: spec.Deep, Workers: workers,
		Hashed: func(n int64) { prog.HashedBytes.Add(n) }, Reading: prog.reading}
	err = spec.Dst.Scan(ctx, req, func(l model.DirListing) error {
		prog.listed(l.Dir)
		visited[l.Dir] = true
		idx, err := e.Store.DirIndex(ctx, tgt.ID, l.Dir)
		if err != nil {
			return err
		}
		onDisk := map[string]model.Entry{}
		for _, ent := range l.Entries {
			if ent.Type == model.TypeDir {
				continue
			}
			onDisk[ent.Name] = ent
			if _, ok := idx[ent.Name]; !ok {
				untracked(l.Dir, ent)
			}
		}
		for name, ie := range idx {
			st.Checked++
			prog.FilesDone.Add(1)
			prog.BytesDone.Add(ie.Size)
			ent, ok := onDisk[name]
			switch {
			case !ok:
				err = broken(l.Dir, ie, "missing on target", true)
			case ent.Type != ie.Type:
				err = broken(l.Dir, ie, fmt.Sprintf("is a %s on target, expected %s", ent.Type, ie.Type), false)
			case ie.Type == model.TypeSymlink && ent.Link != ie.Link:
				err = broken(l.Dir, ie, "symlink target differs", false)
			case ie.Type == model.TypeFile && ent.Size != ie.Size:
				err = broken(l.Dir, ie, fmt.Sprintf("size %d on target, expected %d", ent.Size, ie.Size), false)
			case ie.Type == model.TypeFile && !spec.Deep && abs64(ent.MTime-ie.MTime) > mtimeTolerance:
				err = broken(l.Dir, ie, "modification time differs on target (changed outside BFBackup?)", false)
			case ie.Type == model.TypeFile && spec.Deep && ie.Hash != "" && ent.Hash != ie.Hash:
				err = broken(l.Dir, ie, "checksum mismatch: the stored copy is corrupted", false)
			case ie.Type == model.TypeFile && spec.Deep && ie.Hash == "" && ent.Hash != "":
				// Index rebuilt without hashes: learn the hash now.
				ie.Hash = ent.Hash
				err = iw.Put(ctx, ie)
				logf(spec.Log, logbook.FileOK, "", model.JoinRel(l.Dir, name), "ok, checksum recorded")
			default:
				logf(spec.Log, logbook.FileTrace, "", model.JoinRel(l.Dir, name), "checked: matches the backup")
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		iw.Close(context.WithoutCancel(ctx))
		return res, fmt.Errorf("scanning target: %w", err)
	}

	// Indexed directories the scan never reached are missing entirely.
	dirs, err := e.indexedDirs(ctx, tgt.ID)
	if err != nil {
		return res, err
	}
	for _, d := range dirs {
		if visited[d] {
			continue
		}
		idx, err := e.Store.DirIndex(ctx, tgt.ID, d)
		if err != nil {
			return res, err
		}
		for _, ie := range idx {
			st.Checked++
			prog.FilesDone.Add(1)
			if err := broken(d, ie, "missing on target", true); err != nil {
				return res, err
			}
		}
	}
	if err := iw.Close(ctx); err != nil {
		return res, err
	}
	if spec.Repair && st.Repaired > 0 {
		e.Store.PruneEmptyDirs(ctx, tgt.ID)
		e.Store.RefreshTargetTotals(ctx, tgt.ID)
	}
	if len(toClean) > 0 {
		if spec.CleanLimit > 0 && int64(len(toClean)) > spec.CleanLimit {
			msg := fmt.Sprintf("%d unknown files on the target match the exclusions, more than the protection limit (%d); none were removed. Check the exclusions", len(toClean), spec.CleanLimit)
			logf(spec.Log, logbook.LevelError, tgt.NodeID, "", "%s", msg)
			res.Message = msg
			for _, rel := range toClean {
				if len(unknown) < untrackedSample {
					unknown = append(unknown, rel)
				}
			}
		} else {
			batch := e.trashBatch(spec.RunID)
			for _, rel := range toClean {
				switch err := spec.Dst.Trash(ctx, tgt.Path, rel, batch); {
				case err == nil:
					st.Trashed++
					st.Untracked--
					logf(spec.Log, logbook.FileOK, "", rel, "unknown file that matches the exclusions: moved to the trash on the target")
				case isNotExist(err):
					st.Untracked--
				default:
					errs.add(rel, fmt.Errorf("matches the exclusions, but moving it to the trash on the target failed: %w", describe(err)))
					if len(unknown) < untrackedSample {
						unknown = append(unknown, rel)
					}
				}
			}
		}
	}
	res.Errors = errs.items()
	if len(unknown) > 0 {
		res.Details["untracked_sample"] = unknown
	}
	if st.Untracked > maxIssueLines {
		logf(spec.Log, logbook.LevelEvent, "", "", "%d unknown files on the target; the file log lists the first %d", st.Untracked, maxIssueLines)
	}
	res.Status = store.StatusSuccess
	if st.Missing+st.Mismatched > 0 {
		res.Status = store.StatusWarning
		res.Message = fmt.Sprintf("%d missing, %d damaged or changed", st.Missing, st.Mismatched)
		if spec.Repair {
			res.Message += "; removed from the index, the next backup copies them again"
		}
	}
	return res, nil
}

func (e *Engine) indexedDirs(ctx context.Context, targetID int64) ([]string, error) {
	rows, err := e.Store.DB().QueryContext(ctx, "SELECT path FROM dirs WHERE target_id = ?", targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{""}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
