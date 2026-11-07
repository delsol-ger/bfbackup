package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"bfbackup/internal/model"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newJob(t *testing.T, s *Store, name string) (Job, Target) {
	t.Helper()
	ctx := context.Background()
	j := Job{Name: name, SourceNode: model.LocalNodeID, SourcePath: "/mnt/user/" + name, Enabled: true}
	if err := s.CreateJob(ctx, &j); err != nil {
		t.Fatal(err)
	}
	tg := Target{JobID: j.ID, NodeID: "abcdefabcdef", Path: "/backup/" + name, Schedule: "@daily", Enabled: true}
	if err := s.CreateTarget(ctx, &tg); err != nil {
		t.Fatal(err)
	}
	return j, tg
}

func TestMigrateTwice(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestSettingsSessionsAudit(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if v, _ := s.Setting(ctx, "a", "def"); v != "def" {
		t.Fatal(v)
	}
	s.SetSetting(ctx, "a", "1")
	s.SetSetting(ctx, "a", "2")
	if v, _ := s.Setting(ctx, "a", ""); v != "2" {
		t.Fatal(v)
	}

	now := time.Unix(1_000_000, 0)
	s.SetClock(func() time.Time { return now })
	s.CreateSession(ctx, "h1", now.Add(time.Hour))
	if ok, _ := s.SessionValid(ctx, "h1"); !ok {
		t.Fatal("session should be valid")
	}
	now = now.Add(2 * time.Hour)
	if ok, _ := s.SessionValid(ctx, "h1"); ok {
		t.Fatal("expired session valid")
	}
	if ok, _ := s.SessionValid(ctx, "nope"); ok {
		t.Fatal("unknown session valid")
	}
	s.ExtendSession(ctx, "h1", now.Add(time.Hour))
	if ok, _ := s.SessionValid(ctx, "h1"); !ok {
		t.Fatal("extended session should be valid")
	}

	s.Audit(ctx, "delete", "x")
	s.Audit(ctx, "restore", "y")
	a, _ := s.ListAudit(ctx, 10, 0)
	if len(a) != 2 || a[0].Action != "restore" {
		t.Fatalf("audit %+v", a)
	}
}

func TestJobValidation(t *testing.T) {
	bad := []Job{
		{Name: "", SourceNode: "local", SourcePath: "/a"},
		{Name: "x", SourceNode: "local", SourcePath: "rel"},
		{Name: "x", SourceNode: "local", SourcePath: "/"},
		{Name: "x", SourceNode: "local", SourcePath: "/a", Includes: []string{"../x"}},
		{Name: "x", SourceNode: "local", SourcePath: "/a", Excludes: []string{"["}},
		{Name: "x", SourceNode: "local", SourcePath: "/a", CompareMode: "fuzzy"},
		{Name: "x", SourceNode: "local", SourcePath: "/a", PreHook: "../evil.sh"},
		{Name: "x", SourceNode: "local", SourcePath: "/a", PreHook: "rm -rf"},
		{Name: "x", SourceNode: "local", SourcePath: "/a", ProtectPct: 101},
	}
	for i, j := range bad {
		if err := j.Validate(); err == nil {
			t.Errorf("case %d accepted: %+v", i, j)
		}
	}
	j := Job{Name: " ok ", SourceNode: "local", SourcePath: "/a//b/", Includes: []string{"x/", "", "."}, Excludes: []string{" *.tmp ", ""}}
	if err := j.Validate(); err != nil {
		t.Fatal(err)
	}
	if j.Name != "ok" || j.SourcePath != "/a/b" || len(j.Includes) != 1 || j.Includes[0] != "x" || len(j.Excludes) != 1 || j.CompareMode != "mtime" || j.Workers != 4 {
		t.Fatalf("normalized %+v", j)
	}
	tg := Target{NodeID: "n", Path: "/x", Schedule: "not a cron"}
	if err := tg.Validate(); err == nil {
		t.Fatal("bad cron accepted")
	}
	tg = Target{NodeID: "n", Path: "/x", Schedule: "30 2 * * *", VerifySchedule: "@weekly"}
	if err := tg.Validate(); err != nil || tg.VerifyMode != "quick" {
		t.Fatal(err, tg.VerifyMode)
	}
}

func TestJobsTargetsNodes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreateNode(ctx, "abcdefabcdef", "colo", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNode(ctx, "bbbbbbbbbbbb", "colo", []byte("s")); err == nil {
		t.Fatal("duplicate node name accepted")
	}
	j, tg := newJob(t, s, "photos")
	dup := Job{Name: "photos", SourceNode: "local", SourcePath: "/x"}
	if err := s.CreateJob(ctx, &dup); err == nil {
		t.Fatal("duplicate job name accepted")
	}
	got, err := s.GetJob(ctx, j.ID)
	if err != nil || len(got.Targets) != 1 || got.Targets[0].ID != tg.ID || got.Includes == nil {
		t.Fatalf("GetJob %+v %v", got, err)
	}
	if err := s.DeleteNode(ctx, "abcdefabcdef"); err == nil {
		t.Fatal("deleted node in use")
	}
	jobs, _ := s.ListJobs(ctx)
	if len(jobs) != 1 || len(jobs[0].Targets) != 1 {
		t.Fatalf("ListJobs %+v", jobs)
	}
	tg.Schedule = ""
	tg.Enabled = false
	if err := s.UpdateTarget(ctx, &tg); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTarget(ctx, tg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("target survived job delete")
	}
	if err := s.DeleteNode(ctx, "abcdefabcdef"); err != nil {
		t.Fatal(err)
	}
}

func TestNodeDisks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	s.CreateNode(ctx, "abcdefabcdef", "colo", []byte("secret"))
	got, err := s.ListNodeDisks(ctx)
	if err != nil || got["abcdefabcdef"].At != 0 || len(got["abcdefabcdef"].Disks) != 0 {
		t.Fatalf("before %+v %v", got, err)
	}
	disks := []model.DiskInfo{{Path: "/mnt/user", FSType: "fuse.shfs", Total: 1000, Used: 400, Free: 600}}
	if err := s.SetNodeDisks(ctx, "abcdefabcdef", disks); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListNodeDisks(ctx)
	d := got["abcdefabcdef"]
	if err != nil || d.At == 0 || len(d.Disks) != 1 || d.Disks[0] != disks[0] {
		t.Fatalf("after %+v %v", got, err)
	}
}

func put(t *testing.T, w *IndexWriter, rel string, size int64, run int64) {
	t.Helper()
	dir, name := model.SplitRel(rel)
	if err := w.Put(context.Background(), IndexEntry{Dir: dir, Name: name, Type: model.TypeFile, Size: size, SeenRun: run}); err != nil {
		t.Fatal(err)
	}
}

func TestIndex(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, tg := newJob(t, s, "a")
	w := s.NewIndexWriter(tg.ID)
	put(t, w, "top.txt", 1, 1)
	put(t, w, "a/b/c.txt", 10, 1)
	put(t, w, "a/b/d.txt", 20, 1)
	put(t, w, "a/x.txt", 5, 1)
	put(t, w, "a0/y.txt", 7, 1) // must not count as below "a"
	put(t, w, "a.b/z.txt", 7, 1)
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}

	dirs, files, err := s.BrowseDir(ctx, tg.ID, "", 0)
	if err != nil || len(dirs) != 3 || len(files) != 1 {
		t.Fatalf("browse root dirs=%v files=%v %v", dirs, files, err)
	}
	// "a" holds a/x.txt (5) and a/b/* (10+20); "a0" and "a.b" must not leak in.
	if dirs[0].Name != "a" || dirs[0].Files != 3 || dirs[0].Bytes != 35 || dirs[2].Name != "a0" || dirs[2].Bytes != 7 {
		t.Fatalf("root dir totals %+v", dirs)
	}
	dirs, files, _ = s.BrowseDir(ctx, tg.ID, "a", 0)
	if len(dirs) != 1 || dirs[0].Name != "b" || dirs[0].Files != 2 || dirs[0].Bytes != 30 || len(files) != 1 {
		t.Fatalf("browse a: %+v %v", dirs, files)
	}
	n, b, _ := s.CountPrefix(ctx, tg.ID, "a")
	if n != 3 || b != 35 {
		t.Fatalf("count a = %d %d", n, b)
	}
	n, _, _ = s.CountPrefix(ctx, tg.ID, "a/b/c.txt")
	if n != 1 {
		t.Fatalf("count file = %d", n)
	}
	var walked []string
	s.WalkPrefix(ctx, tg.ID, "a", func(e IndexEntry) error { walked = append(walked, e.Rel()); return nil })
	if len(walked) != 3 {
		t.Fatalf("walk %v", walked)
	}

	// Run 2 sees everything except a/x.txt.
	w = s.NewIndexWriter(tg.ID)
	for _, rel := range []string{"top.txt", "a/b/c.txt", "a/b/d.txt", "a0/y.txt", "a.b/z.txt"} {
		d, nm := model.SplitRel(rel)
		w.Seen(ctx, d, nm, 2)
	}
	w.Close(ctx)
	missing, total, _ := s.NotInSource(ctx, tg.ID, 2, 10, 0)
	if total != 1 || missing[0].Rel() != "a/x.txt" {
		t.Fatalf("not in source %v %d", missing, total)
	}

	if hits, _ := s.SearchIndex(ctx, tg.ID, "d.t", 10); len(hits) != 1 {
		t.Fatalf("search %v", hits)
	}
	if hits, _ := s.SearchIndex(ctx, tg.ID, "%", 10); len(hits) != 0 {
		t.Fatalf("wildcard not escaped: %v", hits)
	}

	del, _ := s.DeletePrefix(ctx, tg.ID, "a/b")
	if del != 2 {
		t.Fatalf("deleted %d", del)
	}
	s.PruneEmptyDirs(ctx, tg.ID)
	dirs, _, _ = s.BrowseDir(ctx, tg.ID, "a", 0)
	if len(dirs) != 0 {
		t.Fatalf("empty dir not pruned: %v", dirs)
	}
	files2, bytes, _ := s.RefreshTargetTotals(ctx, tg.ID)
	if files2 != 4 || bytes != 20 {
		t.Fatalf("totals %d %d", files2, bytes)
	}
}

// TestDeletePrefixes removes files and folders with what is below them, and
// nothing that only shares the beginning of the name.
func TestDeletePrefixes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, tg := newJob(t, s, "a")
	w := s.NewIndexWriter(tg.ID)
	for _, rel := range []string{"a/f.txt", "a/b/c.txt", "a/b/d/e.txt", "a/b-x.txt", "a/b.y/z.txt", "a/b0/z.txt", "k/l.txt", "k/l.txt2"} {
		put(t, w, rel, 1, 1)
	}
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeletePrefixes(ctx, tg.ID, []string{"a/b", "k/l.txt", "nothing"})
	if err != nil || n != 3 {
		t.Fatalf("deleted %d %v", n, err)
	}
	var left []string
	s.WalkPrefix(ctx, tg.ID, "", func(e IndexEntry) error { left = append(left, e.Rel()); return nil })
	sort.Strings(left)
	if want := []string{"a/b-x.txt", "a/b.y/z.txt", "a/b0/z.txt", "a/f.txt", "k/l.txt2"}; !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
	dirs, _, _ := s.BrowseDir(ctx, tg.ID, "a", 0)
	for _, d := range dirs {
		if d.Name == "b" {
			t.Fatalf("folder a/b still listed: %+v", dirs)
		}
	}
}

func TestRuns(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, tg := newJob(t, s, "a")
	r := Run{JobID: j.ID, TargetID: tg.ID, Kind: KindBackup, Status: StatusRunning, Trigger: "manual"}
	if err := s.CreateRun(ctx, &r); err != nil {
		t.Fatal(err)
	}
	r.Status = StatusSuccess
	r.Stats.Transferred = 3
	r.Stats.TransferredBytes = 300
	r.FinishedAt = r.StartedAt + 5
	if err := s.UpdateRun(ctx, &r); err != nil {
		t.Fatal(err)
	}
	s.AddRunErrors(ctx, r.ID, []RunError{{Path: "x", Message: "boom"}})
	errs, _ := s.ListRunErrors(ctx, r.ID, 10)
	if len(errs) != 1 {
		t.Fatal(errs)
	}
	r2 := Run{JobID: j.ID, TargetID: tg.ID, Kind: KindVerifyQuick, Status: StatusQueued}
	s.CreateRun(ctx, &r2)

	runs, total, _ := s.ListRuns(ctx, RunFilter{TargetID: tg.ID, Kinds: []string{KindBackup}})
	if total != 1 || runs[0].Stats.Transferred != 3 {
		t.Fatalf("list %v %d", runs, total)
	}
	last, _ := s.LastRun(ctx, tg.ID, KindBackup, KindVerifyQuick)
	if last == nil || last.ID != r2.ID {
		t.Fatal("last run")
	}
	if n, _ := s.MarkInterrupted(ctx); n != 1 {
		t.Fatalf("interrupted %d", n)
	}
	days, _ := s.DailyStats(ctx, time.Now().Add(-48*time.Hour), 0)
	if len(days) != 1 || days[0].Bytes != 300 {
		t.Fatalf("days %+v", days)
	}
}

func TestExportImport(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	s.CreateNode(ctx, "abcdefabcdef", "colo", []byte("secret"))
	s.SetSetting(ctx, "pushover_user", "u")
	s.SetSetting(ctx, "admin_password_hash", "hash")
	j, tg := newJob(t, s, "photos")
	_, tg2 := newJob(t, s, "movies")
	w := s.NewIndexWriter(tg.ID)
	put(t, w, "a.jpg", 1, 1)
	w.Close(ctx)

	ex, err := s.ExportConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ex.Settings["admin_password_hash"]; ok {
		t.Fatal("password hash exported")
	}
	if len(ex.Nodes) != 1 || string(ex.Nodes[0].Secret) != "secret" || len(ex.Jobs) != 2 {
		t.Fatalf("export %+v", ex)
	}

	// Import an export that only has "photos": "movies" goes away, the
	// index of photos' target stays.
	var keep []Job
	for _, jj := range ex.Jobs {
		if jj.ID == j.ID {
			jj.Name = "photos-renamed"
			keep = append(keep, jj)
		}
	}
	ex.Jobs = keep
	if err := s.ImportConfig(ctx, ex); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.ListJobs(ctx)
	if len(jobs) != 1 || jobs[0].Name != "photos-renamed" || len(jobs[0].Targets) != 1 || jobs[0].Targets[0].Path != tg.Path {
		t.Fatalf("after import %+v", jobs)
	}
	if n, _, _ := s.CountPrefix(ctx, tg.ID, ""); n != 1 {
		t.Fatal("index of kept target was lost")
	}
	if _, err := s.GetTarget(ctx, tg2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("dropped target still exists")
	}
	if v, _ := s.Setting(ctx, "admin_password_hash", ""); v != "hash" {
		t.Fatal("import touched the password")
	}

	// Into a fresh database.
	s2 := newStore(t)
	if err := s2.ImportConfig(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if sec, _ := s2.NodeSecret(ctx, "abcdefabcdef"); string(sec) != "secret" {
		t.Fatal("secret not imported")
	}
}

func TestSnapshot(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	s.SetSetting(ctx, "k", "v")
	p := filepath.Join(t.TempDir(), "snap.db")
	if err := s.Snapshot(ctx, p); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if v, _ := s2.Setting(ctx, "k", ""); v != "v" {
		t.Fatal("snapshot content")
	}
}

func TestNodeOfflineAlert(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreateNode(ctx, "abcdefabcdef", "colo", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	n, _ := s.GetNode(ctx, "abcdefabcdef")
	if !n.OfflineNotify || n.OfflineHours != 1 {
		t.Fatalf("defaults %+v", n)
	}
	if err := s.SetNodeOffline(ctx, "abcdefabcdef", false, 48); err != nil {
		t.Fatal(err)
	}
	n, _ = s.GetNode(ctx, "abcdefabcdef")
	if n.OfflineNotify || n.OfflineHours != 48 {
		t.Fatalf("after set %+v", n)
	}
	for _, h := range []int{0, MaxOfflineHours + 1} {
		if err := s.SetNodeOffline(ctx, "abcdefabcdef", true, h); err == nil {
			t.Fatalf("%d hours accepted", h)
		}
	}
	// An export from before per-peer alerts has no hours: import uses the defaults.
	ex, err := s.ExportConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ex.Nodes[0].OfflineNotify, ex.Nodes[0].OfflineHours = false, 0
	if err := s.ImportConfig(ctx, ex); err != nil {
		t.Fatal(err)
	}
	n, _ = s.GetNode(ctx, "abcdefabcdef")
	if !n.OfflineNotify || n.OfflineHours != 1 {
		t.Fatalf("old export import %+v", n)
	}
}

func TestJobCleanExcludedAndErrorNode(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, tg := newJob(t, s, "photos")
	j.CleanExcluded = true
	if err := s.UpdateJob(ctx, &j); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetJob(ctx, j.ID); !got.CleanExcluded {
		t.Fatal("clean_excluded not stored")
	}
	r := Run{JobID: j.ID, TargetID: tg.ID, Kind: KindDelete, Status: StatusRunning}
	if err := s.CreateRun(ctx, &r); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRunErrors(ctx, r.ID, []RunError{{Path: "a", Message: "boom", Node: "colo"}, {Path: "b", Message: "x"}}); err != nil {
		t.Fatal(err)
	}
	errs, _ := s.ListRunErrors(ctx, r.ID, 10)
	if len(errs) != 2 || errs[0].Node != "colo" || errs[1].Node != "" {
		t.Fatalf("errors %+v", errs)
	}
}

func TestBrowseDirNotInSource(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, tg := newJob(t, s, "photos")
	w := s.NewIndexWriter(tg.ID)
	put(t, w, "old/a.txt", 1, 1) // only seen by scan 1
	put(t, w, "old/b.txt", 1, 1)
	put(t, w, "mixed/c.txt", 1, 1)
	put(t, w, "mixed/d.txt", 1, 2)
	put(t, w, "new/e.txt", 1, 2)
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	dirs, _, err := s.BrowseDir(ctx, tg.ID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, d := range dirs {
		got[d.Name] = d.NotInSource
	}
	if got["old"] != 2 || got["mixed"] != 1 || got["new"] != 0 {
		t.Fatalf("not in source per folder %v", got)
	}
	if dirs, _, _ := s.BrowseDir(ctx, tg.ID, "", 0); dirs[0].NotInSource != 0 {
		t.Fatal("without a scan nothing is not in source")
	}
}

func TestDrivesAndExport(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	d := Drive{UUID: "u1", Name: "Offsite-A", OnConnect: true, OverdueDays: 30}
	if err := s.CreateDrive(ctx, &d); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDrive(ctx, &Drive{UUID: "u2", Name: "Offsite-A"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	j := Job{Name: "j", SourceNode: "local", SourcePath: "/mnt/user/a", Enabled: true}
	if err := s.CreateJob(ctx, &j); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTarget(ctx, &Target{JobID: j.ID, NodeID: "usb:u1", Path: "/j", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s.SeenDrive(ctx, "u1", "/mnt/disks/usb", "xfs", 100, 50)
	ex, err := s.ExportConfig(ctx)
	if err != nil || len(ex.Drives) != 1 || ex.Drives[0].LastMount != "/mnt/disks/usb" {
		t.Fatalf("export %+v %v", ex, err)
	}
	s2 := newStore(t)
	if err := s2.ImportConfig(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if got, err := s2.GetDrive(ctx, "u1"); err != nil || got.Name != "Offsite-A" || !got.OnConnect {
		t.Fatalf("imported %+v %v", got, err)
	}
	if err := s.DeleteDrive(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if j, _ = s.GetJob(ctx, j.ID); len(j.Targets) != 0 {
		t.Fatal("targets of a deleted drive remain")
	}
}
