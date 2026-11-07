package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"bfbackup/internal/store"
)

// Appearance of the web UI.
const (
	ThemeAuto  = "auto" // follow the system (default)
	ThemeLight = "light"
	ThemeDark  = "dark"
)

// Date formats of the web UI.
const (
	DateISO = "iso" // 2026-11-07 14:30 (default)
	DateDE  = "de"  // 07.11.2026 14:30
	DateUK  = "uk"  // 07/11/2026 14:30
	DateUS  = "us"  // 11/07/2026 2:30 PM
)

var (
	themes      = map[string]bool{ThemeAuto: true, ThemeLight: true, ThemeDark: true}
	dateFormats = map[string]bool{DateISO: true, DateDE: true, DateUK: true, DateUS: true}
)

// Features are optional parts of BFBackup that can be switched off.
type Features struct {
	// USBDrives: the USB drives page; off ignores plugged in drives.
	USBDrives bool `json:"usb_drives"`
	// Hooks: scripts before and after a backup; off does not run them.
	Hooks bool `json:"hooks"`
}

// FeaturesInUse names what uses each feature: registered USB drives and
// jobs with hook scripts.
type FeaturesInUse struct {
	USBDrives []string `json:"usb_drives"`
	Hooks     []string `json:"hooks"`
}

const featuresKey = "features"

// FeaturesInUse lists the drives and the jobs with hooks.
func (c *Controller) FeaturesInUse(ctx context.Context) (FeaturesInUse, error) {
	u := FeaturesInUse{USBDrives: []string{}, Hooks: []string{}}
	drives, err := c.store.ListDrives(ctx)
	if err != nil {
		return u, err
	}
	for _, d := range drives {
		u.USBDrives = append(u.USBDrives, d.Name)
	}
	jobs, err := c.store.ListJobs(ctx)
	if err != nil {
		return u, err
	}
	for _, j := range jobs {
		if j.PreHook != "" || j.PostHook != "" {
			u.Hooks = append(u.Hooks, j.Name)
		}
	}
	return u, nil
}

// featuresFrom reads the stored switches. Without stored switches (an
// install from before 1.16 or an older configuration export) a feature is on
// when it is in use, so existing setups keep working.
func (c *Controller) featuresFrom(ctx context.Context, all map[string]string) Features {
	if v, ok := all[featuresKey]; ok {
		var f Features
		if json.Unmarshal([]byte(v), &f) == nil {
			return f
		}
	}
	u, err := c.FeaturesInUse(ctx)
	if err != nil {
		c.log.Warn("features in use", "err", err)
		return Features{USBDrives: true, Hooks: true}
	}
	return Features{USBDrives: len(u.USBDrives) > 0, Hooks: len(u.Hooks) > 0}
}

// Features returns the current feature switches.
func (c *Controller) Features(ctx context.Context) Features {
	all, err := c.store.Settings(ctx)
	if err != nil {
		c.log.Warn("settings", "err", err)
		return Features{USBDrives: true, Hooks: true}
	}
	return c.featuresFrom(ctx, all)
}

// initFeatures stores the switches once, on the first start of a version
// that has them: on for the features already in use, off otherwise.
func (c *Controller) initFeatures(ctx context.Context) error {
	all, err := c.store.Settings(ctx)
	if err != nil {
		return err
	}
	if _, ok := all[featuresKey]; ok {
		return nil
	}
	f := c.featuresFrom(ctx, all)
	b, _ := json.Marshal(f)
	if err := c.store.SetSetting(ctx, featuresKey, string(b)); err != nil {
		return err
	}
	c.log.Info("feature switches set from what is in use", "usb_drives", f.USBDrives, "hooks", f.Hooks)
	return nil
}

func validateDisplay(theme, date string) error {
	if !themes[theme] {
		return fmt.Errorf("unknown appearance %q", theme)
	}
	if !dateFormats[date] {
		return fmt.Errorf("unknown date format %q", date)
	}
	return nil
}

// withoutHooks returns the job with its hooks removed when hooks are off.
func (c *Controller) withoutHooks(ctx context.Context, j store.Job) store.Job {
	if (j.PreHook != "" || j.PostHook != "") && !c.Features(ctx).Hooks {
		c.log.Info("hooks are switched off, not running them", "job", j.Name)
		j.PreHook, j.PostHook = "", ""
	}
	return j
}
