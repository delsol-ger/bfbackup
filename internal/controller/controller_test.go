package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/engine"
	"bfbackup/internal/fsx"
	"bfbackup/internal/hooks"
	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/notify"
	"bfbackup/internal/peer"
	"bfbackup/internal/proto"
	"bfbackup/internal/store"
)

type fakeNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (f *fakeNotifier) Notify(_ context.Context, ev notify.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeNotifier) kinds() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var k []string
	for _, e := range f.events {
		k = append(k, e.Kind)
	}
	return strings.Join(k, ",")
}

type world struct {
	t         *testing.T
	ctx       context.Context
	c         *Controller
	notes     *fakeNotifier
	ctrlRoot  string
	peerRoot  string
	peerAddr  string
	nodeID    string
	token     string
	stopPeer  context.CancelFunc
	peerReady chan struct{}
}

func localEP(t *testing.T, root string) *endpoint.Local {
	f, err := fsx.New([]string{root}, fsx.Options{UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return &endpoint.Local{FS: f, Hooks: hooks.Runner{Dir: filepath.Join(root, "hooks")}, Version: "test"}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, ctx: context.Background(), notes: &fakeNotifier{}, ctrlRoot: t.TempDir(), peerRoot: t.TempDir()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var err error
	w.c, err = New(Config{
		DataDir:       filepath.Join(t.TempDir(), "data"),
		Version:       "test",
		Local:         localEP(t, w.ctrlRoot),
		Log:           log,
		NotifierFor:   func(Settings) notify.Notifier { return w.notes },
		SchedulerTick: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.c.store.SetSetting(w.ctx, "pushover_token", "x")
	w.c.store.SetSetting(w.ctx, "pushover_user", "y")
	// Both optional features on; a fresh install has them off.
	w.c.store.SetSetting(w.ctx, featuresKey, `{"usb_drives":true,"hooks":true}`)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w.peerAddr = ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	w.c.Start(ctx, ln)
	t.Cleanup(func() {
		if w.stopPeer != nil {
			w.stopPeer()
		}
		cancel()
		w.c.Close()
	})

	n, tok, err := w.c.AddNode(w.ctx, "colo")
	if err != nil {
		t.Fatal(err)
	}
	w.nodeID, w.token = n.ID, tok
	w.startPeer(tok)
	return w
}

func (w *world) startPeer(token string) {
	w.t.Helper()
	tok, err := proto.ParseToken(token)
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.stopPeer = cancel
	a := &peer.Agent{Addr: w.peerAddr, Token: tok, EP: localEP(w.t, w.peerRoot), Info: proto.PeerInfo{Hostname: "peerhost", Version: "test"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}
	go a.Run(ctx)
}

func (w *world) waitOnline(want bool) {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		nodes, _ := w.c.Nodes(w.ctx)
		for _, n := range nodes {
			if n.ID == w.nodeID && n.Online == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.t.Fatalf("peer online != %v", want)
}

func (w *world) wait(run store.Run) store.Run {
	w.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := w.c.store.GetRun(w.ctx, run.ID)
		if err != nil {
			w.t.Fatal(err)
		}
		if r.Status != store.StatusQueued && r.Status != store.StatusRunning && w.c.TargetBusy(r.TargetID) != r.ID {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.t.Fatal("run did not finish")
	return store.Run{}
}

func (w *world) run(req RunRequest) store.Run {
	w.t.Helper()
	r, err := w.c.StartRun(w.ctx, req)
	if err != nil {
		w.t.Fatal(err)
	}
	return w.wait(r)
}

func write(t *testing.T, root, rel, content string) {
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(root, rel string) string {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func TestPushPullRestoreDelete(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	nodes, _ := w.c.Nodes(w.ctx)
	if len(nodes) != 2 || nodes[1].Hostname != "peerhost" {
		t.Fatalf("nodes %+v", nodes)
	}

	// Push: controller -> peer.
	src := filepath.Join(w.ctrlRoot, "share")
	for i := 0; i < 30; i++ {
		write(t, src, fmt.Sprintf("dir%d/file%d.txt", i%5, i), strings.Repeat("x", i*100))
	}
	job := store.Job{Name: "push", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{{NodeID: w.nodeID, Path: filepath.Join(w.peerRoot, "backup"), Enabled: true}}}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	tid := job.Targets[0].ID
	r := w.run(RunRequest{Kind: store.KindBackup, TargetID: tid})
	if r.Status != store.StatusSuccess || r.Stats.Transferred != 30 {
		t.Fatalf("push run %s %q %+v", r.Status, r.Error, r.Stats)
	}
	if read(w.peerRoot, "backup/dir3/file13.txt") != strings.Repeat("x", 1300) {
		t.Fatal("pushed content")
	}

	// Verify over the network.
	if r := w.run(RunRequest{Kind: store.KindVerifyDeep, TargetID: tid}); r.Status != store.StatusSuccess || r.Stats.Checked != 30 {
		t.Fatalf("verify %s %+v", r.Status, r.Stats)
	}

	// Restore a folder from the peer back to the source.
	os.RemoveAll(filepath.Join(src, "dir2"))
	r = w.run(RunRequest{Kind: store.KindRestore, TargetID: tid, Restore: &RestoreRequest{Paths: []string{"dir2"}}})
	if r.Status != store.StatusSuccess || r.Stats.Transferred != 6 || read(src, "dir2/file7.txt") != strings.Repeat("x", 700) {
		t.Fatalf("restore %s %q %+v", r.Status, r.Error, r.Stats)
	}

	// Delete everywhere.
	r = w.run(RunRequest{Kind: store.KindDelete, JobID: job.ID, Delete: &DeleteRequest{Paths: []string{"dir4"}, FromSource: true}})
	if r.Status != store.StatusSuccess || read(src, "dir4/file4.txt") != "<missing>" || read(w.peerRoot, "backup/dir4/file4.txt") != "<missing>" {
		t.Fatalf("delete %s %q %+v", r.Status, r.Error, r.Stats)
	}

	// Pull: peer -> controller.
	write(t, w.peerRoot, "data/a.txt", "from peer")
	pull := store.Job{Name: "pull", SourceNode: w.nodeID, SourcePath: filepath.Join(w.peerRoot, "data"), Enabled: true,
		Targets: []store.Target{{NodeID: model.LocalNodeID, Path: filepath.Join(w.ctrlRoot, "pulled"), Enabled: true}}}
	if err := w.c.CreateJob(w.ctx, &pull); err != nil {
		t.Fatal(err)
	}
	r = w.run(RunRequest{Kind: store.KindBackup, TargetID: pull.Targets[0].ID})
	if r.Status != store.StatusSuccess || read(w.ctrlRoot, "pulled/a.txt") != "from peer" {
		t.Fatalf("pull %s %q", r.Status, r.Error)
	}

	ov, err := w.c.Overview(w.ctx)
	if err != nil || len(ov.Jobs) != 2 || ov.StoredFiles != 25 {
		t.Fatalf("overview files=%d %v", ov.StoredFiles, err)
	}
	if f, err := w.c.TargetForecast(w.ctx, tid); err != nil || !f.Live || f.Total == 0 {
		t.Fatalf("forecast %+v %v", f, err)
	}
	if entries, err := w.c.ListDir(w.ctx, w.nodeID, ""); err != nil || len(entries) != 1 {
		t.Fatalf("listdir roots %v %v", entries, err)
	}
}

func TestOfflineFailsAndNotifies(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	src := filepath.Join(w.ctrlRoot, "s")
	write(t, src, "f", "x")
	job := store.Job{Name: "j", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{{NodeID: w.nodeID, Path: filepath.Join(w.peerRoot, "b"), Enabled: true}}}
	w.c.CreateJob(w.ctx, &job)

	w.stopPeer()
	w.waitOnline(false)
	r := w.run(RunRequest{Kind: store.KindBackup, TargetID: job.Targets[0].ID})
	if r.Status != store.StatusFailed || !strings.Contains(r.Error, "offline") {
		t.Fatalf("offline run %s %q", r.Status, r.Error)
	}
	if !strings.Contains(w.notes.kinds(), notify.RunFailed) {
		t.Fatalf("notifications %s", w.notes.kinds())
	}
}

func TestOfflinePeerKeepsLastDisks(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	// The disks are saved in the background right after the peer connects.
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, err := w.c.store.ListNodeDisks(w.ctx)
		if err == nil && d[w.nodeID].At != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disks not saved on connect: %+v %v", d, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	online, err := w.c.NodesUsage(w.ctx)
	if err != nil || len(online) != 2 || len(online[1].Disks) == 0 || online[1].DisksStale {
		t.Fatalf("online usage %+v %v", online, err)
	}

	w.stopPeer()
	w.waitOnline(false)
	offline, err := w.c.NodesUsage(w.ctx)
	if err != nil || len(offline) != 2 {
		t.Fatalf("offline usage %+v %v", offline, err)
	}
	p := offline[1]
	if p.ID != w.nodeID || !p.DisksStale || p.DisksAt == 0 || len(p.Disks) != len(online[1].Disks) || p.Disks[0].Total != online[1].Disks[0].Total {
		t.Fatalf("offline peer %+v, online was %+v", p, online[1])
	}
	if offline[0].DisksStale {
		t.Fatal("controller marked stale")
	}
}

func TestApprovalBusyAndTokens(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	src := filepath.Join(w.ctrlRoot, "s")
	for i := 0; i < 20; i++ {
		write(t, src, fmt.Sprintf("f%02d", i), "orig")
	}
	s, _ := w.c.Settings(w.ctx)
	s.Guards.Fixed, s.Guards.FixedPct, s.Guards.FixedMin = true, 10, 2
	if err := w.c.SaveSettings(w.ctx, s, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	job := store.Job{Name: "j", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{{NodeID: model.LocalNodeID, Path: filepath.Join(w.ctrlRoot, "b"), Enabled: true}}}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	tid := job.Targets[0].ID
	w.run(RunRequest{Kind: store.KindBackup, TargetID: tid})
	for i := 0; i < 10; i++ {
		write(t, src, fmt.Sprintf("f%02d", i), "changed!")
	}
	r := w.run(RunRequest{Kind: store.KindBackup, TargetID: tid})
	if r.Status != store.StatusApproval {
		t.Fatalf("status %s", r.Status)
	}
	if !strings.Contains(w.notes.kinds(), notify.ApprovalRequired) {
		t.Fatalf("notifications %s", w.notes.kinds())
	}
	nr, err := w.c.ApproveRun(w.ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	nr = w.wait(nr)
	if nr.Status != store.StatusSuccess || nr.Stats.Transferred != 10 {
		t.Fatalf("approved run %s %+v", nr.Status, nr.Stats)
	}
	if old, _ := w.c.store.GetRun(w.ctx, r.ID); old.Status != store.StatusApproved {
		t.Fatalf("old run status %s", old.Status)
	}
	if _, err := w.c.ApproveRun(w.ctx, r.ID); err == nil {
		t.Fatal("approved twice")
	}

	// Target overlap with the source is refused.
	bad := store.Target{JobID: job.ID, NodeID: model.LocalNodeID, Path: filepath.Join(src, "inside")}
	if err := w.c.AddTarget(w.ctx, &bad); err == nil {
		t.Fatal("overlapping target accepted")
	}

	// Rotating the token kicks the peer; the old token no longer works.
	if _, err := w.c.RotateToken(w.ctx, w.nodeID); err != nil {
		t.Fatal(err)
	}
	w.waitOnline(false)
	time.Sleep(200 * time.Millisecond)
	nodes, _ := w.c.Nodes(w.ctx)
	if nodes[1].Online {
		t.Fatal("peer with old token reconnected")
	}
}

func TestScheduler(t *testing.T) {
	w := newWorld(t)
	src := filepath.Join(w.ctrlRoot, "s")
	write(t, src, "f", "x")
	job := store.Job{Name: "j", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{{NodeID: model.LocalNodeID, Path: filepath.Join(w.ctrlRoot, "b"), Enabled: true, Schedule: "0 3 * * *"}}}
	w.c.CreateJob(w.ctx, &job)
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.Local)

	w.c.scheduleDue(w.ctx, day.Add(time.Hour), day.Add(2*time.Hour))
	if runs, _, _ := w.c.store.ListRuns(w.ctx, store.RunFilter{}); len(runs) != 0 {
		t.Fatal("run started outside schedule")
	}
	w.c.scheduleDue(w.ctx, day.Add(2*time.Hour+59*time.Minute), day.Add(3*time.Hour))
	runs, _, _ := w.c.store.ListRuns(w.ctx, store.RunFilter{})
	if len(runs) != 1 || runs[0].Trigger != "schedule" {
		t.Fatalf("runs %+v", runs)
	}
	w.wait(runs[0])
	if !due("@daily", day.Add(-time.Minute), day) || due("", day, day) || due("garbage", day, day) {
		t.Fatal("due()")
	}
}

func TestSettingsAndMaintenance(t *testing.T) {
	w := newWorld(t)
	s, _ := w.c.Settings(w.ctx)
	if s.TrashRetentionDays != 30 || !s.PushoverTokenSet || len(s.NotifyEvents) == 0 {
		t.Fatalf("defaults %+v", s)
	}
	s.PushoverToken = ""
	s.MaxParallelRuns = 3
	if err := w.c.SaveSettings(w.ctx, s, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	if s2, _ := w.c.Settings(w.ctx); s2.PushoverToken != "x" || s2.MaxParallelRuns != 3 {
		t.Fatalf("token was not kept: %+v", s2)
	}
	s.MaxParallelRuns = 99
	if err := w.c.SaveSettings(w.ctx, s, ClearSecrets{}); err == nil {
		t.Fatal("bad value accepted")
	}
	s.MaxParallelRuns = 1
	s.NotifyEvents = []string{"nope"}
	if err := w.c.SaveSettings(w.ctx, s, ClearSecrets{}); err == nil {
		t.Fatal("bad event accepted")
	}
	rep := w.c.RunMaintenance(w.ctx)
	if rep.DBSnapshot == "" || len(rep.Errors) != 0 {
		t.Fatalf("maintenance %+v", rep)
	}
	if err := w.c.TestNotification(w.ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := w.c.TestNotification(w.ctx, ChannelPushover); err != nil {
		t.Fatal(err)
	}
	if err := w.c.TestNotification(w.ctx, ChannelEmail); err == nil {
		t.Fatal("email test without email settings accepted")
	}
	if err := w.c.TestNotification(w.ctx, "fax"); err == nil {
		t.Fatal("unknown channel accepted")
	}
}

func TestEmailSettings(t *testing.T) {
	w := newWorld(t)
	s, _ := w.c.Settings(w.ctx)
	if s.EmailPort != 465 || s.EmailPasswordSet || len(s.EmailTo) != 0 {
		t.Fatalf("defaults %+v", s)
	}
	s.NotifyEvents = []string{notify.RunFailed}
	s.EmailHost, s.EmailUser, s.EmailPassword = " smtp.example.com ", "bfb@example.com", "pw"
	s.EmailTo = []string{"a@example.com, Bob <b@example.com>"}
	if err := w.c.SaveSettings(w.ctx, s, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	s2, _ := w.c.Settings(w.ctx)
	if s2.EmailHost != "smtp.example.com" || !s2.EmailPasswordSet || len(s2.EmailTo) != 2 || s2.EmailTo[1] != "Bob <b@example.com>" {
		t.Fatalf("saved %+v", s2)
	}
	s2.EmailPassword = ""
	w.c.SaveSettings(w.ctx, s2, ClearSecrets{})
	if s3, _ := w.c.Settings(w.ctx); s3.EmailPassword != "pw" {
		t.Fatal("password was not kept")
	}
	w.c.SaveSettings(w.ctx, s2, ClearSecrets{EmailPassword: true})
	if s3, _ := w.c.Settings(w.ctx); s3.EmailPasswordSet {
		t.Fatal("password was not cleared")
	}
	for _, bad := range []func(*Settings){
		func(s *Settings) { s.EmailTo = nil },
		func(s *Settings) { s.EmailTo = []string{"nope"} },
		func(s *Settings) { s.EmailUser, s.EmailFrom = "login", "" },
		func(s *Settings) { s.EmailPort = 0 },
	} {
		b := s2
		bad(&b)
		if err := w.c.SaveSettings(w.ctx, b, ClearSecrets{}); err == nil {
			t.Fatalf("accepted %+v", b)
		}
	}
}

func TestGlobalExcludesAndStopAll(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	src := filepath.Join(w.ctrlRoot, "s")
	write(t, src, "keep.txt", "k")
	write(t, src, ".DS_Store", "junk")
	write(t, src, "sub/Thumbs.db", "junk")
	write(t, src, "sub/own.skip", "x")
	job := store.Job{Name: "j", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true, Excludes: []string{"*.skip"},
		Targets: []store.Target{{NodeID: model.LocalNodeID, Path: filepath.Join(w.ctrlRoot, "b"), Enabled: true}}}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	tid := job.Targets[0].ID
	if r := w.run(RunRequest{Kind: store.KindBackup, TargetID: tid}); r.Stats.Transferred != 1 {
		t.Fatalf("with global excludes: %+v", r.Stats)
	}
	job.IgnoreGlobalExcludes = true
	if err := w.c.UpdateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	if r := w.run(RunRequest{Kind: store.KindBackup, TargetID: tid}); r.Stats.Transferred != 2 {
		t.Fatalf("without global excludes: %+v", r.Stats)
	}
	s, _ := w.c.Settings(w.ctx)
	s.GlobalExcludes = []string{"["}
	if err := w.c.SaveSettings(w.ctx, s, ClearSecrets{}); err == nil {
		t.Fatal("bad global pattern accepted")
	}

	// Stop all: a slow pre-hook keeps the run busy until it is stopped.
	os.MkdirAll(filepath.Join(w.ctrlRoot, "hooks"), 0o755)
	os.WriteFile(filepath.Join(w.ctrlRoot, "hooks", "slow.sh"), []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	job.PreHook = "slow.sh"
	w.c.UpdateJob(w.ctx, &job)
	r, err := w.c.StartRun(w.ctx, RunRequest{Kind: store.KindBackup, TargetID: tid})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := w.c.CancelAll(w.ctx); n != 1 {
		t.Fatalf("stopped %d", n)
	}
	start := time.Now()
	if r = w.wait(r); r.Status != store.StatusCancelled && r.Status != store.StatusFailed {
		t.Fatalf("status %s", r.Status)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("stop took too long")
	}

	usage, err := w.c.NodesUsage(w.ctx)
	if err != nil || len(usage) != 2 || len(usage[0].Disks) == 0 || usage[0].BackupFiles != 3 {
		t.Fatalf("usage %+v %v", usage, err)
	}
}

func TestActivityLogEndToEnd(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	s, _ := w.c.Settings(w.ctx)
	if s.LogLevel != logbook.LevelEvent {
		t.Fatalf("default log level %d", s.LogLevel)
	}
	s.LogLevel = logbook.LevelFile
	if err := w.c.SaveSettings(w.ctx, s, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(w.ctrlRoot, "share")
	write(t, src, "a.txt", "aaa")
	write(t, src, "sub/b.txt", "bb")
	job := store.Job{Name: "logged", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{{NodeID: w.nodeID, Path: filepath.Join(w.peerRoot, "backup"), Enabled: true}}}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	r := w.run(RunRequest{Kind: store.KindBackup, TargetID: job.Targets[0].ID})
	if r.Status != store.StatusSuccess {
		t.Fatalf("backup %s %s", r.Status, r.Error)
	}

	lines, total, err := w.c.Logs(w.ctx, logbook.Filter{RunID: r.ID, Sort: "ts", Asc: true})
	if err != nil || total < 3 {
		t.Fatalf("run log: %d %+v %v", total, lines, err)
	}
	if !strings.HasPrefix(lines[0].Message, "Backup started") || !strings.HasPrefix(lines[len(lines)-1].Message, "Backup finished (success): 2 files copied") {
		t.Fatalf("first/last line: %q / %q", lines[0].Message, lines[len(lines)-1].Message)
	}
	for _, l := range lines {
		if l.JobID != job.ID || l.Kind != store.KindBackup || l.Path != "" {
			t.Fatalf("main log line without job/kind, or with a path: %+v", l)
		}
	}
	// The files are in the file log.
	fileLines, _, err := w.c.FileLogs(w.ctx, logbook.Filter{RunID: r.ID})
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, l := range fileLines {
		if l.Level == logbook.FileOK {
			files++
			if l.Node != w.nodeID || l.SrcNode != model.LocalNodeID || !strings.HasPrefix(l.Message, "copied (new") || l.JobID != job.ID {
				t.Fatalf("file line: %+v", l)
			}
		}
	}
	if files != 2 {
		t.Fatalf("%d file lines, want 2", files)
	}

	// The peer filter finds the run and the peer's own events.
	peerLines, _, _ := w.c.Logs(w.ctx, logbook.Filter{Node: w.nodeID, Kind: logbook.KindPeer})
	if len(peerLines) == 0 || !strings.HasPrefix(peerLines[len(peerLines)-1].Message, "connected from") {
		t.Fatalf("peer lines: %+v", peerLines)
	}

	// Back at level 1 only errors are kept.
	s.LogLevel = logbook.LevelError
	w.c.SaveSettings(w.ctx, s, ClearSecrets{})
	write(t, src, "c.txt", "c")
	r = w.run(RunRequest{Kind: store.KindBackup, TargetID: job.Targets[0].ID})
	if _, n, _ := w.c.Logs(w.ctx, logbook.Filter{RunID: r.ID}); n != 0 {
		t.Fatalf("%d lines for a successful run at level 1", n)
	}
	w.stopPeer()
	w.waitOnline(false)
	r = w.run(RunRequest{Kind: store.KindBackup, TargetID: job.Targets[0].ID})
	failed, _, _ := w.c.Logs(w.ctx, logbook.Filter{RunID: r.ID})
	if len(failed) != 1 || failed[0].Level != logbook.LevelError || !strings.Contains(failed[0].Message, "offline") {
		t.Fatalf("failed run lines: %+v", failed)
	}
}

// backupRuns lists the backups of a target, newest first.
func (w *world) backupRuns(targetID int64) []store.Run {
	w.t.Helper()
	runs, _, err := w.c.store.ListRuns(w.ctx, store.RunFilter{TargetID: targetID, Kinds: []string{store.KindBackup}})
	if err != nil {
		w.t.Fatal(err)
	}
	return runs
}

// goOffline stops the peer and waits until the controller has recorded the
// disconnect (last_seen is written just before the "disconnected" line).
func (w *world) goOffline() {
	w.t.Helper()
	count := func() int {
		_, n, _ := w.c.Logs(w.ctx, logbook.Filter{Node: w.nodeID, Kind: logbook.KindPeer, Search: "disconnected"})
		return n
	}
	before := count()
	w.stopPeer()
	w.waitOnline(false)
	for deadline := time.Now().Add(5 * time.Second); count() == before; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			w.t.Fatal("disconnect not recorded")
		}
	}
}

func TestRunWhenPeerComesOnline(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	src := filepath.Join(w.ctrlRoot, "share")
	write(t, src, "a.txt", "aaa")
	job := store.Job{Name: "on-connect", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{{NodeID: w.nodeID, Path: filepath.Join(w.peerRoot, "backup"), Enabled: true, RunOnConnect: true}}}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	tid := job.Targets[0].ID
	if got, _ := w.c.store.GetTarget(w.ctx, tid); !got.RunOnConnect {
		t.Fatal("run_on_connect not stored")
	}

	// A short blip does not start a backup.
	w.goOffline()
	w.startPeer(w.token)
	w.waitOnline(true)
	time.Sleep(200 * time.Millisecond)
	if runs := w.backupRuns(tid); len(runs) != 0 {
		t.Fatalf("backup after a blip: %+v", runs)
	}

	// Back after an hour: the backup starts by itself.
	w.goOffline()
	if _, err := w.c.store.DB().Exec("UPDATE nodes SET last_seen = ? WHERE id = ?", time.Now().Add(-time.Hour).Unix(), w.nodeID); err != nil {
		t.Fatal(err)
	}
	w.startPeer(w.token)
	w.waitOnline(true)
	var runs []store.Run
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if runs = w.backupRuns(tid); len(runs) > 0 {
			break
		}
	}
	if len(runs) != 1 || runs[0].Trigger != "colo came online" {
		t.Fatalf("runs after coming online: %+v", runs)
	}
	if r := w.wait(runs[0]); r.Status != store.StatusSuccess || read(filepath.Join(w.peerRoot, "backup"), "a.txt") != "aaa" {
		t.Fatalf("backup on connect: %s %s", r.Status, r.Error)
	}

	// Switched off: no backup on connect.
	tg := job.Targets[0]
	tg.RunOnConnect = false
	if err := w.c.UpdateTarget(w.ctx, &tg); err != nil {
		t.Fatal(err)
	}
	w.goOffline()
	w.c.store.DB().Exec("UPDATE nodes SET last_seen = ? WHERE id = ?", time.Now().Add(-time.Hour).Unix(), w.nodeID)
	w.startPeer(w.token)
	w.waitOnline(true)
	time.Sleep(200 * time.Millisecond)
	if runs := w.backupRuns(tid); len(runs) != 1 {
		t.Fatalf("backup although run on connect is off: %d runs", len(runs))
	}
}

func TestOfflineAlertPerPeer(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	n, _ := w.c.store.GetNode(w.ctx, w.nodeID)
	if !n.OfflineNotify || n.OfflineHours != 1 {
		t.Fatalf("defaults: %+v", n)
	}
	w.stopPeer()
	w.waitOnline(false)
	offlineFor := func(d time.Duration) {
		w.c.mu.Lock()
		w.c.offSince[w.nodeID] = time.Now().Add(-d)
		delete(w.c.offNotify, w.nodeID)
		w.c.mu.Unlock()
	}
	alerts := func() int { return strings.Count(w.notes.kinds(), notify.NodeOffline) }

	offlineFor(30 * time.Minute)
	w.c.checkPeers(w.ctx)
	if alerts() != 0 {
		t.Fatal("alert before the hour was up")
	}
	offlineFor(2 * time.Hour)
	w.c.checkPeers(w.ctx)
	w.c.checkPeers(w.ctx)
	if alerts() != 1 {
		t.Fatalf("%d alerts after 2 hours, want exactly 1", alerts())
	}

	if err := w.c.SetNodeOffline(w.ctx, w.nodeID, true, 3); err != nil {
		t.Fatal(err)
	}
	offlineFor(2 * time.Hour)
	w.c.checkPeers(w.ctx)
	if alerts() != 1 {
		t.Fatal("alert before 3 hours")
	}
	if err := w.c.SetNodeOffline(w.ctx, w.nodeID, false, 3); err != nil {
		t.Fatal(err)
	}
	offlineFor(100 * time.Hour)
	w.c.checkPeers(w.ctx)
	if alerts() != 1 {
		t.Fatal("alert although notifications are off for this peer")
	}
	if err := w.c.SetNodeOffline(w.ctx, w.nodeID, true, 0); err == nil {
		t.Fatal("0 hours accepted")
	}
}

func TestPeerConnectionLog(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	run := func(tok proto.Token, want string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), peer.ConnLogName)
		cl, err := peer.OpenConnLog(p, 0)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		a := &peer.Agent{Addr: w.peerAddr, Token: tok, EP: localEP(t, t.TempDir()), Info: proto.PeerInfo{Hostname: "debug", Version: "test"},
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ConnLog: cl, MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}
		done := make(chan error)
		go func() { done <- a.Run(ctx) }()
		var log string
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			b, _ := os.ReadFile(p)
			if log = string(b); strings.Contains(log, want) {
				break
			}
		}
		cancel()
		<-done
		cl.Close()
		if !strings.Contains(log, want) {
			t.Fatalf("connection log lacks %q:\n%s", want, log)
		}
		return log
	}

	// A token for a peer the controller does not know: the steps of each
	// attempt are written with the first failures only, repeats are condensed.
	log := run(proto.NewToken(), "times in a row")
	if !strings.Contains(log, "hint: the controller does not know this peer") || !strings.Contains(log, "unknown node") ||
		strings.Count(log, "TCP connection to "+w.peerAddr+" open") != 3 {
		t.Fatalf("unknown peer log:\n%s", log)
	}

	// The right token, after the real peer stopped.
	w.stopPeer()
	w.waitOnline(false)
	tok, _ := proto.ParseToken(w.token)
	run(tok, "authenticated as node "+tok.NodeID+", connected to the controller")
}

// oldPeerEP answers like a peer of a version without permanent delete.
type oldPeerEP struct{ endpoint.Endpoint }

func (oldPeerEP) Remove(context.Context, string, string) error {
	return fmt.Errorf("%w (unknown method remove)", model.ErrUnsupported)
}

func TestPermanentDeleteChecksNodesFirst(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	src := filepath.Join(w.ctrlRoot, "share")
	write(t, src, "d/x.txt", "x")
	job := store.Job{Name: "perm", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{{NodeID: w.nodeID, Path: filepath.Join(w.peerRoot, "backup"), Enabled: true}}}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	peerEP, err := w.c.Endpoint(w.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	spec := engine.DeleteSpec{Job: job, Src: w.c.cfg.Local, Permanent: true,
		Targets: []engine.TargetEP{{Target: job.Targets[0], EP: oldPeerEP{peerEP}}}}
	err = w.c.checkCanRemove(w.ctx, spec)
	if err == nil || !strings.Contains(err.Error(), "colo runs BFBackup test, which cannot delete permanently") || !strings.Contains(err.Error(), "Nothing was deleted") {
		t.Fatalf("old peer: %v", err)
	}
	if read(src, "d/x.txt") != "x" {
		t.Fatal("the check deleted something")
	}
	// Current nodes pass.
	spec.Targets[0].EP = peerEP
	if err := w.c.checkCanRemove(w.ctx, spec); err != nil {
		t.Fatalf("current peer: %v", err)
	}
}

func TestDecisionsAndAttention(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	s, _ := w.c.Settings(w.ctx)
	if !s.Guards.History || s.Guards.Fixed || s.LogLevel != logbook.LevelEvent {
		t.Fatalf("default guards %+v", s.Guards)
	}
	s.Guards.ExtMin = 0
	if err := w.c.SaveSettings(w.ctx, s, ClearSecrets{}); err == nil {
		t.Fatal("invalid guard accepted")
	}
	s.Guards.ExtMin, s.LogLevel = 7, logbook.LevelTrace
	if err := w.c.SaveSettings(w.ctx, s, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.c.Settings(w.ctx); got.Guards.ExtMin != 7 || got.LogLevel != logbook.LevelTrace {
		t.Fatalf("saved guards %+v", got.Guards)
	}

	src := filepath.Join(w.ctrlRoot, "ask")
	write(t, src, "a.txt", "one")
	job := store.Job{Name: "ask", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true, OnConflict: store.ConflictAsk,
		Targets: []store.Target{{NodeID: model.LocalNodeID, Path: filepath.Join(w.ctrlRoot, "ask-b"), Enabled: true}}}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	tid := job.Targets[0].ID
	w.run(RunRequest{Kind: store.KindBackup, TargetID: tid})
	write(t, src, "a.txt", "two!")
	r := w.run(RunRequest{Kind: store.KindBackup, TargetID: tid})
	if r.Status != store.StatusApproval {
		t.Fatalf("status %s", r.Status)
	}
	o, _ := w.c.Overview(w.ctx)
	if len(o.Approvals) != 1 {
		t.Fatalf("approvals %d", len(o.Approvals))
	}
	// A scheduled backup does not ask the same question again.
	w.c.startScheduled(w.ctx, RunRequest{Kind: store.KindBackup, TargetID: tid, Trigger: "schedule"})
	if runs := w.backupRuns(tid); len(runs) != 2 {
		t.Fatalf("scheduled run started while a decision is pending: %d runs", len(runs))
	}
	if err := w.c.DeclineRun(w.ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.c.store.GetRun(w.ctx, r.ID); got.Status != store.StatusDeclined || read(filepath.Join(w.ctrlRoot, "ask-b"), "a.txt") != "one" {
		t.Fatalf("declined run %s", got.Status)
	}
	if err := w.c.DeclineRun(w.ctx, r.ID); err == nil {
		t.Fatal("declined twice")
	}

	// Problems stay on the attention list until dismissed.
	w.stopPeer()
	w.waitOnline(false)
	peerJob := store.Job{Name: "off", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{{NodeID: w.nodeID, Path: filepath.Join(w.peerRoot, "x"), Enabled: true}}}
	w.c.CreateJob(w.ctx, &peerJob)
	failed := w.run(RunRequest{Kind: store.KindBackup, TargetID: peerJob.Targets[0].ID})
	o, _ = w.c.Overview(w.ctx)
	if len(o.Failures) != 1 || o.Failures[0].ID != failed.ID {
		t.Fatalf("failures %+v", o.Failures)
	}
	if err := w.c.AcknowledgeRun(w.ctx, failed.ID); err != nil {
		t.Fatal(err)
	}
	if o, _ = w.c.Overview(w.ctx); len(o.Failures) != 0 {
		t.Fatal("dismissed run still listed")
	}
}

func TestTrashOverviewAndEmpty(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	src := filepath.Join(w.ctrlRoot, "share")
	dst := filepath.Join(w.peerRoot, "backup")
	write(t, src, "a.txt", "aaa")
	write(t, dst, "old.txt", "oldold")
	job := store.Job{Name: "tr", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{{NodeID: w.nodeID, Path: dst, Enabled: true}}}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	peerEP, err := w.c.Endpoint(w.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := peerEP.Trash(w.ctx, dst, "old.txt", model.TrashBatchName(time.Now(), 7)); err != nil {
		t.Fatal(err)
	}
	ov, err := w.c.Trash(w.ctx, job.ID)
	if err != nil || len(ov.Roots) != 2 {
		t.Fatalf("overview %+v %v", ov, err)
	}
	src0, peer := ov.Roots[0], ov.Roots[1]
	if src0.Node != model.LocalNodeID || src0.Status != "ok" || src0.Files != 0 || src0.Uses[0].Role != "source" {
		t.Fatalf("source root %+v", src0)
	}
	if peer.Status != "ok" || peer.Files != 1 || peer.Bytes != 6 || len(peer.Batches) != 1 || peer.Batches[0].RunID != 7 || peer.PurgeAt <= peer.Oldest {
		t.Fatalf("peer root %+v", peer)
	}
	if _, err := w.c.EmptyTrash(w.ctx, w.nodeID, w.peerRoot); !errors.Is(err, model.ErrInvalidPath) {
		t.Fatalf("emptying a non-job folder: %v", err)
	}
	if n, err := w.c.EmptyTrash(w.ctx, w.nodeID, dst); err != nil || n != 1 {
		t.Fatalf("empty = %d, %v", n, err)
	}
	if ov, _ := w.c.Trash(w.ctx, 0); ov.Roots[1].Files != 0 {
		t.Fatalf("after empty %+v", ov.Roots[1])
	}
	// A peer of an older version is listed without numbers.
	r := TrashRoot{Node: w.nodeID, Root: dst}
	w.c.countTrashWith(w.ctx, &r, oldPeerEP{peerEP}, 0)
	if r.Status != "old_peer" {
		t.Fatalf("old peer %+v", r)
	}
}

func TestTryAgain(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	src := filepath.Join(w.ctrlRoot, "retry")
	write(t, src, "a.txt", "a")
	job := store.Job{Name: "retry", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{
			{NodeID: model.LocalNodeID, Path: filepath.Join(w.ctrlRoot, "local-b"), Enabled: true},
			{NodeID: w.nodeID, Path: filepath.Join(w.peerRoot, "peer-b"), Enabled: true},
		}}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	local, remote := job.Targets[0], job.Targets[1]
	w.run(RunRequest{Kind: store.KindBackup, TargetID: local.ID})
	w.run(RunRequest{Kind: store.KindBackup, TargetID: remote.ID})

	// The peer is offline: the delete runs on the reachable target only.
	w.stopPeer()
	w.waitOnline(false)
	r := w.run(RunRequest{Kind: store.KindDelete, JobID: job.ID, Delete: &DeleteRequest{Paths: []string{"a.txt"}}})
	if r.Status != store.StatusWarning || !strings.Contains(r.Error+string(r.Details), "offline") {
		t.Fatalf("delete with an offline target: %s %q", r.Status, r.Error)
	}
	if read(filepath.Join(w.ctrlRoot, "local-b"), "a.txt") != "<missing>" || read(filepath.Join(w.peerRoot, "peer-b"), "a.txt") != "a" {
		t.Fatal("the reachable target must be done, the offline one untouched")
	}

	// Try again: only the skipped target, the old run leaves the attention list.
	w.startPeer(w.token)
	w.waitOnline(true)
	nr, err := w.c.RetryRun(w.ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Delete DeleteRequest `json:"delete"`
	}
	json.Unmarshal(nr.Details, &d)
	if len(d.Delete.TargetIDs) != 1 || d.Delete.TargetIDs[0] != remote.ID || d.Delete.FromSource {
		t.Fatalf("retry request %+v", d.Delete)
	}
	if nr = w.wait(nr); nr.Status != store.StatusSuccess || read(filepath.Join(w.peerRoot, "peer-b"), "a.txt") != "<missing>" {
		t.Fatalf("retry run %s %q", nr.Status, nr.Error)
	}
	if old, _ := w.c.store.GetRun(w.ctx, r.ID); !old.Acknowledged {
		t.Fatal("the old run must be dismissed")
	}
	// Successful runs are not tried again; a failed backup is, with its target.
	if _, err := w.c.RetryRun(w.ctx, nr.ID); err == nil {
		t.Fatal("retried a successful run")
	}
	w.stopPeer()
	w.waitOnline(false)
	failed := w.run(RunRequest{Kind: store.KindBackup, TargetID: remote.ID})
	w.startPeer(w.token)
	w.waitOnline(true)
	again, err := w.c.RetryRun(w.ctx, failed.ID)
	if err != nil || again.TargetID != remote.ID || again.Kind != store.KindBackup || !strings.Contains(again.Trigger, "try again of run") {
		t.Fatalf("retry backup %+v %v", again, err)
	}
	w.wait(again)
}

func TestSyncNowOnCopyJob(t *testing.T) {
	w := newWorld(t)
	src := filepath.Join(w.ctrlRoot, "syncnow")
	dst := filepath.Join(w.ctrlRoot, "syncnow-b")
	write(t, src, "keep.txt", "k")
	write(t, src, "gone.txt", "g")
	job := store.Job{Name: "syncnow", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true,
		Targets: []store.Target{{NodeID: model.LocalNodeID, Path: dst, Enabled: true}}}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	tgt := job.Targets[0]
	w.run(RunRequest{Kind: store.KindBackup, TargetID: tgt.ID})
	os.Remove(filepath.Join(src, "gone.txt"))

	// A plain backup of a copy job keeps the file on the target.
	w.run(RunRequest{Kind: store.KindBackup, TargetID: tgt.ID})
	if read(dst, "gone.txt") != "g" {
		t.Fatal("a copy job must keep files gone from the source")
	}
	// Only backups of copy jobs run as a sync, with a known removal.
	if _, err := w.c.StartRun(w.ctx, RunRequest{Kind: store.KindVerifyQuick, TargetID: tgt.ID, Sync: store.OldTrash}); err == nil {
		t.Fatal("a verify must not run as a sync")
	}
	if _, err := w.c.StartRun(w.ctx, RunRequest{Kind: store.KindBackup, TargetID: tgt.ID, Sync: "shred"}); err == nil {
		t.Fatal("unknown sync removal accepted")
	}
	r := w.run(RunRequest{Kind: store.KindBackup, TargetID: tgt.ID, Sync: store.OldTrash})
	if r.Status != store.StatusSuccess || r.Stats.Trashed != 1 || !strings.Contains(string(r.Details), `"sync":"trash"`) {
		t.Fatalf("sync now: %s %q %+v %s", r.Status, r.Error, r.Stats, r.Details)
	}
	if read(dst, "gone.txt") != "<missing>" || read(dst, "keep.txt") != "k" {
		t.Fatal("sync now must remove only what is gone from the source")
	}
	if j, _ := w.c.store.GetJob(w.ctx, job.ID); j.Mode != store.ModeCopy {
		t.Fatalf("the job's mode changed to %q", j.Mode)
	}

	// Delete permanently: nothing goes to the trash.
	write(t, src, "extra.txt", "x")
	w.run(RunRequest{Kind: store.KindBackup, TargetID: tgt.ID})
	os.Remove(filepath.Join(src, "extra.txt"))
	r = w.run(RunRequest{Kind: store.KindBackup, TargetID: tgt.ID, Sync: store.OldDelete})
	if r.Status != store.StatusSuccess || read(dst, "extra.txt") != "<missing>" || r.Stats.Trashed != 0 {
		t.Fatalf("sync now, delete: %s %q %+v", r.Status, r.Error, r.Stats)
	}

	// A sync job has nothing to sync once.
	job.Mode = store.ModeSync
	if err := w.c.UpdateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	if _, err := w.c.StartRun(w.ctx, RunRequest{Kind: store.KindBackup, TargetID: tgt.ID, Sync: store.OldTrash}); err == nil {
		t.Fatal("sync now accepted on a sync job")
	}
}
