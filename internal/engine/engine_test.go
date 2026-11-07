package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/fsx"
	"bfbackup/internal/hooks"
	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

type env struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	eng      *Engine
	ep       *endpoint.Local
	src, dst string
	hookDir  string
	job      store.Job
	tgt      store.Target
	runs     int64
}

func setup(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{t: t, ctx: context.Background(), src: filepath.Join(base, "src"), dst: filepath.Join(base, "dst"), hookDir: filepath.Join(base, "hooks")}
	os.MkdirAll(e.src, 0o755)
	os.MkdirAll(e.hookDir, 0o755)
	f, err := fsx.New([]string{base}, fsx.Options{UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	e.ep = &endpoint.Local{FS: f, Hooks: hooks.Runner{Dir: e.hookDir}}
	e.st, err = store.Open(filepath.Join(base, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.st.Close() })
	e.eng = &Engine{Store: e.st, TmpDir: filepath.Join(base, "tmp"), Reserve: 1}
	// The tests expect the source to win; new jobs default to "newer".
	e.job = store.Job{Name: "j", SourceNode: model.LocalNodeID, SourcePath: e.src, Enabled: true, Workers: 3, OnConflict: store.ConflictSource}
	if err := e.st.CreateJob(e.ctx, &e.job); err != nil {
		t.Fatal(err)
	}
	e.tgt = store.Target{JobID: e.job.ID, NodeID: model.LocalNodeID, Path: e.dst, Enabled: true}
	if err := e.st.CreateTarget(e.ctx, &e.tgt); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) write(root, rel, content string) {
	e.t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) read(root, rel string) string {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (e *env) newRun(kind string) int64 {
	r := store.Run{JobID: e.job.ID, TargetID: e.tgt.ID, Kind: kind, Status: store.StatusRunning}
	if err := e.st.CreateRun(e.ctx, &r); err != nil {
		e.t.Fatal(err)
	}
	return r.ID
}

func (e *env) refresh() {
	var err error
	if e.tgt, err = e.st.GetTarget(e.ctx, e.tgt.ID); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.UpdateJob(e.ctx, &e.job); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) backup(mod func(*BackupSpec)) *Result {
	e.t.Helper()
	e.refresh()
	spec := BackupSpec{RunID: e.newRun(store.KindBackup), Job: e.job, Target: e.tgt, Src: e.ep, Dst: e.ep, Progress: NewProgress()}
	if mod != nil {
		mod(&spec)
	}
	res, err := e.eng.Backup(e.ctx, spec)
	if err != nil {
		e.t.Fatalf("backup: %v", err)
	}
	return res
}

func TestBackupIncremental(t *testing.T) {
	e := setup(t)
	e.write(e.src, "a.txt", "aaa")
	e.write(e.src, "dir/b.txt", "bbbb")
	e.write(e.src, "dir/sub/c.txt", "c")
	e.write(e.src, "cache/skip.txt", "x")
	e.write(e.src, "dir/x.tmp", "x")
	os.Symlink("a.txt", filepath.Join(e.src, "link"))
	e.job.Excludes = []string{"cache/", "*.tmp"}

	res := e.backup(nil)
	s := res.Stats
	if res.Status != store.StatusSuccess || s.New != 4 || s.Transferred != 4 || s.TransferredBytes != 8 {
		t.Fatalf("first run: %s %+v %v", res.Status, s, res.Errors)
	}
	if e.read(e.dst, "dir/sub/c.txt") != "c" || e.read(e.dst, "cache/skip.txt") != "<missing>" {
		t.Fatal("copy content / exclude")
	}
	if l, _ := os.Readlink(filepath.Join(e.dst, "link")); l != "a.txt" {
		t.Fatalf("symlink = %q", l)
	}
	fi, _ := os.Stat(filepath.Join(e.src, "dir/b.txt"))
	fd, _ := os.Stat(filepath.Join(e.dst, "dir/b.txt"))
	if !fi.ModTime().Equal(fd.ModTime()) {
		t.Fatal("mtime not preserved")
	}

	// Nothing changed.
	res = e.backup(nil)
	if res.Stats.Transferred != 0 || res.Stats.Unchanged != 4 {
		t.Fatalf("second run %+v", res.Stats)
	}

	// Change one, add one, delete one.
	e.write(e.src, "a.txt", "AAAAA")
	e.write(e.src, "new.txt", "n")
	os.Remove(filepath.Join(e.src, "dir/b.txt"))
	res = e.backup(nil)
	s = res.Stats
	if s.Changed != 1 || s.New != 1 || s.NotInSource != 1 || s.Transferred != 2 {
		t.Fatalf("third run %+v", s)
	}
	if e.read(e.dst, "dir/b.txt") != "bbbb" {
		t.Fatal("deleting on the source must not delete on the target")
	}
	if e.read(e.dst, "a.txt") != "AAAAA" {
		t.Fatal("changed file not updated")
	}
	e.refresh()
	missing, total, _ := e.st.NotInSource(e.ctx, e.tgt.ID, e.tgt.LastScanRun, 10, 0)
	if total != 1 || missing[0].Rel() != "dir/b.txt" {
		t.Fatalf("not in source %v", missing)
	}
	if e.tgt.StoredFiles != 5 {
		t.Fatalf("stored files %d", e.tgt.StoredFiles)
	}
}

func TestCompareModes(t *testing.T) {
	e := setup(t)
	e.write(e.src, "f", "1234")
	e.backup(nil)
	old := time.Now().Add(-time.Hour)

	// Same size, new content, mtime forced back to the indexed value.
	fi, _ := os.Stat(filepath.Join(e.src, "f"))
	e.write(e.src, "f", "abcd")
	os.Chtimes(filepath.Join(e.src, "f"), fi.ModTime(), fi.ModTime())
	if r := e.backup(nil); r.Stats.Changed != 0 {
		t.Fatal("mtime mode must not see content-only changes")
	}
	e.job.CompareMode = model.CompareChecksum
	if r := e.backup(nil); r.Stats.Changed != 1 || e.read(e.dst, "f") != "abcd" {
		t.Fatalf("checksum mode missed change: %+v", r.Stats)
	}
	// Only the mtime changes: size mode ignores it.
	e.job.CompareMode = model.CompareSize
	os.Chtimes(filepath.Join(e.src, "f"), old, old)
	if r := e.backup(nil); r.Stats.Changed != 0 {
		t.Fatal("size mode reacted to mtime")
	}
	e.job.CompareMode = model.CompareMTime
	if r := e.backup(nil); r.Stats.Changed != 1 {
		t.Fatal("mtime mode missed mtime change")
	}
}

func TestProtectionAndApproval(t *testing.T) {
	e := setup(t)
	for i := 0; i < 100; i++ {
		e.write(e.src, fmt.Sprintf("f%03d", i), "orig")
	}
	fixed := func(s *BackupSpec) { s.Guards = &Guards{Fixed: true, FixedPct: 10, FixedMin: 5} }
	e.backup(fixed)
	for i := 0; i < 20; i++ {
		e.write(e.src, fmt.Sprintf("f%03d", i), "ENCRYPTED")
	}
	res := e.backup(fixed)
	if res.Status != store.StatusApproval || res.Stats.Transferred != 0 {
		t.Fatalf("expected approval, got %s %+v", res.Status, res.Stats)
	}
	if e.read(e.dst, "f000") != "orig" {
		t.Fatal("target touched before approval")
	}
	if sample, _ := res.Details["plan_sample"].([]PlanSample); len(sample) != 20 {
		t.Fatalf("sample %v", res.Details["plan_sample"])
	}
	res = e.backup(func(s *BackupSpec) { fixed(s); s.Approved = true })
	if res.Status != store.StatusSuccess || res.Stats.Transferred != 20 {
		t.Fatalf("approved run %s %+v", res.Status, res.Stats)
	}
	// Below the limit runs through.
	for i := 0; i < 3; i++ {
		e.write(e.src, fmt.Sprintf("f%03d", i), "again")
	}
	if res := e.backup(fixed); res.Status != store.StatusSuccess {
		t.Fatalf("small change needed approval: %s", res.Status)
	}
	var off *Guards
	if off.DeleteLimit(100) != 0 || (&Guards{Deletions: true, DeletePct: 10, DeleteMin: 50}).DeleteLimit(1000) != 100 {
		t.Fatal("delete limit")
	}
	for _, c := range []struct {
		g      Guards
		stored int64
		want   int64
	}{
		{Guards{DeleteCount: true, DeleteCountMax: 50}, 1_000_000, 50},
		{Guards{Deletions: true, DeletePct: 10, DeleteMin: 5, DeleteCount: true, DeleteCountMax: 50}, 100, 10},
		{Guards{Deletions: true, DeletePct: 10, DeleteMin: 5, DeleteCount: true, DeleteCountMax: 50}, 10_000, 50},
		{Guards{Deletions: true, DeletePct: 10, DeleteMin: 5}, 10_000, 1000},
	} {
		if got := c.g.DeleteLimit(c.stored); got != c.want {
			t.Fatalf("DeleteLimit(%d) of %+v = %d, want %d", c.stored, c.g, got, c.want)
		}
	}
}

func TestEmptySourceRefused(t *testing.T) {
	e := setup(t)
	e.write(e.src, "f", "x")
	e.backup(nil)
	os.Remove(filepath.Join(e.src, "f"))
	e.refresh()
	_, err := e.eng.Backup(e.ctx, BackupSpec{RunID: e.newRun("backup"), Job: e.job, Target: e.tgt, Src: e.ep, Dst: e.ep})
	if err == nil || !strings.Contains(err.Error(), "mounted") {
		t.Fatalf("err = %v", err)
	}
}

func TestDryRunAndSpace(t *testing.T) {
	e := setup(t)
	e.write(e.src, "f", "data")
	res := e.backup(func(s *BackupSpec) { s.DryRun = true })
	if res.Stats.New != 1 || res.Stats.Transferred != 0 {
		t.Fatalf("dry run %+v", res.Stats)
	}
	if _, err := os.Stat(e.dst); !os.IsNotExist(err) {
		t.Fatal("dry run wrote to the target")
	}
	e.eng.Reserve = 1 << 62
	e.refresh()
	_, err := e.eng.Backup(e.ctx, BackupSpec{RunID: e.newRun("backup"), Job: e.job, Target: e.tgt, Src: e.ep, Dst: e.ep})
	if err == nil || !strings.Contains(err.Error(), "not enough space") {
		t.Fatalf("err = %v", err)
	}
}

func TestHooks(t *testing.T) {
	e := setup(t)
	e.write(e.src, "f", "x")
	out := filepath.Join(e.hookDir, "out")
	os.WriteFile(filepath.Join(e.hookDir, "pre.sh"), []byte("#!/bin/sh\necho pre $BFB_JOB > "+out+"\n"), 0o755)
	os.WriteFile(filepath.Join(e.hookDir, "post.sh"), []byte("#!/bin/sh\necho post $BFB_RUN_STATUS >> "+out+"\n"), 0o755)
	os.WriteFile(filepath.Join(e.hookDir, "fail.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	e.job.PreHook, e.job.PostHook = "pre.sh", "post.sh"
	e.backup(nil)
	b, _ := os.ReadFile(out)
	if string(b) != "pre j\npost success\n" {
		t.Fatalf("hook output %q", b)
	}
	e.job.PreHook = "fail.sh"
	e.refresh()
	_, err := e.eng.Backup(e.ctx, BackupSpec{RunID: e.newRun("backup"), Job: e.job, Target: e.tgt, Src: e.ep, Dst: e.ep})
	if err == nil || !strings.Contains(err.Error(), "pre-hook") {
		t.Fatalf("err = %v", err)
	}
}

func (e *env) verify(deep, repair bool) *Result {
	e.t.Helper()
	e.refresh()
	res, err := e.eng.Verify(e.ctx, VerifySpec{RunID: e.newRun("verify"), Target: e.tgt, Dst: e.ep, Deep: deep, Repair: repair, Progress: NewProgress()})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func TestVerify(t *testing.T) {
	e := setup(t)
	e.write(e.src, "a/1", "one")
	e.write(e.src, "a/2", "two")
	e.write(e.src, "b/3", "three")
	e.backup(nil)
	if r := e.verify(false, false); r.Status != store.StatusSuccess || r.Stats.Checked != 3 {
		t.Fatalf("clean verify %s %+v", r.Status, r.Stats)
	}

	// Bit rot: same size and mtime, other content.
	p := filepath.Join(e.dst, "a/1")
	fi, _ := os.Stat(p)
	os.WriteFile(p, []byte("ONE"), 0o644)
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	os.RemoveAll(filepath.Join(e.dst, "b")) // whole dir missing
	e.write(e.dst, "a/extra", "?")

	r := e.verify(false, false)
	if r.Stats.Missing != 1 || r.Stats.Mismatched != 0 || r.Stats.Untracked != 1 || r.Status != store.StatusWarning {
		t.Fatalf("quick verify %+v", r.Stats)
	}
	r = e.verify(true, false)
	if r.Stats.Missing != 1 || r.Stats.Mismatched != 1 {
		t.Fatalf("deep verify %+v %v", r.Stats, r.Errors)
	}
	r = e.verify(true, true)
	if r.Stats.Repaired != 2 {
		t.Fatalf("repair %+v", r.Stats)
	}
	res := e.backup(nil)
	if res.Stats.Transferred != 2 || e.read(e.dst, "a/1") != "one" || e.read(e.dst, "b/3") != "three" {
		t.Fatalf("repair backup %+v", res.Stats)
	}
	if r := e.verify(true, false); r.Status != store.StatusSuccess {
		t.Fatalf("after repair %+v", r.Stats)
	}
}

func TestRestore(t *testing.T) {
	e := setup(t)
	e.write(e.src, "docs/a.txt", "a")
	e.write(e.src, "docs/sub/b.txt", "b")
	e.write(e.src, "docs/c.txt", "c")
	e.write(e.src, "other/d.txt", "d")
	e.backup(nil)

	os.RemoveAll(filepath.Join(e.src, "docs/sub"))
	e.write(e.src, "docs/c.txt", "local edit")
	e.refresh()
	res, err := e.eng.Restore(e.ctx, RestoreSpec{RunID: e.newRun("restore"), Target: e.tgt, From: e.ep, To: e.ep,
		ToRoot: e.src, Paths: []string{"docs"}, Progress: NewProgress()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Transferred != 2 || res.Stats.Skipped != 1 || res.Stats.Trashed != 1 {
		t.Fatalf("restore %+v %v", res.Stats, res.Errors)
	}
	if e.read(e.src, "docs/sub/b.txt") != "b" || e.read(e.src, "docs/c.txt") != "c" {
		t.Fatal("restore content")
	}
	matches, _ := filepath.Glob(filepath.Join(e.src, ".bfbackup-trash/*/docs/c.txt"))
	if len(matches) != 1 || e.read("", matches[0]) != "local edit" {
		t.Fatalf("overwritten file not in trash: %v", matches)
	}

	// Single file to another location.
	alt := filepath.Join(filepath.Dir(e.src), "alt")
	res, err = e.eng.Restore(e.ctx, RestoreSpec{RunID: e.newRun("restore"), Target: e.tgt, From: e.ep, To: e.ep,
		ToRoot: alt, Paths: []string{"other/d.txt"}})
	if err != nil || res.Stats.Transferred != 1 || e.read(alt, "other/d.txt") != "d" {
		t.Fatalf("alt restore %+v %v", res, err)
	}
	if _, err := e.eng.Restore(e.ctx, RestoreSpec{Target: e.tgt, From: e.ep, To: e.ep, ToRoot: alt, Paths: []string{"../x"}}); err == nil {
		t.Fatal("bad path accepted")
	}
}

func TestDeleteAndRebuild(t *testing.T) {
	e := setup(t)
	e.write(e.src, "keep/k.txt", "k")
	e.write(e.src, "gone/g1.txt", "g")
	e.write(e.src, "gone/g2.txt", "g")
	e.write(e.src, "orphan.txt", "o")
	e.backup(nil)
	os.Remove(filepath.Join(e.src, "orphan.txt"))

	res, err := e.eng.Delete(e.ctx, DeleteSpec{Job: e.job, Src: e.ep, Targets: []TargetEP{{Target: e.tgt, EP: e.ep}}, Paths: []string{"gone"}})
	if err != nil || res.Stats.Trashed != 2 || res.Stats.Indexed != 2 {
		t.Fatalf("delete %+v %v", res, err)
	}
	if e.read(e.src, "gone/g1.txt") != "<missing>" || e.read(e.dst, "gone/g1.txt") != "<missing>" {
		t.Fatal("not deleted")
	}
	if m, _ := filepath.Glob(filepath.Join(e.src, ".bfbackup-trash/*/gone/g1.txt")); len(m) != 1 {
		t.Fatal("source copy not in trash")
	}
	// Target only, for a file already gone from the source.
	res, err = e.eng.Delete(e.ctx, DeleteSpec{Job: e.job, Targets: []TargetEP{{Target: e.tgt, EP: e.ep}}, Paths: []string{"orphan.txt"}})
	if err != nil || res.Stats.Trashed != 1 {
		t.Fatalf("target delete %+v %v", res, err)
	}
	if _, err := e.eng.Delete(e.ctx, DeleteSpec{Job: e.job, Paths: []string{""}}); err == nil {
		t.Fatal("deleting the root accepted")
	}

	// Lose the index, rebuild it, nothing needs copying.
	e.st.DeletePrefix(e.ctx, e.tgt.ID, "")
	e.refresh()
	res, err = e.eng.RebuildIndex(e.ctx, RebuildSpec{RunID: e.newRun("rebuild_index"), Target: e.tgt, Dst: e.ep, Hash: true})
	if err != nil || res.Stats.Indexed != 1 {
		t.Fatalf("rebuild %+v %v", res, err)
	}
	if r := e.backup(nil); r.Stats.Transferred != 0 || r.Stats.Unchanged != 1 {
		t.Fatalf("backup after rebuild %+v", r.Stats)
	}
}

func TestCancel(t *testing.T) {
	e := setup(t)
	for i := 0; i < 50; i++ {
		e.write(e.src, fmt.Sprintf("d%d/f", i), "x")
	}
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	e.refresh()
	_, err := e.eng.Backup(ctx, BackupSpec{RunID: e.newRun("backup"), Job: e.job, Target: e.tgt, Src: e.ep, Dst: e.ep})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreUsesRecordedMTime(t *testing.T) {
	e := setup(t)
	e.write(e.src, "f.txt", "data")
	exact := time.Date(2024, 3, 4, 5, 6, 7, 123456789, time.UTC)
	os.Chtimes(filepath.Join(e.src, "f.txt"), exact, exact)
	e.backup(nil)
	// The target stores whole seconds only (exFAT, some FUSE mounts).
	coarse := exact.Truncate(time.Second)
	os.Chtimes(filepath.Join(e.dst, "f.txt"), coarse, coarse)
	os.Remove(filepath.Join(e.src, "f.txt"))
	e.refresh()
	if _, err := e.eng.Restore(e.ctx, RestoreSpec{RunID: e.newRun("restore"), Target: e.tgt, From: e.ep, To: e.ep, ToRoot: e.src, Paths: []string{"f.txt"}}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(e.src, "f.txt"))
	if !fi.ModTime().Equal(exact) {
		t.Fatalf("restored mtime %v, want %v", fi.ModTime(), exact)
	}
	if r := e.backup(nil); r.Stats.Changed != 0 {
		t.Fatalf("restored file counted as changed: %+v", r.Stats)
	}
}

func TestProgressPercentAndETA(t *testing.T) {
	p := NewProgress()
	if s := p.Snapshot(); s.Percent != -1 || s.EtaSeconds != -1 {
		t.Fatalf("unknown progress %+v", s)
	}
	p.SetPhase("scanning")
	p.ScanEstimate.Store(200)
	p.Scanned.Store(500) // more files than expected: capped below 100 %
	if s := p.Snapshot(); s.Percent != 99 {
		t.Fatalf("scan percent %v", s.Percent)
	}
	p.SetPhase("transferring")
	p.BytesTotal.Store(1000)
	p.BytesDone.Store(250)
	p.mu.Lock()
	p.since = time.Now().Add(-10 * time.Second)
	p.mu.Unlock()
	s := p.Snapshot()
	if s.Percent != 25 || s.EtaSeconds < 29 || s.EtaSeconds > 31 || s.BytesPerSec < 20 {
		t.Fatalf("transfer %+v", s)
	}
}

func TestCancelStopsLargeLocalCopy(t *testing.T) {
	e := setup(t)
	f, _ := os.Create(filepath.Join(e.src, "big.bin"))
	f.Truncate(512 << 20) // sparse, reads fast but takes many chunks
	f.Close()
	e.refresh()
	ctx, cancel := context.WithCancel(e.ctx)
	prog := NewProgress()
	go func() {
		for prog.BytesDone.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := e.eng.Backup(ctx, BackupSpec{RunID: e.newRun("backup"), Job: e.job, Target: e.tgt, Src: e.ep, Dst: e.ep, Progress: prog})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if done := prog.BytesDone.Load(); done >= 512<<20 {
		t.Fatalf("copy ran to the end (%d bytes) instead of stopping", done)
	}
	if _, err := os.Stat(filepath.Join(e.dst, "big.bin")); !os.IsNotExist(err) {
		t.Fatal("partial file left under the real name")
	}
}

// recorder is an Activity that keeps every line.
type recorder struct {
	mu    sync.Mutex
	level int
	lines []string // "level|node|path|message"
}

func (r *recorder) Enabled(level int) bool { return level <= r.level }

func (r *recorder) Log(level int, node, path, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf("%d|%s|%s|%s", level, node, path, msg))
}

func (r *recorder) has(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestActivityLog(t *testing.T) {
	e := setup(t)
	e.write(e.src, "a.txt", "aaa")
	e.write(e.src, "dir/b.txt", "bb")

	// Level 2: the scan summary, but no single files.
	ev := &recorder{level: logbook.LevelEvent}
	e.backup(func(s *BackupSpec) { s.Log = ev })
	if !ev.has("scan finished: 2 files") || ev.has("copied") {
		t.Fatalf("level 2 lines: %q", ev.lines)
	}

	// Level 3: one line per file, with the reason.
	e.write(e.src, "a.txt", "AAAA")
	e.write(e.src, "new.txt", "n")
	files := &recorder{level: logbook.LevelFile}
	e.backup(func(s *BackupSpec) { s.Log = files })
	if !files.has("3||a.txt|copied (changed, 4 B)") || !files.has("3||new.txt|copied (new, 1 B)") || files.has("dir/b.txt") {
		t.Fatalf("level 3 lines: %q", files.lines)
	}

	// Errors are logged at level 1 with the path.
	os.Remove(filepath.Join(e.dst, "dir/b.txt"))
	errs := &recorder{level: logbook.LevelError}
	e.refresh()
	if _, err := e.eng.Verify(e.ctx, VerifySpec{RunID: e.newRun(store.KindVerifyQuick), Target: e.tgt, Dst: e.ep, Log: errs}); err != nil {
		t.Fatal(err)
	}
	if !errs.has("1|local|dir/b.txt|missing on target") || len(errs.lines) != 1 {
		t.Fatalf("level 1 lines: %q", errs.lines)
	}

	// Deletes name the node they happened on.
	del := &recorder{level: logbook.LevelFile}
	e.refresh()
	if _, err := e.eng.Delete(e.ctx, DeleteSpec{Job: e.job, Src: e.ep, Targets: []TargetEP{{Target: e.tgt, EP: e.ep}}, Paths: []string{"new.txt"}, Log: del}); err != nil {
		t.Fatal(err)
	}
	if !del.has("3|local|new.txt|moved to the trash on the source") || !del.has("3|local|new.txt|moved to the trash on target") {
		t.Fatalf("delete lines: %q", del.lines)
	}
}

func TestIssuesAreLogged(t *testing.T) {
	e := setup(t)
	e.write(e.src, "a.txt", "aaa")
	e.write(e.src, "keep/b.txt", "bb")
	if err := syscall.Mkfifo(filepath.Join(e.src, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := &recorder{level: logbook.FileIssue}
	e.backup(func(s *BackupSpec) { s.Log = first })
	if !first.has("2|local|pipe|skipped: a device, socket or FIFO") {
		t.Fatalf("special file not logged: %q", first.lines)
	}

	// Gone from the source: listed as an issue on every backup.
	os.Remove(filepath.Join(e.src, "a.txt"))
	second := &recorder{level: logbook.FileIssue}
	res := e.backup(func(s *BackupSpec) { s.Log = second })
	if res.Stats.NotInSource != 1 || !second.has("2||a.txt|in the backup, but no longer in the source") {
		t.Fatalf("not in source: %+v %q", res.Stats, second.lines)
	}

	// Files on the target that BFBackup did not put there.
	e.write(e.dst, ".DS_Store", "finder")
	e.write(e.dst, "keep/manual.txt", "by hand")
	ver := &recorder{level: logbook.FileIssue}
	e.refresh()
	vres, err := e.eng.Verify(e.ctx, VerifySpec{RunID: e.newRun(store.KindVerifyQuick), Target: e.tgt, Dst: e.ep, Log: ver, Excludes: []string{".DS_Store"}})
	if err != nil {
		t.Fatal(err)
	}
	if vres.Stats.Untracked != 2 || !ver.has("2||.DS_Store|unknown file on the target: not copied by BFBackup and it matches the job's exclusions") ||
		!ver.has("2||keep/manual.txt|unknown file on the target (7 B)") {
		t.Fatalf("unknown files: %+v %q", vres.Stats, ver.lines)
	}
	if got := vres.Details["untracked_sample"].([]string); strings.Join(got, ",") != ".DS_Store,keep/manual.txt" && strings.Join(got, ",") != "keep/manual.txt,.DS_Store" {
		t.Fatalf("untracked sample %v", got)
	}
}

func TestDeletePermanently(t *testing.T) {
	e := setup(t)
	e.write(e.src, "old/x.txt", "x")
	e.write(e.src, "y.txt", "y")
	e.backup(nil)
	e.refresh()
	log := &recorder{level: logbook.FileOK}
	res, err := e.eng.Delete(e.ctx, DeleteSpec{Job: e.job, Src: e.ep, Targets: []TargetEP{{Target: e.tgt, EP: e.ep}}, Paths: []string{"old"}, Permanent: true, Log: log})
	if err != nil || res.Status != store.StatusSuccess || res.Stats.Deleted != 2 || res.Stats.Trashed != 0 {
		t.Fatalf("delete: %v %+v", err, res)
	}
	for _, root := range []string{e.src, e.dst} {
		if e.read(root, "old/x.txt") != "<missing>" {
			t.Fatalf("%s/old/x.txt still there", root)
		}
		if _, err := os.Stat(filepath.Join(root, model.TrashDirName)); !os.IsNotExist(err) {
			t.Fatalf("%s: permanent delete used the trash", root)
		}
	}
	if !log.has("3|local|old|deleted permanently on the source") || !log.has("3|local|old|deleted permanently on target") {
		t.Fatalf("delete lines %q", log.lines)
	}
	// The trash and the root itself are still refused.
	for _, p := range []string{model.TrashDirName, model.TrashDirName + "/a"} {
		if err := e.ep.Remove(e.ctx, e.dst, p); err == nil {
			t.Fatalf("removing %q was allowed", p)
		}
	}
	if _, err := e.eng.Delete(e.ctx, DeleteSpec{Job: e.job, Src: e.ep, Paths: []string{""}, Permanent: true}); err == nil {
		t.Fatal("deleting the job root was allowed")
	}
}

func TestCleanExcluded(t *testing.T) {
	e := setup(t)
	e.write(e.src, "a.txt", "aaa")
	e.write(e.src, "x.tmp", "x")
	e.write(e.src, "cache/c1", "1")
	e.backup(nil)

	// The exclusions grow; with the option on, the backup removes what matches.
	e.job.Excludes = []string{"*.tmp", "cache/"}
	e.job.CleanExcluded = true
	dry := e.backup(func(s *BackupSpec) { s.DryRun = true })
	if dry.Details["removals"] != 2 || e.read(e.dst, "x.tmp") != "x" {
		t.Fatalf("dry run: %v, x.tmp %q", dry.Details["removals"], e.read(e.dst, "x.tmp"))
	}
	log := &recorder{level: logbook.FileOK}
	res := e.backup(func(s *BackupSpec) { s.Log = log })
	if res.Stats.Trashed != 2 || res.Stats.NotInSource != 0 || e.read(e.dst, "x.tmp") != "<missing>" || e.read(e.dst, "cache/c1") != "<missing>" || e.read(e.dst, "a.txt") != "aaa" {
		t.Fatalf("clean: %+v", res.Stats)
	}
	if !log.has("3||x.tmp|matches the exclusions: moved to the trash on the target") {
		t.Fatalf("clean lines %q", log.lines)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dst, model.TrashDirName)); len(entries) == 0 {
		t.Fatal("cleaned files must go to the trash")
	}

	// More removals than the deletion limit: the run stops and asks first.
	for i := 0; i < 3; i++ {
		e.write(e.src, fmt.Sprintf("logs/l%d.log", i), "l")
	}
	e.job.Excludes = nil
	e.backup(nil)
	e.job.Excludes = []string{"logs/"}
	guard := func(s *BackupSpec) { s.Guards = &Guards{Deletions: true, DeletePct: 10, DeleteMin: 1} }
	res = e.backup(guard)
	if res.Status != store.StatusApproval || res.Stats.Trashed != 0 || e.read(e.dst, "logs/l0.log") != "l" || !strings.Contains(res.Message, "would remove 3 files") {
		t.Fatalf("limit: %s %+v %q", res.Status, res.Stats, res.Message)
	}
	res = e.backup(func(s *BackupSpec) { guard(s); s.Approved = true })
	if res.Stats.Trashed != 3 || e.read(e.dst, "logs/l0.log") != "<missing>" {
		t.Fatalf("after continuing: %+v", res.Stats)
	}

	// Junk that appeared on the target goes with a verify.
	e.write(e.dst, "keep.DS_Store", "finder")
	e.refresh()
	vres, err := e.eng.Verify(e.ctx, VerifySpec{RunID: e.newRun(store.KindVerifyQuick), Target: e.tgt, Dst: e.ep, Excludes: []string{"*.DS_Store"}, Clean: true})
	if err != nil || vres.Stats.Trashed != 1 || vres.Stats.Untracked != 0 || e.read(e.dst, "keep.DS_Store") != "<missing>" {
		t.Fatalf("verify clean: %v %+v", err, vres.Stats)
	}
}

// oldPeer is a node that runs a BFBackup without permanent delete.
type oldPeer struct{ *endpoint.Local }

func (oldPeer) Remove(context.Context, string, string) error {
	return fmt.Errorf("%w (unknown method remove)", model.ErrUnsupported)
}

func TestErrorsNameTheirNode(t *testing.T) {
	e := setup(t)
	e.write(e.src, "d/x.txt", "x")
	e.backup(nil)
	e.refresh()
	tgt := e.tgt
	tgt.NodeID = "colo"
	log := &recorder{level: logbook.FileError}
	res, err := e.eng.Delete(e.ctx, DeleteSpec{Job: e.job, Targets: []TargetEP{{Target: tgt, EP: oldPeer{e.ep}}}, Paths: []string{"d"}, Permanent: true, Log: log})
	if err != nil || len(res.Errors) != 1 {
		t.Fatalf("delete: %v %+v", err, res)
	}
	got := res.Errors[0]
	if got.Node != "colo" || got.Path != "d" || !strings.Contains(got.Message, "on target "+e.dst) || !strings.Contains(got.Message, "older BFBackup version") {
		t.Fatalf("error %+v", got)
	}
	if !log.has("1|colo|d|on target") {
		t.Fatalf("log lines %q", log.lines)
	}
}

func TestSyncRemovesWhatIsGone(t *testing.T) {
	e := setup(t)
	e.write(e.src, "a.txt", "a")
	e.write(e.src, "d/b.txt", "b")
	e.job.Mode = store.ModeSync
	e.backup(nil)
	os.Remove(filepath.Join(e.src, "d/b.txt"))
	res := e.backup(nil)
	if res.Stats.Removed != 1 || res.Stats.Trashed != 1 || res.Stats.NotInSource != 0 || e.read(e.dst, "d/b.txt") != "<missing>" {
		t.Fatalf("sync: %+v", res.Stats)
	}
	// Copy mode keeps it.
	e2 := setup(t)
	e2.write(e2.src, "x.txt", "x")
	e2.write(e2.src, "stays.txt", "s") // an empty source would stop the backup
	e2.backup(nil)
	os.Remove(filepath.Join(e2.src, "x.txt"))
	if res := e2.backup(nil); res.Stats.Removed != 0 || e2.read(e2.dst, "x.txt") != "x" {
		t.Fatalf("copy removed a file: %+v", res.Stats)
	}
	// Old copies "delete": no trash.
	e.write(e.src, "c.txt", "c")
	e.backup(nil)
	os.Remove(filepath.Join(e.src, "c.txt"))
	e.job.OldCopies = store.OldDelete
	if res := e.backup(nil); res.Stats.Deleted != 1 || res.Stats.Trashed != 0 || e.read(e.dst, "c.txt") != "<missing>" {
		t.Fatalf("sync delete: %+v", res.Stats)
	}
}

func TestExistingFilePolicies(t *testing.T) {
	e := setup(t)
	e.write(e.src, "a.txt", "one")
	e.backup(nil)
	change := func(content string) {
		e.write(e.src, "a.txt", content)
		later := time.Now().Add(time.Duration(len(content)) * time.Minute)
		os.Chtimes(filepath.Join(e.src, "a.txt"), later, later)
	}

	e.job.OnConflict = store.ConflictKeep
	change("two")
	if res := e.backup(nil); res.Stats.Kept != 1 || res.Stats.Transferred != 0 || e.read(e.dst, "a.txt") != "one" {
		t.Fatalf("keep: %+v", res.Stats)
	}

	e.job.OnConflict = store.ConflictAsk
	res := e.backup(nil)
	if res.Status != store.StatusApproval || e.read(e.dst, "a.txt") != "one" {
		t.Fatalf("ask: %s", res.Status)
	}
	res = e.backup(func(s *BackupSpec) { s.Approved = true })
	if res.Stats.Transferred != 1 || e.read(e.dst, "a.txt") != "two" || res.Stats.Trashed != 0 || res.Stats.Deleted != 0 {
		t.Fatalf("ask, continued: %+v", res.Stats)
	}
	if _, err := os.Stat(filepath.Join(e.dst, model.TrashDirName)); !os.IsNotExist(err) {
		t.Fatal("a replaced copy must be overwritten in place, not kept in the trash")
	}

	// Newer: the target's copy was touched after the source's.
	e.job.OnConflict = store.ConflictNewer
	change("three!")
	future := time.Now().Add(48 * time.Hour)
	os.Chtimes(filepath.Join(e.dst, "a.txt"), future, future)
	if res := e.backup(nil); res.Stats.Kept != 1 || e.read(e.dst, "a.txt") != "two" {
		t.Fatalf("newer kept: %+v", res.Stats)
	}
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(e.dst, "a.txt"), past, past)
	if res := e.backup(nil); res.Stats.Transferred != 1 || e.read(e.dst, "a.txt") != "three!" {
		t.Fatalf("newer replaced: %+v", res.Stats)
	}
}

func TestAnomalyChecks(t *testing.T) {
	all := DefaultGuards()
	all.ExtMin, all.TruncMin, all.HistoryMin = 3, 3, 3
	run := func(e *env, mod func(*BackupSpec)) *Result {
		return e.backup(func(s *BackupSpec) { s.Guards = &all; mod(s) })
	}
	checks := func(r *Result) string {
		d, _ := r.Details["decision"].(map[string]any)
		an, _ := d["anomalies"].([]Anomaly)
		var c []string
		for _, a := range an {
			c = append(c, a.Check)
		}
		return strings.Join(c, ",")
	}

	// Renamed to a new extension.
	e := setup(t)
	for i := 0; i < 5; i++ {
		e.write(e.src, fmt.Sprintf("doc%d.docx", i), "PK\x03\x04 a document")
	}
	run(e, func(*BackupSpec) {})
	for i := 0; i < 5; i++ {
		os.Rename(filepath.Join(e.src, fmt.Sprintf("doc%d.docx", i)), filepath.Join(e.src, fmt.Sprintf("doc%d.docx.locked", i)))
	}
	if r := run(e, func(*BackupSpec) {}); r.Status != store.StatusApproval || !strings.Contains(checks(r), "extensions") {
		t.Fatalf("renames: %s %q", r.Status, checks(r))
	}

	// Encrypted content and emptied files.
	e = setup(t)
	for i := 0; i < 6; i++ {
		e.write(e.src, fmt.Sprintf("p%d.jpg", i), "\xFF\xD8\xFF a real jpeg header and some bytes")
		e.write(e.src, fmt.Sprintf("t%d.txt", i), "some text that is long enough to count")
	}
	run(e, func(*BackupSpec) {})
	random := make([]byte, 4096)
	for i := range random {
		random[i] = byte(i*7919 ^ i>>3)
	}
	for i := 0; i < 6; i++ {
		e.write(e.src, fmt.Sprintf("p%d.jpg", i), "garbage "+string(random))
		e.write(e.src, fmt.Sprintf("t%d.txt", i), "")
	}
	r := run(e, func(*BackupSpec) {})
	if c := checks(r); r.Status != store.StatusApproval || !strings.Contains(c, "encryption") || !strings.Contains(c, "truncation") {
		t.Fatalf("content: %s %q", r.Status, c)
	}

	// History: three quiet backups, then a burst; the source shrinks a lot.
	e = setup(t)
	for i := 0; i < 40; i++ {
		e.write(e.src, fmt.Sprintf("f%02d", i), strings.Repeat("x", 1000))
	}
	hist := []model.RunStats{{Changed: 1, ScannedBytes: 40000}, {Changed: 0, ScannedBytes: 40000}, {Changed: 1, ScannedBytes: 40000}}
	run(e, func(s *BackupSpec) { s.History = hist })
	for i := 0; i < 10; i++ {
		e.write(e.src, fmt.Sprintf("f%02d", i), "y")
	}
	if c := checks(run(e, func(s *BackupSpec) { s.History = hist })); !strings.Contains(c, "history") {
		t.Fatalf("history: %q", c)
	}
	for i := 10; i < 40; i++ {
		os.Remove(filepath.Join(e.src, fmt.Sprintf("f%02d", i)))
	}
	if c := checks(run(e, func(s *BackupSpec) { s.History = hist })); !strings.Contains(c, "shrink") {
		t.Fatalf("shrink: %q", c)
	}
	// Dry runs only report.
	if r := run(e, func(s *BackupSpec) { s.History = hist; s.DryRun = true }); r.Status != store.StatusSuccess || r.Details["anomalies"] == nil {
		t.Fatalf("dry run: %s %v", r.Status, r.Details["anomalies"])
	}
	if err := DefaultGuards().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDetailLevelLogsDecisions(t *testing.T) {
	e := setup(t)
	e.write(e.src, "a.txt", "a")
	e.write(e.src, "b.txt", "b")
	e.backup(nil)
	e.write(e.src, "b.txt", "bb")
	log := &recorder{level: logbook.LevelTrace}
	e.backup(func(s *BackupSpec) { s.Log = log })
	if !log.has("4||a.txt|unchanged (same size and modification time as in the backup): not copied") ||
		!log.has("4||b.txt|changed (size 1 → 2 bytes") || !log.has("4|||plan: copy 1 files") || !log.has("4|||anomaly checks passed") {
		t.Fatalf("detail lines %q", log.lines)
	}
	three := &recorder{level: logbook.FileOK}
	e.backup(func(s *BackupSpec) { s.Log = three })
	if three.has("unchanged (same") {
		t.Fatal("unchanged files must only be logged at level 4")
	}
}

func TestAdoptFilesAlreadyOnTarget(t *testing.T) {
	e := setup(t)
	when := time.Now().Add(-24 * time.Hour)
	put := func(root, rel, content string, mtime time.Time) {
		e.write(root, rel, content)
		os.Chtimes(filepath.Join(root, rel), mtime, mtime)
	}
	put(e.src, "same.mkv", "movie one", when) // same size and time on both
	put(e.dst, "same.mkv", "movie one", when)
	put(e.src, "touched.mkv", "movie two", when) // same size, other time on the target
	put(e.dst, "touched.mkv", "movie two", when.Add(time.Hour))
	put(e.src, "missing.mkv", "movie three", when) // only on the source
	put(e.dst, "extra.mkv", "old movie", when)     // only on the target

	dry := e.backup(func(s *BackupSpec) { s.DryRun = true })
	st := dry.Stats
	if st.Adopted != 1 || st.Unchanged != 1 || st.Changed != 1 || st.New != 1 || st.NotInSource != 1 {
		t.Fatalf("mtime mode, dry run: %+v", st)
	}
	if need := dry.Details["bytes_needed"].(int64); need != int64(len("movie three")) {
		t.Fatalf("space needed %d: only the missing file counts (same size replacements need no extra)", need)
	}
	res := e.backup(nil)
	if res.Stats.Transferred != 2 || res.Stats.Adopted != 0 || e.read(e.dst, "extra.mkv") != "old movie" {
		t.Fatalf("backup after adoption: %+v", res.Stats)
	}

	// Size mode takes over the file with the other time, too; checksum mode
	// compares the content.
	for mode, adopted := range map[string]int64{model.CompareSize: 3, model.CompareChecksum: 2} {
		e := setup(t)
		e.job.CompareMode = mode
		put := func(root, rel, content string, mtime time.Time) {
			e.write(root, rel, content)
			os.Chtimes(filepath.Join(root, rel), mtime, mtime)
		}
		put(e.src, "a", "aaaa", when)
		put(e.dst, "a", "aaaa", when.Add(time.Hour))
		put(e.src, "b", "bbbb", when)
		put(e.dst, "b", "bbbb", when)
		put(e.src, "c", "cccc", when)
		put(e.dst, "c", "XXXX", when) // same size and time, other content
		if st := e.backup(func(s *BackupSpec) { s.DryRun = true }).Stats; st.Adopted != adopted {
			t.Fatalf("%s mode: adopted %d, want %d (%+v)", mode, st.Adopted, adopted, st)
		}
	}
}

func TestDryRunListsRemovals(t *testing.T) {
	e := setup(t)
	e.write(e.src, "keep.txt", "k")
	e.write(e.src, "gone.txt", "g")
	e.write(e.src, "x.tmp", "x")
	e.backup(nil)
	os.Remove(filepath.Join(e.src, "gone.txt"))
	e.job.Mode, e.job.CleanExcluded, e.job.Excludes = store.ModeSync, true, []string{"*.tmp"}
	log := &recorder{level: logbook.FileIssue}
	dry := e.backup(func(s *BackupSpec) { s.DryRun = true; s.Log = log })
	got := map[string]string{}
	for _, p := range dry.Details["removal_sample"].([]PlanSample) {
		got[p.Path] = p.Reason
	}
	if dry.Details["removals"] != 2 || got["gone.txt"] != "gone from source" || got["x.tmp"] != "excluded" || e.read(e.dst, "gone.txt") != "g" {
		t.Fatalf("dry run removals %v %v", dry.Details["removals"], got)
	}
	if !log.has("2||gone.txt|gone from the source (sync): a backup removes it from the target") || !log.has("2||x.tmp|matches the exclusions: a backup removes it") {
		t.Fatalf("file log %q", log.lines)
	}
	if res := e.backup(nil); res.Details["removal_sample"] != nil {
		t.Fatal("a finished backup keeps no removal list")
	}
}

func TestProgressShowsStepsAndFolders(t *testing.T) {
	e := setup(t)
	e.write(e.src, "a/one.txt", "one")
	e.write(e.src, "b/two.txt", "two")
	prog := NewProgress()
	e.backup(func(s *BackupSpec) { s.Progress = prog })
	var names []string
	for _, p := range prog.Phases() {
		if p.End == 0 || p.End < p.Start {
			t.Fatalf("step %+v has no proper end", p)
		}
		names = append(names, p.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "scanning") || !strings.Contains(joined, "transferring") {
		t.Fatalf("steps %v", names)
	}
	s := prog.Snapshot()
	if s.DirsRead != 3 { // root, a, b
		t.Fatalf("folders read %d, want 3", s.DirsRead)
	}
	if len(s.Files) != 0 || s.FilesDone != 2 || s.BytesDone != 6 {
		t.Fatalf("after the transfer: %+v", s)
	}

	// The bytes of a copy count for the file a worker copies.
	f := prog.startFile(0, "big.bin", 10)
	cw := countingWriter{w: io.Discard, p: prog, f: f}
	cw.Write(make([]byte, 4))
	if got := prog.Snapshot().Files; len(got) != 1 || got[0].Done != 4 || got[0].Path != "big.bin" {
		t.Fatalf("files %+v", got)
	}
	prog.endFile(0)
	if was := prog.SetWaiting("peer gone"); was != "" || prog.Snapshot().Waiting != "peer gone" {
		t.Fatal("waiting not shown")
	}
}

// awayPeer is a target node that drops off after its first answer.
type awayPeer struct {
	*endpoint.Local
	calls int
}

func (a *awayPeer) Trash(ctx context.Context, root, rel, batch string) error {
	if a.calls++; a.calls > 1 {
		return fmt.Errorf("trash: %w", model.ErrOffline)
	}
	return a.Local.Trash(ctx, root, rel, batch)
}

func TestDeleteSkipsOfflineTargets(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"a.txt", "b.txt", "c.txt"} {
		e.write(e.src, p, p)
	}
	e.backup(nil)
	e.refresh()
	log := &recorder{level: logbook.LevelError}
	away := &awayPeer{Local: e.ep}
	res, err := e.eng.Delete(e.ctx, DeleteSpec{RunID: e.newRun(store.KindDelete), Job: e.job, Src: e.ep,
		Targets: []TargetEP{{Target: e.tgt, EP: away}}, Paths: []string{"a.txt", "b.txt", "c.txt"}, Log: log,
		Skipped: []SkippedTarget{{TargetID: 99, NodeID: "colo", Path: "/mnt/backup", Reason: "offline"}}})
	if err != nil {
		t.Fatal(err)
	}
	skipped, _ := res.Details["skipped_targets"].([]SkippedTarget)
	if res.Status != store.StatusWarning || len(skipped) != 2 || skipped[1].Reason != "went offline during the run" || away.calls != 2 {
		t.Fatalf("status %s, skipped %+v, calls %d", res.Status, skipped, away.calls)
	}
	// The source is deleted everywhere, the target only for the first path,
	// and the offline target is no error per path.
	if e.read(e.src, "c.txt") != "<missing>" || e.read(e.dst, "a.txt") != "<missing>" || e.read(e.dst, "c.txt") != "c.txt" || st(res).Failed != 0 {
		t.Fatalf("src c %q, dst a %q, dst c %q, %+v", e.read(e.src, "c.txt"), e.read(e.dst, "a.txt"), e.read(e.dst, "c.txt"), res.Stats)
	}
	if !strings.Contains(res.Message, "not deleted on /mnt/backup (offline)") || !log.has("went offline during the delete") {
		t.Fatalf("message %q, log %q", res.Message, log.lines)
	}
}

func st(r *Result) model.RunStats { return r.Stats }
