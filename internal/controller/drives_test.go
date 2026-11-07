package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bfbackup/internal/model"
	"bfbackup/internal/notify"
	"bfbackup/internal/store"
)

// fakeMounts plays the mount table for the drive tests.
type fakeMounts struct {
	mu     sync.Mutex
	mounts map[string]bool // path -> mounted
}

func (f *fakeMounts) set(p string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts[p] = on
}

func (f *fakeMounts) probe() []model.DiskInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.DiskInfo
	for p, on := range f.mounts {
		if on {
			out = append(out, model.DiskInfo{Path: p, FSType: "xfs", Total: 1 << 30, Used: 1 << 20, Free: 1<<30 - 1<<20})
		}
	}
	return out
}

func (f *fakeMounts) isMount(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mounts[p]
}

type driveWorld struct {
	t      *testing.T
	ctx    context.Context
	c      *Controller
	notes  *fakeNotifier
	root   string
	mounts *fakeMounts
	now    *time.Time
	mu     sync.Mutex
}

func newDriveWorld(t *testing.T) *driveWorld {
	t.Helper()
	now := time.Now()
	w := &driveWorld{t: t, ctx: context.Background(), notes: &fakeNotifier{}, root: t.TempDir(),
		mounts: &fakeMounts{mounts: map[string]bool{}}, now: &now}
	var err error
	w.c, err = New(Config{
		DataDir:       filepath.Join(t.TempDir(), "data"),
		Version:       "test",
		Local:         localEP(t, w.root),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		NotifierFor:   func(Settings) notify.Notifier { return w.notes },
		SchedulerTick: time.Hour,
		DriveTick:     time.Hour, // scans are started by the test
		ProbeMounts:   w.mounts.probe,
		IsMount:       w.mounts.isMount,
		Now:           func() time.Time { w.mu.Lock(); defer w.mu.Unlock(); return *w.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	w.c.store.SetSetting(w.ctx, "pushover_token", "x")
	w.c.store.SetSetting(w.ctx, "pushover_user", "y")
	// Both optional features on; a fresh install has them off.
	w.c.store.SetSetting(w.ctx, featuresKey, `{"usb_drives":true,"hooks":true}`)
	ctx, cancel := context.WithCancel(context.Background())
	w.c.Start(ctx, nil)
	t.Cleanup(func() {
		cancel()
		w.c.Close()
	})
	return w
}

func (w *driveWorld) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	*w.now = w.now.Add(d)
}

// idle waits until no run uses the drive.
func (w *driveWorld) idle(uuid string) {
	w.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !w.c.driveBusy(uuid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.t.Fatal("drive runs did not finish")
}

func (w *driveWorld) lastBackup(tid int64) *store.Run {
	w.t.Helper()
	r, err := w.c.store.LastRun(w.ctx, tid, store.KindBackup)
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

func TestUSBDriveLifecycle(t *testing.T) {
	w := newDriveWorld(t)
	src := filepath.Join(w.root, "user", "Photos")
	write(t, src, "a.jpg", "photo a")
	write(t, src, "sub/b.jpg", "photo b")
	job := store.Job{Name: "Photos", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	other := store.Job{Name: "Docs", SourceNode: model.LocalNodeID, SourcePath: filepath.Join(w.root, "user", "Docs"), Enabled: true}
	write(t, other.SourcePath, "d.txt", "doc")
	if err := w.c.CreateJob(w.ctx, &other); err != nil {
		t.Fatal(err)
	}

	// A new, unmarked drive shows up as found.
	mount := filepath.Join(w.root, "disks", "usb1")
	os.MkdirAll(mount, 0o755)
	w.mounts.set(mount, true)
	v, err := w.c.Drives(w.ctx)
	if err != nil || len(v.Drives) != 0 || len(v.Found) != 1 || v.Found[0].Path != mount || v.Found[0].Label != "usb1" {
		t.Fatalf("found %+v %v", v, err)
	}

	// Set it up with one job and back up at once, verify after.
	d, err := w.c.InitDrive(w.ctx, InitDriveRequest{Path: mount, Name: "Offsite-A", JobIDs: []int64{job.ID}, Start: true, OnConnect: true, VerifyAfter: true, OverdueDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mount, model.DriveMarkerName)); err != nil {
		t.Fatal("no marker:", err)
	}
	node := model.DriveNodeID(d.UUID)
	j, _ := w.c.store.GetJob(w.ctx, job.ID)
	if len(j.Targets) != 1 || j.Targets[0].NodeID != node || j.Targets[0].Path != "/Photos" {
		t.Fatalf("targets %+v", j.Targets)
	}
	tid := j.Targets[0].ID
	w.idle(d.UUID)
	if r := w.lastBackup(tid); r == nil || r.Status != store.StatusSuccess || r.Stats.Transferred != 2 {
		t.Fatalf("first backup %+v", r)
	}
	if read(mount, "Photos/sub/b.jpg") != "photo b" {
		t.Fatal("file not on the drive")
	}
	if r, _ := w.c.store.LastRun(w.ctx, tid, store.KindVerifyQuick); r == nil || r.Status != store.StatusSuccess {
		t.Fatalf("verify after %+v", r)
	}
	w.c.scanDrives(w.ctx)
	if !strings.Contains(w.notes.kinds(), notify.DriveDone) {
		t.Fatalf("no done notification: %s", w.notes.kinds())
	}

	// The drive view: connected, one target, sizes; the marker is hidden
	// in listings of the drive.
	v, _ = w.c.Drives(w.ctx)
	if len(v.Drives) != 1 || len(v.Found) != 0 || !v.Drives[0].Connected || v.Drives[0].StoredFiles != 2 || v.Drives[0].LastBackup == 0 {
		t.Fatalf("drive view %+v", v)
	}
	if es, err := w.c.ListDir(w.ctx, node, "/"); err != nil || len(es) != 1 || es[0].Name != "Photos" {
		t.Fatalf("drive listing %+v %v", es, err)
	}
	if es, err := w.c.ListDir(w.ctx, node, ""); err != nil || len(es) != 1 || es[0].Name != "/" {
		t.Fatalf("drive roots %+v %v", es, err)
	}

	// Rules for drive nodes.
	bad := store.Job{Name: "from usb", SourceNode: node, SourcePath: "/Photos", Enabled: true}
	if err := w.c.CreateJob(w.ctx, &bad); err == nil {
		t.Fatal("a drive as source was accepted")
	}
	if err := w.c.AddTarget(w.ctx, &store.Target{JobID: other.ID, NodeID: node, Path: "/Docs", Schedule: "@daily", Enabled: true}); err == nil {
		t.Fatal("a schedule on a drive target was accepted")
	}

	// Unplugged: the folder stays, runs fail and write nothing.
	w.mounts.set(mount, false)
	w.c.scanDrives(w.ctx)
	write(t, src, "c.jpg", "photo c")
	r, err := w.c.StartRun(w.ctx, RunRequest{Kind: store.KindBackup, TargetID: tid})
	if err != nil {
		t.Fatal(err)
	}
	w.idle(d.UUID)
	if r, _ = w.c.store.GetRun(w.ctx, r.ID); r.Status != store.StatusFailed || !strings.Contains(r.Error, "not connected") {
		t.Fatalf("offline run %s %q", r.Status, r.Error)
	}
	if read(mount, "Photos/c.jpg") != "<missing>" {
		t.Fatal("written to an unplugged drive")
	}
	if _, err := w.c.RunDrive(w.ctx, d.UUID, store.KindBackup, false, "manual"); !errors.Is(err, model.ErrOffline) {
		t.Fatalf("run drive offline: %v", err)
	}

	// Mount guard: listed as connected, but no longer a mount point.
	w.mounts.set(mount, true)
	w.c.scanDrives(w.ctx) // plugged in again: on_connect backs up
	w.idle(d.UUID)
	if read(mount, "Photos/c.jpg") != "photo c" {
		t.Fatal("replug did not back up")
	}
	ep, err := w.c.Endpoint(node)
	if err != nil {
		t.Fatal(err)
	}
	w.mounts.set(mount, false)
	if _, err := ep.Create(w.ctx, "/Photos/x", model.Entry{Name: "x", Type: model.TypeFile}); !errors.Is(err, model.ErrOffline) {
		t.Fatalf("write after unmount: %v", err)
	}
	if _, err := ep.Stat(w.ctx, []string{"/Photos/a.jpg"}); !errors.Is(err, model.ErrOffline) {
		t.Fatalf("stat after unmount: %v", err)
	}
	w.mounts.set(mount, true)

	// Another drive at the same place: not ours.
	marker, _ := os.ReadFile(filepath.Join(mount, model.DriveMarkerName))
	os.WriteFile(filepath.Join(mount, model.DriveMarkerName), []byte(strings.Replace(string(marker), d.UUID, "0000", 1)), 0o644)
	if _, err := w.c.Endpoint(node); !errors.Is(err, model.ErrOffline) {
		t.Fatalf("other drive accepted: %v", err)
	}
	os.WriteFile(filepath.Join(mount, model.DriveMarkerName), marker, 0o644)

	// Overdue: away for longer than its days, notified once.
	w.mounts.set(mount, false)
	w.c.scanDrives(w.ctx)
	w.advance(8 * 24 * time.Hour)
	w.c.checkDrivesOverdue(w.ctx)
	w.c.checkDrivesOverdue(w.ctx)
	if n := strings.Count(w.notes.kinds(), notify.DriveOverdue); n != 1 {
		t.Fatalf("overdue notifications %d: %s", n, w.notes.kinds())
	}
	if v, _ = w.c.Drives(w.ctx); !v.Drives[0].Overdue || v.Drives[0].Connected {
		t.Fatalf("overdue view %+v", v.Drives[0])
	}

	// Forget: targets and index go, the drive comes back as known.
	if err := w.c.ForgetDrive(w.ctx, d.UUID); err != nil {
		t.Fatal(err)
	}
	if j, _ = w.c.store.GetJob(w.ctx, job.ID); len(j.Targets) != 0 {
		t.Fatalf("targets after forget %+v", j.Targets)
	}
	w.mounts.set(mount, true)
	if v, _ = w.c.Drives(w.ctx); len(v.Drives) != 0 || len(v.Found) != 1 || !v.Found[0].Known || v.Found[0].Name != "Offsite-A" {
		t.Fatalf("after forget %+v", v)
	}
	// Set up again: same ID.
	d2, err := w.c.InitDrive(w.ctx, InitDriveRequest{Path: mount, Name: "Offsite-A", JobIDs: []int64{other.ID}})
	if err != nil || d2.UUID != d.UUID {
		t.Fatalf("re-init %+v %v", d2, err)
	}
}

func TestUSBDriveForeignAndRestart(t *testing.T) {
	w := newDriveWorld(t)
	mount := filepath.Join(w.root, "disks", "usb2")
	os.MkdirAll(mount, 0o755)
	os.WriteFile(filepath.Join(mount, model.DriveMarkerName),
		[]byte(`{"bfbackup_drive":1,"uuid":"abc","name":"Theirs","controller":"someone-else"}`), 0o644)
	w.mounts.set(mount, true)
	v, _ := w.c.Drives(w.ctx)
	if len(v.Found) != 1 || !v.Found[0].Foreign {
		t.Fatalf("foreign %+v", v)
	}
	if _, err := w.c.InitDrive(w.ctx, InitDriveRequest{Path: mount, Name: "Mine"}); err == nil {
		t.Fatal("foreign drive was set up")
	}
	// The user shares and the roots are never drives.
	w.mounts.set(w.root, true)
	w.mounts.set(filepath.Join(w.root, "user"), true)
	if v, _ = w.c.Drives(w.ctx); len(v.Found) != 1 {
		t.Fatalf("candidates %+v", v.Found)
	}
}
