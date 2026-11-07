// Package controller ties everything together on the controller: the peer
// registry, the run manager, the scheduler, maintenance and notifications.
// The web package is a thin HTTP layer over this.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/engine"
	"bfbackup/internal/fsx"
	"bfbackup/internal/logbook"
	"bfbackup/internal/match"
	"bfbackup/internal/model"
	"bfbackup/internal/notify"
	"bfbackup/internal/proto"
	"bfbackup/internal/store"
)

// Config configures a Controller.
type Config struct {
	DataDir  string // database, temp files, DB snapshots
	Version  string
	Local    *endpoint.Local
	Security proto.Security
	// RequireTLS turns away peers that connect unencrypted.
	RequireTLS bool
	Log        *slog.Logger
	Now        func() time.Time
	// NotifierFor builds the notifier from settings; tests replace it.
	NotifierFor func(Settings) notify.Notifier
	// SchedulerTick defaults to 20s.
	SchedulerTick time.Duration
	// DriveTick is how often the mounts are checked for USB drives (20s).
	DriveTick time.Duration
	// ProbeMounts lists the mounted file systems below the roots (default:
	// Local.FS.Disks); IsMount checks a mount point (fsx.IsMountPoint).
	// Tests replace both.
	ProbeMounts func() []model.DiskInfo
	IsMount     func(string) bool
}

// Controller is the central coordinator.
type Controller struct {
	cfg    Config
	store  *store.Store
	book   *logbook.Book
	engine *engine.Engine
	log    *slog.Logger

	mu        sync.Mutex
	peers     map[string]*peerConn
	offSince  map[string]time.Time // node -> when it went offline
	offNotify map[string]bool      // offline notification sent
	lowSpace  map[int64]time.Time  // target -> last low-space notification
	active    map[int64]*activeRun // run id -> run (running or queued)
	queue     []*activeRun         // runs waiting to start, first in first out
	heavyCap  int                  // parallel backup/verify/rebuild runs
	started   time.Time
	wg        sync.WaitGroup

	notSeen notSeenCache
	drv     driveState
}

// notSeenCache keeps the "not in source" count of each target. Counting
// takes seconds on a target with millions of such files, and the dashboard
// asks every few seconds. A count holds until the target's last scan or its
// file total changes (a new scan, or files deleted from the backup).
type notSeenCache struct {
	mu sync.Mutex
	m  map[int64]notSeenCount
}

type notSeenCount struct{ scan, stored, n int64 }

func (c *Controller) countNotSeen(ctx context.Context, t store.Target) int64 {
	c.notSeen.mu.Lock()
	v, ok := c.notSeen.m[t.ID]
	c.notSeen.mu.Unlock()
	if ok && v.scan == t.LastScanRun && v.stored == t.StoredFiles {
		return v.n
	}
	n, err := c.store.CountNotSeen(ctx, t.ID, t.LastScanRun)
	if err != nil {
		return v.n
	}
	c.notSeen.mu.Lock()
	if c.notSeen.m == nil {
		c.notSeen.m = map[int64]notSeenCount{}
	}
	c.notSeen.m[t.ID] = notSeenCount{t.LastScanRun, t.StoredFiles, n}
	c.notSeen.mu.Unlock()
	return n
}

// New opens the database and prepares the controller.
func New(cfg Config) (*Controller, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Security == nil {
		cfg.Security = proto.Plain{}
	}
	if cfg.NotifierFor == nil {
		cfg.NotifierFor = func(s Settings) notify.Notifier {
			var m notify.Multi
			if s.pushoverReady() {
				m = append(m, notify.Pushover{Token: s.PushoverToken, User: s.PushoverUser})
			}
			if s.emailReady() {
				m = append(m, notify.Email{Host: s.EmailHost, Port: s.EmailPort, User: s.EmailUser,
					Password: s.EmailPassword, From: s.EmailFrom, To: s.EmailTo})
			}
			return m
		}
	}
	if cfg.SchedulerTick <= 0 {
		cfg.SchedulerTick = 20 * time.Second
	}
	if cfg.DriveTick <= 0 {
		cfg.DriveTick = 20 * time.Second
	}
	if cfg.ProbeMounts == nil && cfg.Local != nil {
		cfg.ProbeMounts = cfg.Local.FS.Disks
	}
	if cfg.ProbeMounts == nil {
		cfg.ProbeMounts = func() []model.DiskInfo { return nil }
	}
	if cfg.IsMount == nil {
		cfg.IsMount = fsx.IsMountPoint
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "bfbackup.db"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	st.SetClock(cfg.Now)
	book, err := logbook.Open(filepath.Join(cfg.DataDir, "logs.db"))
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("open log database: %w", err)
	}
	book.SetClock(cfg.Now)
	c := &Controller{
		cfg:   cfg,
		store: st,
		book:  book,
		engine: &engine.Engine{
			Store:  st,
			TmpDir: filepath.Join(cfg.DataDir, "tmp"),
			Log:    cfg.Log,
			Now:    cfg.Now,
		},
		log:       cfg.Log,
		peers:     map[string]*peerConn{},
		offSince:  map[string]time.Time{},
		offNotify: map[string]bool{},
		lowSpace:  map[int64]time.Time{},
		active:    map[int64]*activeRun{},
		started:   cfg.Now(),
	}
	s, err := c.Settings(context.Background())
	if err != nil {
		book.Close()
		st.Close()
		return nil, err
	}
	c.setHeavyCap(s.MaxParallelRuns)
	book.SetLevel(s.LogLevel)
	if err := c.initFeatures(context.Background()); err != nil {
		c.log.Warn("feature switches", "err", err)
	}
	c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindSystem, Node: model.LocalNodeID,
		Message: "controller started, version " + cfg.Version})
	if n, err := st.MarkInterrupted(context.Background()); err == nil && n > 0 {
		c.log.Warn("marked runs from a previous process as interrupted", "count", n)
		c.logEvent(logbook.Entry{Level: logbook.LevelError, Kind: logbook.KindSystem,
			Message: fmt.Sprintf("%d run(s) were interrupted by a restart of the controller", n)})
	}
	os.RemoveAll(c.engine.TmpDir)
	return c, nil
}

// Store gives access to the database (web layer, tests).
func (c *Controller) Store() *store.Store { return c.store }

// Version returns the build version.
func (c *Controller) Version() string { return c.cfg.Version }

// Start launches the peer listener (if ln is not nil), the scheduler and
// maintenance. They stop when ctx ends.
func (c *Controller) Start(ctx context.Context, ln net.Listener) {
	if ln != nil {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.ServePeers(ctx, ln)
		}()
	}
	c.wg.Add(3)
	go func() {
		defer c.wg.Done()
		c.schedulerLoop(ctx)
	}()
	go func() {
		defer c.wg.Done()
		c.drivesLoop(ctx)
	}()
	go func() {
		defer c.wg.Done()
		c.watchLoop(ctx)
	}()
}

// Close waits for background loops and active runs, then closes the store.
func (c *Controller) Close() error {
	c.mu.Lock()
	for _, ar := range c.active {
		ar.cancel()
	}
	for _, p := range c.peers {
		p.sess.Close()
	}
	c.mu.Unlock()
	c.wg.Wait()
	c.book.Close()
	return c.store.Close()
}

// logEvent writes to the activity log (dropped when above the log level).
func (c *Controller) logEvent(e logbook.Entry) { c.book.Add(e) }

// Logs returns one page of the activity log.
func (c *Controller) Logs(ctx context.Context, f logbook.Filter) ([]logbook.Entry, int, error) {
	return c.book.Query(ctx, f)
}

// FileLogs returns one page of the file log.
func (c *Controller) FileLogs(ctx context.Context, f logbook.Filter) ([]logbook.Entry, int, error) {
	return c.book.QueryFiles(ctx, f)
}

// LogLevel is the level the activity log keeps.
func (c *Controller) LogLevel() int { return c.book.Level() }

// runActivity logs on behalf of one run; it fills in the run's job, target
// and nodes.
type runActivity struct {
	book *logbook.Book
	base logbook.Entry
	last *lastLine // the newest run-level line, for the progress; may be nil
}

// lastLine keeps the newest run-level log line of a run.
type lastLine struct {
	mu  sync.Mutex
	msg string
	at  time.Time
}

func (l *lastLine) get() (string, time.Time) {
	if l == nil {
		return "", time.Time{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.msg, l.at
}

func (a runActivity) Enabled(level int) bool { return a.book.Enabled(level) }

func (a runActivity) Log(level int, node, path, msg string) {
	e := a.base
	e.Level, e.Path, e.Message = level, path, msg
	if a.last != nil && path == "" && level <= logbook.LevelEvent {
		a.last.mu.Lock()
		a.last.msg, a.last.at = msg, time.Now()
		a.last.mu.Unlock()
	}
	if node != "" && node != e.Node {
		// Something on the other side (e.g. a hook on the source).
		e.Node, e.SrcNode = node, ""
	}
	a.book.Add(e)
}

func (c *Controller) setHeavyCap(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 {
		n = 1
	}
	c.heavyCap = n
	// More slots can start queued runs; fewer let running runs finish.
	c.pumpLocked()
}

func (c *Controller) audit(ctx context.Context, action, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	if err := c.store.Audit(ctx, action, detail); err != nil {
		c.log.Error("audit log write failed", "err", err)
	}
	c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindAudit, Message: action + ": " + detail})
}

// ---- settings ----

// Settings are the user-editable global settings.
type Settings struct {
	PushoverToken    string `json:"pushover_token,omitempty"`
	PushoverTokenSet bool   `json:"pushover_token_set"`
	PushoverUser     string `json:"pushover_user"`
	// Email goes out by SMTP over implicit TLS (SMTPS).
	EmailHost          string   `json:"email_host"`
	EmailPort          int      `json:"email_port"`
	EmailUser          string   `json:"email_user"`
	EmailPassword      string   `json:"email_password,omitempty"`
	EmailPasswordSet   bool     `json:"email_password_set"`
	EmailFrom          string   `json:"email_from"`
	EmailTo            []string `json:"email_to"`
	NotifyEvents       []string `json:"notify_events"`
	TrashRetentionDays int      `json:"trash_retention_days"`
	RunRetentionDays   int      `json:"run_retention_days"`
	MaxParallelRuns    int      `json:"max_parallel_runs"`
	LowSpacePct        int      `json:"low_space_pct"`
	DBBackupKeep       int      `json:"db_backup_keep"`
	MaintenanceHour    int      `json:"maintenance_hour"`
	// StallMinutes stops a run that makes no progress this long; 0 = never.
	StallMinutes int `json:"stall_minutes"`
	// Activity log: level 1 errors, 2 events, 3 every file.
	LogLevel         int `json:"log_level"`
	LogRetentionDays int `json:"log_retention_days"`
	LogMaxEntries    int `json:"log_max_entries"`
	// GlobalExcludes apply to every job that does not opt out.
	GlobalExcludes []string `json:"global_excludes"`
	// Guards are the anomaly checks of every backup.
	Guards engine.Guards `json:"guards"`
	// Theme and DateFormat are how the web UI looks: Theme* and Date*.
	Theme      string `json:"theme"`
	DateFormat string `json:"date_format"`
	// Features switches optional parts on and off.
	Features Features `json:"features"`
}

// DefaultGlobalExcludes are junk files that never need a backup.
var DefaultGlobalExcludes = []string{
	"# macOS",
	".DS_Store",
	"._*",
	".AppleDouble/",
	".Spotlight-V100/",
	".Trashes/",
	".fseventsd/",
	".TemporaryItems/",
	"# Windows",
	"Thumbs.db",
	"desktop.ini",
	"$RECYCLE.BIN/",
	"# Recycle bins and trash",
	".Recycle.Bin/",
	".Trash-*/",
	"# Temporary and partial downloads",
	"*.tmp",
	"*.part",
	"*.crdownload",
	"*.!qB",
	"~$*",
	".~lock.*#",
}

var settingDefaults = map[string]string{
	"trash_retention_days": "30",
	"run_retention_days":   "365",
	"max_parallel_runs":    "2",
	"low_space_pct":        "10",
	"db_backup_keep":       "7",
	"maintenance_hour":     "4",
	"email_port":           strconv.Itoa(notify.DefaultSMTPPort),
	"stall_minutes":        strconv.Itoa(defaultStallMinutes),
	"log_level":            "2",
	"log_retention_days":   "30",
	"log_max_entries":      "1000000",
	"theme":                ThemeAuto,
	"date_format":          DateISO,
}

func defaultEvents() string {
	var ev []string
	for _, k := range notify.AllKinds {
		if k.Default {
			ev = append(ev, k.Kind)
		}
	}
	return strings.Join(ev, ",")
}

// Settings loads settings with defaults. The Pushover token and the email
// password are included; the web layer strips them.
func (c *Controller) Settings(ctx context.Context) (Settings, error) {
	all, err := c.store.Settings(ctx)
	if err != nil {
		return Settings{}, err
	}
	get := func(k string) string {
		if v, ok := all[k]; ok {
			return v
		}
		return settingDefaults[k]
	}
	num := func(k string) int {
		n, err := strconv.Atoi(get(k))
		if err != nil {
			n, _ = strconv.Atoi(settingDefaults[k])
		}
		return n
	}
	ev, ok := all["notify_events"]
	if !ok {
		ev = defaultEvents()
	}
	s := Settings{
		PushoverToken:      all["pushover_token"],
		PushoverUser:       all["pushover_user"],
		EmailHost:          all["email_host"],
		EmailPort:          num("email_port"),
		EmailUser:          all["email_user"],
		EmailPassword:      all["email_password"],
		EmailFrom:          all["email_from"],
		EmailTo:            splitList(all["email_to"]),
		NotifyEvents:       splitList(ev),
		TrashRetentionDays: num("trash_retention_days"),
		RunRetentionDays:   num("run_retention_days"),
		MaxParallelRuns:    num("max_parallel_runs"),
		LowSpacePct:        num("low_space_pct"),
		DBBackupKeep:       num("db_backup_keep"),
		MaintenanceHour:    num("maintenance_hour"),
		StallMinutes:       num("stall_minutes"),
		LogLevel:           num("log_level"),
		LogRetentionDays:   num("log_retention_days"),
		LogMaxEntries:      num("log_max_entries"),
		Guards:             engine.DefaultGuards(),
		Theme:              get("theme"),
		DateFormat:         get("date_format"),
		Features:           c.featuresFrom(ctx, all),
	}
	if !themes[s.Theme] {
		s.Theme = ThemeAuto
	}
	if !dateFormats[s.DateFormat] {
		s.DateFormat = DateISO
	}
	if g, ok := all["guards"]; ok {
		// Unknown or missing fields keep their defaults.
		json.Unmarshal([]byte(g), &s.Guards)
	}
	s.PushoverTokenSet = s.PushoverToken != ""
	s.EmailPasswordSet = s.EmailPassword != ""
	if ge, ok := all["global_excludes"]; ok {
		s.GlobalExcludes = splitLines(ge)
	} else {
		s.GlobalExcludes = append([]string{}, DefaultGlobalExcludes...)
	}
	return s, nil
}

func splitLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// effectiveExcludes returns the job's exclusions plus the global ones.
func (c *Controller) effectiveExcludes(ctx context.Context, j store.Job) ([]string, error) {
	if j.IgnoreGlobalExcludes {
		return j.Excludes, nil
	}
	s, err := c.Settings(ctx)
	if err != nil {
		return nil, err
	}
	return append(append([]string{}, s.GlobalExcludes...), j.Excludes...), nil
}

func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s Settings) pushoverReady() bool { return s.PushoverToken != "" && s.PushoverUser != "" }
func (s Settings) emailReady() bool    { return s.EmailHost != "" && len(s.EmailTo) > 0 }
func (s Settings) notifyReady() bool   { return s.pushoverReady() || s.emailReady() }

// ClearSecrets names the stored secrets SaveSettings removes.
type ClearSecrets struct {
	PushoverToken, EmailPassword bool
}

// SaveSettings validates and stores settings. An empty Pushover token or
// email password keeps the stored one; clear removes it.
func (c *Controller) SaveSettings(ctx context.Context, s Settings, clear ClearSecrets) error {
	checks := []struct {
		name      string
		v, lo, hi int
	}{
		{"trash retention days", s.TrashRetentionDays, 1, 3650},
		{"run retention days", s.RunRetentionDays, 7, 3650},
		{"parallel runs", s.MaxParallelRuns, 1, 8},
		{"low space percent", s.LowSpacePct, 0, 50},
		{"database backups to keep", s.DBBackupKeep, 1, 100},
		{"maintenance hour", s.MaintenanceHour, 0, 23},
		{"minutes without progress", s.StallMinutes, 0, 1440},
		{"log level", s.LogLevel, logbook.LevelError, logbook.LevelTrace},
		{"log retention days", s.LogRetentionDays, 1, 3650},
		{"log entries to keep", s.LogMaxEntries, 10000, 100000000},
		{"SMTP port", s.EmailPort, 1, 65535},
	}
	for _, ch := range checks {
		if ch.v < ch.lo || ch.v > ch.hi {
			return fmt.Errorf("%s must be between %d and %d", ch.name, ch.lo, ch.hi)
		}
	}
	known := map[string]bool{}
	for _, k := range notify.AllKinds {
		known[k.Kind] = true
	}
	for _, e := range s.NotifyEvents {
		if !known[e] {
			return fmt.Errorf("unknown notification event %q", e)
		}
	}
	s.EmailHost, s.EmailUser, s.EmailFrom = strings.TrimSpace(s.EmailHost), strings.TrimSpace(s.EmailUser), strings.TrimSpace(s.EmailFrom)
	var to []string
	for _, t := range s.EmailTo {
		for _, a := range splitList(t) {
			if _, err := mail.ParseAddress(a); err != nil {
				return fmt.Errorf("email recipient %q is not an address", a)
			}
			to = append(to, a)
		}
	}
	if s.EmailHost != "" {
		if len(to) == 0 {
			return errors.New("email: add at least one recipient")
		}
		from := s.EmailFrom
		if from == "" {
			from = s.EmailUser
		}
		if _, err := mail.ParseAddress(from); err != nil {
			return errors.New("email: set a sender address (or use an email address as the user name)")
		}
	}
	if _, err := match.New(s.GlobalExcludes); err != nil {
		return fmt.Errorf("global exclusions: %w", err)
	}
	if err := s.Guards.Validate(); err != nil {
		return err
	}
	if s.Theme == "" {
		s.Theme = ThemeAuto
	}
	if s.DateFormat == "" {
		s.DateFormat = DateISO
	}
	if err := validateDisplay(s.Theme, s.DateFormat); err != nil {
		return err
	}
	guards, _ := json.Marshal(s.Guards)
	features, _ := json.Marshal(s.Features)
	kv := map[string]string{
		"pushover_user":        strings.TrimSpace(s.PushoverUser),
		"notify_events":        strings.Join(s.NotifyEvents, ","),
		"email_host":           s.EmailHost,
		"email_port":           strconv.Itoa(s.EmailPort),
		"email_user":           s.EmailUser,
		"email_from":           s.EmailFrom,
		"email_to":             strings.Join(to, ","),
		"trash_retention_days": strconv.Itoa(s.TrashRetentionDays),
		"run_retention_days":   strconv.Itoa(s.RunRetentionDays),
		"max_parallel_runs":    strconv.Itoa(s.MaxParallelRuns),
		"low_space_pct":        strconv.Itoa(s.LowSpacePct),
		"db_backup_keep":       strconv.Itoa(s.DBBackupKeep),
		"maintenance_hour":     strconv.Itoa(s.MaintenanceHour),
		"stall_minutes":        strconv.Itoa(s.StallMinutes),
		"log_level":            strconv.Itoa(s.LogLevel),
		"log_retention_days":   strconv.Itoa(s.LogRetentionDays),
		"log_max_entries":      strconv.Itoa(s.LogMaxEntries),
		"guards":               string(guards),
		"theme":                s.Theme,
		"date_format":          s.DateFormat,
		featuresKey:            string(features),
		"global_excludes":      strings.Join(splitLines(strings.Join(s.GlobalExcludes, "\n")), "\n"),
	}
	if t := strings.TrimSpace(s.PushoverToken); t != "" {
		kv["pushover_token"] = t
	} else if clear.PushoverToken {
		kv["pushover_token"] = ""
	}
	if p := s.EmailPassword; p != "" {
		kv["email_password"] = p
	} else if clear.EmailPassword {
		kv["email_password"] = ""
	}
	for k, v := range kv {
		if err := c.store.SetSetting(ctx, k, v); err != nil {
			return err
		}
	}
	c.setHeavyCap(s.MaxParallelRuns)
	c.book.SetLevel(s.LogLevel)
	c.audit(ctx, "settings", "settings updated")
	return nil
}

// Notify sends an event if the user subscribed to its kind. Errors are
// logged, never returned: a broken notifier must not break backups.
func (c *Controller) Notify(ctx context.Context, ev notify.Event) {
	s, err := c.Settings(ctx)
	if err != nil {
		c.log.Error("notify: settings", "err", err)
		return
	}
	if ev.Kind != notify.Test {
		wanted := false
		for _, k := range s.NotifyEvents {
			if k == ev.Kind {
				wanted = true
			}
		}
		if !wanted {
			return
		}
	}
	if !s.notifyReady() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := c.cfg.NotifierFor(s).Notify(ctx, ev); err != nil {
		c.log.Warn("notification failed", "kind", ev.Kind, "err", err)
		c.logEvent(logbook.Entry{Level: logbook.LevelError, Kind: logbook.KindSystem, Message: fmt.Sprintf("notification %q failed: %v", ev.Title, err)})
	}
}

// Notification channels for TestNotification.
const (
	ChannelPushover = "pushover"
	ChannelEmail    = "email"
)

// TestNotification sends a test message through one channel ("" = every
// channel that is set up) and returns the error, if any.
func (c *Controller) TestNotification(ctx context.Context, channel string) error {
	s, err := c.Settings(ctx)
	if err != nil {
		return err
	}
	switch channel {
	case "":
		if !s.notifyReady() {
			return errors.New("set up Pushover or email first")
		}
	case ChannelPushover:
		if !s.pushoverReady() {
			return errors.New("set the Pushover user key and application token first")
		}
		s.EmailHost = ""
	case ChannelEmail:
		if !s.emailReady() {
			return errors.New("set the SMTP server and a recipient first")
		}
		s.PushoverToken = ""
	default:
		return fmt.Errorf("unknown notification channel %q", channel)
	}
	return c.cfg.NotifierFor(s).Notify(ctx, notify.Event{Kind: notify.Test, Title: "BFBackup", Message: "Test notification from BFBackup."})
}
