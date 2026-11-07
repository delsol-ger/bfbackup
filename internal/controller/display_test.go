package controller

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bfbackup/internal/model"
	"bfbackup/internal/notify"
	"bfbackup/internal/store"
)

func TestDisplaySettings(t *testing.T) {
	w := newDriveWorld(t)
	s, err := w.c.Settings(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Theme != ThemeAuto || s.DateFormat != DateISO {
		t.Fatalf("defaults: theme %q date %q", s.Theme, s.DateFormat)
	}
	s.Theme, s.DateFormat = ThemeDark, DateDE
	s.Features = Features{USBDrives: false, Hooks: true}
	if err := w.c.SaveSettings(w.ctx, s, ClearSecrets{}); err != nil {
		t.Fatal(err)
	}
	got, _ := w.c.Settings(w.ctx)
	if got.Theme != ThemeDark || got.DateFormat != DateDE || got.Features.USBDrives || !got.Features.Hooks {
		t.Fatalf("saved: %q %q %+v", got.Theme, got.DateFormat, got.Features)
	}
	bad := got
	bad.Theme = "neon"
	if err := w.c.SaveSettings(w.ctx, bad, ClearSecrets{}); err == nil {
		t.Fatal("unknown theme accepted")
	}
	bad = got
	bad.DateFormat = "yyyy"
	if err := w.c.SaveSettings(w.ctx, bad, ClearSecrets{}); err == nil {
		t.Fatal("unknown date format accepted")
	}
}

// A fresh install has both features off; an update switches on the ones in
// use; stored switches are never overwritten.
func TestFeaturesFromUse(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	root := t.TempDir()
	open := func() *Controller {
		c, err := New(Config{DataDir: data, Version: "test", Local: localEP(t, root),
			Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			NotifierFor: func(Settings) notify.Notifier { return &fakeNotifier{} }, SchedulerTick: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	ctx := t.Context()
	if f := c.Features(ctx); f.USBDrives || f.Hooks {
		t.Fatalf("fresh install: %+v", f)
	}
	// An install from before the switches: a job with a hook, no stored key.
	os.MkdirAll(filepath.Join(root, "src"), 0o755)
	j := store.Job{Name: "with hook", SourceNode: model.LocalNodeID, SourcePath: filepath.Join(root, "src"), PreHook: "stop.sh", Enabled: true}
	if err := c.CreateJob(ctx, &j); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.DB().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, featuresKey); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c = open()
	defer c.Close()
	if f := c.Features(ctx); !f.Hooks || f.USBDrives {
		t.Fatalf("after update: %+v", f)
	}
	u, _ := c.FeaturesInUse(ctx)
	if len(u.Hooks) != 1 || u.Hooks[0] != "with hook" || len(u.USBDrives) != 0 {
		t.Fatalf("in use %+v", u)
	}
	// Switched off by the user: stays off, the hook is not run.
	c.store.SetSetting(ctx, featuresKey, `{"usb_drives":false,"hooks":false}`)
	if err := c.initFeatures(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Features(ctx).Hooks {
		t.Fatal("stored switch overwritten")
	}
	if got := c.withoutHooks(ctx, j); got.PreHook != "" {
		t.Fatal("hook kept although hooks are off")
	}
	c.store.SetSetting(ctx, featuresKey, `{"usb_drives":false,"hooks":true}`)
	if got := c.withoutHooks(ctx, j); got.PreHook != "stop.sh" {
		t.Fatal("hook removed although hooks are on")
	}
}

// With USB drive backups off, plugging a drive in does not back it up.
func TestUSBDrivesOffNoBackupOnConnect(t *testing.T) {
	w := newDriveWorld(t)
	src := filepath.Join(w.root, "user", "Photos")
	write(t, src, "a.jpg", "photo a")
	job := store.Job{Name: "Photos", SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true}
	if err := w.c.CreateJob(w.ctx, &job); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(w.root, "disks", "usb1")
	os.MkdirAll(mount, 0o755)
	w.mounts.set(mount, true)
	d, err := w.c.InitDrive(w.ctx, InitDriveRequest{Path: mount, Name: "Offsite-A", JobIDs: []int64{job.ID}, OnConnect: true})
	if err != nil {
		t.Fatal(err)
	}
	w.c.scanDrives(w.ctx)
	w.mounts.set(mount, false)
	w.c.scanDrives(w.ctx)

	w.c.store.SetSetting(w.ctx, featuresKey, `{"usb_drives":false,"hooks":true}`)
	w.mounts.set(mount, true)
	w.c.scanDrives(w.ctx)
	w.idle(d.UUID)
	if read(mount, "Photos/a.jpg") != "<missing>" {
		t.Fatal("backed up although USB drive backups are off")
	}

	w.c.store.SetSetting(w.ctx, featuresKey, `{"usb_drives":true,"hooks":true}`)
	w.mounts.set(mount, false)
	w.c.scanDrives(w.ctx)
	w.mounts.set(mount, true)
	w.c.scanDrives(w.ctx)
	w.idle(d.UUID)
	if read(mount, "Photos/a.jpg") != "photo a" {
		t.Fatal("not backed up after switching USB drive backups on")
	}
}
