package controller

// USB drives on the controller (see "USB drives" in docs/DESIGN.md).
//
// A drive is a virtual node "usb:<uuid>": its targets are ordinary targets
// whose paths are relative to the drive's root, so the engine, the index,
// the queue (one run per drive at a time, like a peer), browse, restore and
// verify work unchanged. A marker file at the drive's root identifies it;
// mount path and label do not matter.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/notify"
	"bfbackup/internal/store"
)

func isDriveNode(id string) bool {
	_, ok := model.DriveUUID(id)
	return ok
}

// driveMount is where a connected drive is mounted now.
type driveMount struct {
	Path        string
	FSType      string
	Total, Free uint64
}

// FoundDrive is a mounted file system that is no drive of this controller
// (yet): a new drive, one that was forgotten, or one of another controller.
type FoundDrive struct {
	Path      string `json:"path"`
	Label     string `json:"label"`
	FSType    string `json:"fstype"`
	Total     uint64 `json:"total"`
	Free      uint64 `json:"free"`
	Name      string `json:"name,omitempty"`    // from its marker
	Known     bool   `json:"known,omitempty"`   // marker of this controller, drive was forgotten
	Foreign   bool   `json:"foreign,omitempty"` // marker of another controller: never written to
	Problem   string `json:"problem,omitempty"` // cannot be used
	FSWarning string `json:"fs_warning,omitempty"`
	uuid      string
}

type driveState struct {
	scanMu  sync.Mutex // one scan at a time
	mu      sync.Mutex
	scanned bool
	online  map[string]driveMount // uuid -> mount
	found   []FoundDrive
	session map[string]*driveSession // uuid -> runs started together
}

// driveSession are the runs started for a drive at once (plug in, "Back up
// all now"); when all are done, a notification says it is safe to unmount.
type driveSession struct {
	runs    []int64
	trigger string
}

// driveMarker is the content of model.DriveMarkerName.
type driveMarker struct {
	Format     int    `json:"bfbackup_drive"`
	UUID       string `json:"uuid"`
	Name       string `json:"name"`
	Controller string `json:"controller"`
	Created    int64  `json:"created"`
}

var errNoMarker = errors.New("no BFBackup marker")

// notDriveFS are file systems that are never a USB drive.
var notDriveFS = map[string]bool{
	"fuse.shfs": true, "nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true,
	"fuse.sshfs": true, "fuse.rclone": true, "overlay": true, "9p": true,
}

// driveCandidates are the mounts below the allowed roots that may be USB
// drives: not a root itself, not the user shares, no network share.
func (c *Controller) driveCandidates() []model.DiskInfo {
	if c.cfg.Local == nil {
		return nil
	}
	roots := map[string]bool{}
	for _, r := range c.cfg.Local.FS.Roots() {
		roots[r] = true
	}
	var out []model.DiskInfo
	for _, d := range c.cfg.ProbeMounts() {
		if roots[d.Path] || notDriveFS[d.FSType] || d.Total == 0 {
			continue
		}
		if b := path.Base(d.Path); roots[path.Dir(d.Path)] && (b == "user" || b == "user0") {
			continue // Unraid user shares
		}
		if strings.Contains(d.Path, "/remotes/") || strings.Contains(d.Path, "/rootshare/") {
			continue
		}
		out = append(out, d)
	}
	return out
}

// fsWarning explains what a file system cannot store.
func fsWarning(fstype string) string {
	switch strings.ToLower(fstype) {
	case "", "xfs", "ext4", "ext3", "ext2", "btrfs", "zfs", "f2fs":
		return ""
	case "vfat", "msdos", "fat", "fat32":
		return "FAT32: files over 4 GB cannot be stored, times are only exact to 2 seconds, no permissions, some characters are not allowed in names. Use XFS, ext4 or btrfs if you can."
	case "exfat":
		return "exFAT: no permissions, some characters are not allowed in names. XFS, ext4 or btrfs are better."
	case "ntfs", "ntfs3", "fuseblk":
		return "NTFS: no Linux permissions, some characters are not allowed in names. XFS, ext4 or btrfs are better."
	}
	return ""
}

// controllerID identifies this controller in drive markers. It is a setting,
// so a restored configuration still knows its drives.
func (c *Controller) controllerID(ctx context.Context) (string, error) {
	id, err := c.store.Setting(ctx, "controller_id", "")
	if err != nil || id != "" {
		return id, err
	}
	id = randomHex(16)
	return id, c.store.SetSetting(ctx, "controller_id", id)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Controller) readMarker(ctx context.Context, mount string) (driveMarker, error) {
	var m driveMarker
	r, err := c.cfg.Local.Open(ctx, mount+"/"+model.DriveMarkerName)
	if errors.Is(err, fs.ErrNotExist) {
		return m, errNoMarker
	}
	if err != nil {
		return m, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil && !errors.Is(err, model.ErrChanged) {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Format != 1 || m.UUID == "" {
		return m, errors.New("the marker file is damaged")
	}
	return m, nil
}

func (c *Controller) writeMarker(ctx context.Context, mount string, m driveMarker) error {
	m.Format = 1
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w, err := c.cfg.Local.Create(ctx, mount+"/"+model.DriveMarkerName,
		model.Entry{Name: model.DriveMarkerName, Type: model.TypeFile, Size: int64(len(b)), Mode: 0o644, MTime: c.cfg.Now().UnixNano()})
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		w.Abort()
		return err
	}
	_, err = w.Commit(ctx, "")
	return err
}

// drivesLoop looks for plugged in and removed drives.
func (c *Controller) drivesLoop(ctx context.Context) {
	c.scanDrives(ctx)
	t := time.NewTicker(c.cfg.DriveTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.scanDrives(ctx)
	}
}

// replugGap: on the first scan after a start, a drive counts as plugged in
// only if it was not seen for this long (a restart is no plug in).
const replugGap = 10 * time.Minute

// scanDrives reads the mounts and markers, updates which drives are
// connected and starts the backups of drives that were plugged in.
func (c *Controller) scanDrives(ctx context.Context) {
	c.drv.scanMu.Lock()
	defer c.drv.scanMu.Unlock()
	drives, err := c.store.ListDrives(ctx)
	if err != nil {
		c.log.Error("drives: list", "err", err)
		return
	}
	known := map[string]store.Drive{}
	for _, d := range drives {
		known[d.UUID] = d
	}
	myID, err := c.controllerID(ctx)
	if err != nil {
		c.log.Error("drives: controller id", "err", err)
		return
	}
	online := map[string]driveMount{}
	found := []FoundDrive{}
	for _, d := range c.driveCandidates() {
		f := FoundDrive{Path: d.Path, Label: path.Base(d.Path), FSType: d.FSType, Total: d.Total, Free: d.Free, FSWarning: fsWarning(d.FSType)}
		m, err := c.readMarker(ctx, d.Path)
		switch {
		case errors.Is(err, errNoMarker):
		case err != nil:
			f.Problem = err.Error()
		case m.Controller != myID:
			f.Name, f.Foreign = m.Name, true
		case known[m.UUID].UUID == "":
			f.Name, f.Known, f.uuid = m.Name, true, m.UUID
		default:
			if other, dup := online[m.UUID]; dup {
				f.Problem = fmt.Sprintf("same drive ID as %s (a copied drive?); unplug one of them", other.Path)
				break
			}
			online[m.UUID] = driveMount{Path: d.Path, FSType: d.FSType, Total: d.Total, Free: d.Free}
			continue
		}
		found = append(found, f)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })

	c.drv.mu.Lock()
	prev, first := c.drv.online, !c.drv.scanned
	c.drv.online, c.drv.found, c.drv.scanned = online, found, true
	c.drv.mu.Unlock()

	now := c.cfg.Now()
	for uuid, m := range online {
		d := known[uuid]
		c.store.SeenDrive(ctx, uuid, m.Path, m.FSType, int64(m.Total), int64(m.Free))
		if _, was := prev[uuid]; was {
			continue
		}
		if first && d.LastSeen > 0 && now.Sub(time.Unix(d.LastSeen, 0)) < replugGap {
			continue // still connected while the controller restarted
		}
		c.driveConnected(ctx, d, m)
	}
	for uuid := range prev {
		if _, still := online[uuid]; !still {
			name := known[uuid].Name
			if name == "" {
				name = uuid
			}
			c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindDrive, Node: model.DriveNodeID(uuid),
				Message: fmt.Sprintf("USB drive %s was removed", name)})
		}
	}
	c.checkDriveSessions(ctx, known)
}

// driveConnected logs a plugged in drive and starts its backups.
func (c *Controller) driveConnected(ctx context.Context, d store.Drive, m driveMount) {
	c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindDrive, Node: model.DriveNodeID(d.UUID),
		Message: fmt.Sprintf("USB drive %s connected at %s", d.Name, m.Path)})
	if d.OnConnect {
		if !c.Features(ctx).USBDrives {
			c.log.Info("USB drive backups are switched off, not backing up", "drive", d.Name)
			return
		}
		c.RunDrive(ctx, d.UUID, store.KindBackup, d.VerifyAfter, "USB drive "+d.Name+" plugged in")
	}
}

// RunDrive queues a run of kind (backup or verify_quick) for every enabled
// target on the drive, with a quick verify after each backup if verify is
// set. It returns the number of runs queued. They run one after another.
func (c *Controller) RunDrive(ctx context.Context, uuid, kind string, verify bool, trigger string) (int, error) {
	if kind != store.KindBackup && kind != store.KindVerifyQuick && kind != store.KindVerifyDeep {
		return 0, fmt.Errorf("unknown run kind %q", kind)
	}
	d, err := c.store.GetDrive(ctx, uuid)
	if err != nil {
		return 0, err
	}
	if _, err := c.driveEndpoint(uuid); err != nil {
		return 0, fmt.Errorf("USB drive %s: %w", d.Name, err)
	}
	jobs, err := c.store.ListJobs(ctx)
	if err != nil {
		return 0, err
	}
	node := model.DriveNodeID(uuid)
	var ids []int64
	start := func(j store.Job, t store.Target, kind string) {
		e := logbook.Entry{Kind: logbook.KindDrive, JobID: j.ID, TargetID: t.ID, Node: node, SrcNode: j.SourceNode}
		if kind == store.KindBackup {
			if id := c.pendingDecision(ctx, t.ID); id != 0 {
				e.Level, e.Message = logbook.LevelEvent, fmt.Sprintf("%s: backup skipped, run #%d waits for your decision", j.Name, id)
				c.logEvent(e)
				return
			}
		}
		run, err := c.StartRun(ctx, RunRequest{Kind: kind, TargetID: t.ID, Trigger: trigger})
		if err != nil {
			e.Level, e.Message = logbook.LevelError, fmt.Sprintf("%s: %s could not start: %v", j.Name, kindLabel(kind), err)
			if errors.Is(err, ErrBusy) {
				e.Level = logbook.LevelEvent
			}
			c.logEvent(e)
			return
		}
		ids = append(ids, run.ID)
	}
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		for _, t := range j.Targets {
			if t.NodeID != node || !t.Enabled {
				continue
			}
			if _, err := c.Endpoint(j.SourceNode); err != nil {
				c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindDrive, JobID: j.ID, TargetID: t.ID, Node: node,
					Message: fmt.Sprintf("%s: skipped, the source %s is offline", j.Name, c.nodeName(ctx, j.SourceNode))})
				continue
			}
			start(j, t, kind)
			if kind == store.KindBackup && verify {
				start(j, t, store.KindVerifyQuick)
			}
		}
	}
	if len(ids) > 0 {
		c.drv.mu.Lock()
		if c.drv.session == nil {
			c.drv.session = map[string]*driveSession{}
		}
		s := c.drv.session[uuid]
		if s == nil {
			s = &driveSession{trigger: trigger}
			c.drv.session[uuid] = s
		}
		s.runs = append(s.runs, ids...)
		c.drv.mu.Unlock()
	}
	return len(ids), nil
}

// driveBusy reports whether a run uses the drive (running or queued).
func (c *Controller) driveBusy(uuid string) bool {
	node := model.DriveNodeID(uuid)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ar := range c.active {
		for _, n := range ar.nodes {
			if n == node {
				return true
			}
		}
	}
	return false
}

// checkDriveSessions sends the "done" notification of every drive whose
// session runs have all finished.
func (c *Controller) checkDriveSessions(ctx context.Context, known map[string]store.Drive) {
	// Never hold drv.mu and c.mu together.
	c.drv.mu.Lock()
	snap := map[string][]int64{}
	for uuid, s := range c.drv.session {
		snap[uuid] = append([]int64(nil), s.runs...)
	}
	c.drv.mu.Unlock()
	var done []string
	c.mu.Lock()
	for uuid, runs := range snap {
		running := false
		for _, id := range runs {
			if _, ok := c.active[id]; ok {
				running = true
			}
		}
		if !running {
			done = append(done, uuid)
		}
	}
	c.mu.Unlock()
	c.drv.mu.Lock()
	sessions := map[string]*driveSession{}
	for _, uuid := range done {
		if s := c.drv.session[uuid]; s != nil && len(s.runs) == len(snap[uuid]) { // no runs added meanwhile
			sessions[uuid] = s
			delete(c.drv.session, uuid)
		}
	}
	online := c.drv.online
	c.drv.mu.Unlock()

	for uuid, s := range sessions {
		name := known[uuid].Name
		count := map[string]int{}
		for _, id := range s.runs {
			if r, err := c.store.GetRun(ctx, id); err == nil {
				count[r.Status]++
			}
		}
		parts := []string{}
		for _, st := range []string{store.StatusSuccess, store.StatusWarning, store.StatusFailed, store.StatusApproval, store.StatusCancelled, store.StatusInterrupted, store.StatusDeclined} {
			if count[st] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", count[st], strings.ReplaceAll(st, "_", " ")))
			}
		}
		_, connected := online[uuid]
		msg := fmt.Sprintf("USB drive %s: %d run(s) finished (%s).", name, len(s.runs), strings.Join(parts, ", "))
		switch {
		case !connected:
			msg += " The drive was removed."
		case c.driveBusy(uuid):
			msg += " Other runs still use it."
		default:
			msg += " Safe to unmount."
		}
		prio := 0
		if count[store.StatusFailed]+count[store.StatusWarning]+count[store.StatusApproval]+count[store.StatusInterrupted] > 0 {
			prio = 1
		}
		c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindDrive, Node: model.DriveNodeID(uuid), Message: msg})
		c.Notify(ctx, notify.Event{Kind: notify.DriveDone, Title: "USB drive " + name, Message: msg, Priority: prio})
	}
}

// checkDrivesOverdue notifies about drives with targets that were not
// connected for their overdue days, again every overdue days.
func (c *Controller) checkDrivesOverdue(ctx context.Context) {
	if !c.Features(ctx).USBDrives {
		return // switched off: no reminders either
	}
	drives, err := c.store.ListDrives(ctx)
	if err != nil {
		return
	}
	targets, err := c.store.ListTargets(ctx)
	if err != nil {
		return
	}
	used := map[string]bool{}
	for _, t := range targets {
		used[t.NodeID] = true
	}
	now := c.cfg.Now()
	c.drv.mu.Lock()
	online := c.drv.online
	c.drv.mu.Unlock()
	for _, d := range drives {
		if _, ok := online[d.UUID]; ok || d.OverdueDays <= 0 || !used[model.DriveNodeID(d.UUID)] {
			continue
		}
		limit := time.Duration(d.OverdueDays) * 24 * time.Hour
		since := time.Unix(d.LastSeen, 0)
		if d.LastSeen == 0 {
			since = time.Unix(d.CreatedAt, 0)
		}
		if now.Sub(since) < limit || (d.OverdueNotified > 0 && now.Sub(time.Unix(d.OverdueNotified, 0)) < limit) {
			continue
		}
		days := int(now.Sub(since).Hours() / 24)
		msg := fmt.Sprintf("USB drive %s was not connected for %d days. Plug it in to bring its backups up to date.", d.Name, days)
		c.store.SetDriveOverdueNotified(ctx, d.UUID, now.Unix())
		c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindDrive, Node: model.DriveNodeID(d.UUID), Message: msg})
		c.Notify(ctx, notify.Event{Kind: notify.DriveOverdue, Title: "USB drive overdue", Message: msg})
	}
}

// driveEndpoint returns the endpoint of a connected drive after checking
// that it is mounted and carries its marker, or an error wrapping
// model.ErrOffline.
func (c *Controller) driveEndpoint(uuid string) (endpoint.Endpoint, error) {
	c.drv.mu.Lock()
	m, ok := c.drv.online[uuid]
	c.drv.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("drive not connected (%w)", model.ErrOffline)
	}
	if !c.cfg.IsMount(m.Path) {
		return nil, fmt.Errorf("drive no longer mounted at %s (%w)", m.Path, model.ErrOffline)
	}
	ctx := context.Background()
	mk, err := c.readMarker(ctx, m.Path)
	if err != nil || mk.UUID != uuid {
		return nil, fmt.Errorf("another drive is mounted at %s (%w)", m.Path, model.ErrOffline)
	}
	name := mk.Name
	if d, err := c.store.GetDrive(ctx, uuid); err == nil {
		name = d.Name
	}
	return &driveEP{local: c.cfg.Local, mount: m.Path, name: name, version: c.cfg.Version, mounted: c.cfg.IsMount}, nil
}

// driveName is the display name of a drive node.
func (c *Controller) driveName(ctx context.Context, uuid string) string {
	if d, err := c.store.GetDrive(ctx, uuid); err == nil {
		return "USB · " + d.Name
	}
	return "USB · " + uuid
}

// DriveTarget is one target on a drive.
type DriveTarget struct {
	TargetID    int64      `json:"target_id"`
	JobID       int64      `json:"job_id"`
	JobName     string     `json:"job_name"`
	SourceName  string     `json:"source_name"`
	SourcePath  string     `json:"source_path"`
	Path        string     `json:"path"`
	Enabled     bool       `json:"enabled"`
	StoredFiles int64      `json:"stored_files"`
	StoredBytes int64      `json:"stored_bytes"`
	LastBackup  *store.Run `json:"last_backup"`
	LastSuccess *store.Run `json:"last_success"`
	LastVerify  *store.Run `json:"last_verify"`
	ActiveRun   int64      `json:"active_run"`
}

// DriveView is a drive with its state and targets for the UI.
type DriveView struct {
	store.Drive
	NodeID      string        `json:"node_id"`
	Connected   bool          `json:"connected"`
	Mount       string        `json:"mount,omitempty"`
	Busy        bool          `json:"busy"`    // a run uses it (running or queued)
	Overdue     bool          `json:"overdue"` // not connected for its overdue days
	FSWarning   string        `json:"fs_warning,omitempty"`
	StoredFiles int64         `json:"stored_files"`
	StoredBytes int64         `json:"stored_bytes"`
	LastBackup  int64         `json:"last_backup"` // newest successful backup on it
	Targets     []DriveTarget `json:"targets"`
}

// DrivesView is the USB drives page.
type DrivesView struct {
	Drives []DriveView  `json:"drives"`
	Found  []FoundDrive `json:"found"`
}

// Drives scans the mounts and returns all drives and the new ones found.
func (c *Controller) Drives(ctx context.Context) (DrivesView, error) {
	c.scanDrives(ctx)
	out := DrivesView{Drives: []DriveView{}}
	drives, err := c.store.ListDrives(ctx)
	if err != nil {
		return out, err
	}
	jobs, err := c.store.ListJobs(ctx)
	if err != nil {
		return out, err
	}
	c.drv.mu.Lock()
	online := c.drv.online
	out.Found = append([]FoundDrive{}, c.drv.found...)
	c.drv.mu.Unlock()
	now := c.cfg.Now()
	for _, d := range drives {
		v := DriveView{Drive: d, NodeID: model.DriveNodeID(d.UUID), Targets: []DriveTarget{}, FSWarning: fsWarning(d.FSType)}
		if m, ok := online[d.UUID]; ok {
			v.Connected, v.Mount = true, m.Path
		}
		v.Busy = c.driveBusy(d.UUID)
		for _, j := range jobs {
			for _, t := range j.Targets {
				if t.NodeID != v.NodeID {
					continue
				}
				dt := DriveTarget{TargetID: t.ID, JobID: j.ID, JobName: j.Name, SourceName: c.nodeName(ctx, j.SourceNode), SourcePath: j.SourcePath,
					Path: t.Path, Enabled: t.Enabled && j.Enabled, StoredFiles: t.StoredFiles, StoredBytes: t.StoredBytes, ActiveRun: c.TargetBusy(t.ID)}
				dt.LastBackup, _ = c.store.LastRun(ctx, t.ID, store.KindBackup)
				dt.LastSuccess, _ = c.store.LastSuccess(ctx, t.ID, store.KindBackup)
				dt.LastVerify, _ = c.store.LastRun(ctx, t.ID, store.KindVerifyQuick, store.KindVerifyDeep)
				if dt.LastSuccess != nil && dt.LastSuccess.FinishedAt > v.LastBackup {
					v.LastBackup = dt.LastSuccess.FinishedAt
				}
				v.StoredFiles += t.StoredFiles
				v.StoredBytes += t.StoredBytes
				v.Targets = append(v.Targets, dt)
			}
		}
		since := d.LastSeen
		if since == 0 {
			since = d.CreatedAt
		}
		v.Overdue = !v.Connected && d.OverdueDays > 0 && len(v.Targets) > 0 &&
			now.Sub(time.Unix(since, 0)) >= time.Duration(d.OverdueDays)*24*time.Hour
		out.Drives = append(out.Drives, v)
	}
	return out, nil
}

// PartialError: the drive was set up, but adding a target or starting the
// backups failed.
type PartialError struct{ msg string }

func (e *PartialError) Error() string { return e.msg }

// InitDriveRequest sets up a newly found drive.
type InitDriveRequest struct {
	Path        string  `json:"path"`
	Name        string  `json:"name"`
	JobIDs      []int64 `json:"job_ids"`
	Start       bool    `json:"start"` // back up the selected jobs now
	OnConnect   bool    `json:"on_connect"`
	VerifyAfter bool    `json:"verify_after"`
	OverdueDays int     `json:"overdue_days"`
}

// folderName is the folder of a job on a drive.
func folderName(j store.Job) string {
	n := strings.TrimSpace(strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(j.Name))
	if n == "" || n == "." || n == ".." || strings.HasPrefix(n, model.TrashDirName) {
		n = fmt.Sprintf("job-%d", j.ID)
	}
	return "/" + n
}

// InitDrive writes the marker to a found drive, registers it and adds it as
// a target to the selected jobs (folder = job name).
func (c *Controller) InitDrive(ctx context.Context, req InitDriveRequest) (store.Drive, error) {
	c.scanDrives(ctx)
	c.drv.mu.Lock()
	var f *FoundDrive
	for i := range c.drv.found {
		if c.drv.found[i].Path == req.Path {
			x := c.drv.found[i]
			f = &x
		}
	}
	c.drv.mu.Unlock()
	switch {
	case f == nil:
		return store.Drive{}, fmt.Errorf("no new drive is mounted at %s", req.Path)
	case f.Foreign:
		return store.Drive{}, errors.New("this drive belongs to another BFBackup controller; it is not touched")
	case f.Problem != "":
		return store.Drive{}, errors.New(f.Problem)
	case !c.cfg.IsMount(f.Path):
		return store.Drive{}, fmt.Errorf("%s is no mount point", f.Path)
	}
	jobs := []store.Job{}
	for _, id := range req.JobIDs {
		j, err := c.store.GetJob(ctx, id)
		if err != nil {
			return store.Drive{}, fmt.Errorf("job %d: %w", id, err)
		}
		if isDriveNode(j.SourceNode) {
			return store.Drive{}, fmt.Errorf("job %q backs up a USB drive", j.Name)
		}
		jobs = append(jobs, j)
	}
	myID, err := c.controllerID(ctx)
	if err != nil {
		return store.Drive{}, err
	}
	d := store.Drive{UUID: f.uuid, Name: req.Name, OnConnect: req.OnConnect, VerifyAfter: req.VerifyAfter, OverdueDays: req.OverdueDays}
	if d.UUID == "" {
		d.UUID = randomHex(16)
	}
	if err := c.store.CreateDrive(ctx, &d); err != nil {
		return d, err
	}
	if err := c.writeMarker(ctx, f.Path, driveMarker{UUID: d.UUID, Name: d.Name, Controller: myID, Created: d.CreatedAt}); err != nil {
		c.store.DeleteDrive(ctx, d.UUID)
		return d, fmt.Errorf("write the marker file to the drive: %w", err)
	}
	c.store.SeenDrive(ctx, d.UUID, f.Path, f.FSType, int64(f.Total), int64(f.Free))
	c.drv.mu.Lock()
	online := map[string]driveMount{}
	for k, v := range c.drv.online {
		online[k] = v
	}
	online[d.UUID] = driveMount{Path: f.Path, FSType: f.FSType, Total: f.Total, Free: f.Free}
	c.drv.online = online
	found := c.drv.found[:0:0]
	for _, x := range c.drv.found {
		if x.Path != f.Path {
			found = append(found, x)
		}
	}
	c.drv.found = found
	c.drv.mu.Unlock()
	c.audit(ctx, "drive_init", "initialized USB drive %q (%s) at %s", d.Name, d.UUID, f.Path)

	var errs []string
	for _, j := range jobs {
		t := store.Target{JobID: j.ID, NodeID: model.DriveNodeID(d.UUID), Path: folderName(j), VerifyMode: "quick", Enabled: true}
		if err := c.AddTarget(ctx, &t); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", j.Name, err))
		}
	}
	if req.Start && len(jobs) > 0 {
		if _, err := c.RunDrive(ctx, d.UUID, store.KindBackup, d.VerifyAfter, "USB drive "+d.Name+" set up"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return d, &PartialError{"drive set up, but: " + strings.Join(errs, "; ")}
	}
	return d, nil
}

// UpdateDrive changes a drive's name and settings. A connected drive's
// marker gets the new name too.
func (c *Controller) UpdateDrive(ctx context.Context, d store.Drive) error {
	old, err := c.store.GetDrive(ctx, d.UUID)
	if err != nil {
		return err
	}
	old.Name, old.OnConnect, old.VerifyAfter, old.OverdueDays = d.Name, d.OnConnect, d.VerifyAfter, d.OverdueDays
	if err := c.store.UpdateDrive(ctx, &old); err != nil {
		return err
	}
	if ep, err := c.driveEndpoint(d.UUID); err == nil {
		myID, _ := c.controllerID(ctx)
		c.writeMarker(ctx, ep.(*driveEP).mount, driveMarker{UUID: old.UUID, Name: old.Name, Controller: myID, Created: old.CreatedAt})
	}
	c.audit(ctx, "drive_update", "updated USB drive %q (%s)", old.Name, old.UUID)
	return nil
}

// ForgetDrive removes a drive, its targets and their index. The drive and
// its files are not touched; plugged in again, it shows up as a known drive
// that can be set up again.
func (c *Controller) ForgetDrive(ctx context.Context, uuid string) error {
	d, err := c.store.GetDrive(ctx, uuid)
	if err != nil {
		return err
	}
	if c.driveBusy(uuid) {
		return ErrBusy
	}
	if err := c.store.DeleteDrive(ctx, uuid); err != nil {
		return err
	}
	c.drv.mu.Lock()
	online := map[string]driveMount{}
	for k, v := range c.drv.online {
		if k != uuid {
			online[k] = v
		}
	}
	c.drv.online = online
	delete(c.drv.session, uuid)
	c.drv.mu.Unlock()
	c.audit(ctx, "drive_forget", "forgot USB drive %q (%s) and its targets; files on it were kept", d.Name, uuid)
	return nil
}

// driveCounts sums up the drives for the dashboard, without a scan.
func (c *Controller) driveCounts(ctx context.Context) (total, connected, overdue int) {
	drives, err := c.store.ListDrives(ctx)
	if err != nil {
		return
	}
	targets, _ := c.store.ListTargets(ctx)
	used := map[string]bool{}
	for _, t := range targets {
		used[t.NodeID] = true
	}
	now := c.cfg.Now()
	c.drv.mu.Lock()
	defer c.drv.mu.Unlock()
	for _, d := range drives {
		total++
		if _, ok := c.drv.online[d.UUID]; ok {
			connected++
			continue
		}
		since := d.LastSeen
		if since == 0 {
			since = d.CreatedAt
		}
		if d.OverdueDays > 0 && used[model.DriveNodeID(d.UUID)] && now.Sub(time.Unix(since, 0)) >= time.Duration(d.OverdueDays)*24*time.Hour {
			overdue++
		}
	}
	return
}
