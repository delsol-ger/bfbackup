package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"time"

	"bfbackup/internal/controller"
	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/notify"
	"bfbackup/internal/store"
)

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	st, err := s.ctrl.Settings(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":      s.ctrl.Version(),
		"notify_kinds": notify.AllKinds,
		"local_node":   model.LocalNodeID,
		// How the UI looks and which optional parts it shows.
		"theme":       st.Theme,
		"date_format": st.DateFormat,
		"features":    st.Features,

		"default_global_excludes": controller.DefaultGlobalExcludes,
	})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	o, err := s.ctrl.Overview(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// ---- nodes ----

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	n, err := s.ctrl.Nodes(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleNodesUsage(w http.ResponseWriter, r *http.Request) {
	n, err := s.ctrl.NodesUsage(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

type nameReq struct {
	Name string `json:"name"`
}

func (s *Server) handleAddNode(w http.ResponseWriter, r *http.Request) {
	var req nameReq
	if !readJSON(w, r, &req) {
		return
	}
	n, tok, err := s.ctrl.AddNode(r.Context(), req.Name)
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"node": n, "token": tok})
}

// nodeReq changes a peer; fields left out stay as they are.
type nodeReq struct {
	Name          *string `json:"name"`
	OfflineNotify *bool   `json:"offline_notify"`
	OfflineHours  *int    `json:"offline_hours"`
}

func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	var req nodeReq
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if req.Name != nil {
		if err := s.ctrl.RenameNode(r.Context(), id, *req.Name); err != nil {
			s.fail(w, r, err, http.StatusBadRequest)
			return
		}
	}
	if req.OfflineNotify != nil || req.OfflineHours != nil {
		n, err := s.store.GetNode(r.Context(), id)
		if err != nil {
			s.fail(w, r, err, http.StatusNotFound)
			return
		}
		notify, hours := n.OfflineNotify, n.OfflineHours
		if req.OfflineNotify != nil {
			notify = *req.OfflineNotify
		}
		if req.OfflineHours != nil {
			hours = *req.OfflineHours
		}
		if err := s.ctrl.SetNodeOffline(r.Context(), id, notify, hours); err != nil {
			s.fail(w, r, err, http.StatusBadRequest)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	if err := s.ctrl.RemoveNode(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	tok, err := s.ctrl.RotateToken(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func (s *Server) handleReconnectNode(w http.ResponseWriter, r *http.Request) {
	if err := s.ctrl.ReconnectPeer(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleNodeInfo(w http.ResponseWriter, r *http.Request) {
	info, err := s.ctrl.NodeInfo(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleListDir(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	entries, err := s.ctrl.ListDir(r.Context(), r.PathValue("id"), p)
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "entries": entries})
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := s.ctrl.Mkdir(r.Context(), r.PathValue("id"), body.Path); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- jobs and targets ----

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.ctrl.JobStatuses(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	jobs, err := s.ctrl.JobStatuses(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	for _, j := range jobs {
		if j.ID == id {
			writeJSON(w, http.StatusOK, j)
			return
		}
	}
	writeError(w, http.StatusNotFound, "job not found")
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var j store.Job
	if !readJSON(w, r, &j) {
		return
	}
	j.ID = 0
	if err := s.ctrl.CreateJob(r.Context(), &j); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

func (s *Server) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	var j store.Job
	if !readJSON(w, r, &j) {
		return
	}
	j.ID = id
	if err := s.ctrl.UpdateJob(r.Context(), &j); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.ctrl.DeleteJob(r.Context(), id); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAddTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	var t store.Target
	if !readJSON(w, r, &t) {
		return
	}
	t.ID, t.JobID = 0, id
	if err := s.ctrl.AddTarget(r.Context(), &t); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	var t store.Target
	if !readJSON(w, r, &t) {
		return
	}
	t.ID = id
	if err := s.ctrl.UpdateTarget(r.Context(), &t); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.ctrl.RemoveTarget(r.Context(), id); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type runReq struct {
	Kind        string `json:"kind"`
	Repair      bool   `json:"repair"`
	Hash        bool   `json:"hash"`
	Sync        bool   `json:"sync"`         // backup of a copy job, once as a sync
	SyncRemoval string `json:"sync_removal"` // trash (default) or delete
}

var targetRunKinds = map[string]bool{
	store.KindBackup: true, store.KindDryRun: true, store.KindVerifyQuick: true,
	store.KindVerifyDeep: true, store.KindRebuildIndex: true,
}

func (s *Server) handleRunTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	var req runReq
	if !readJSON(w, r, &req) {
		return
	}
	if !targetRunKinds[req.Kind] {
		writeError(w, http.StatusBadRequest, "unknown run kind")
		return
	}
	sync := ""
	if req.Sync {
		sync = req.SyncRemoval
		if sync == "" {
			sync = store.OldTrash
		}
	}
	run, err := s.ctrl.StartRun(r.Context(), controller.RunRequest{Kind: req.Kind, TargetID: id, Repair: req.Repair, Hash: req.Hash, Sync: sync})
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	if sync != "" {
		s.store.Audit(r.Context(), "run_sync", fmt.Sprintf("target %d, run %d (removed files: %s)", id, run.ID, sync))
	}
	if req.Kind == store.KindRebuildIndex || req.Repair {
		s.store.Audit(r.Context(), "run_"+req.Kind, fmt.Sprintf("target %d, run %d (repair=%v)", id, run.ID, req.Repair))
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	var req controller.RestoreRequest
	if !readJSON(w, r, &req) {
		return
	}
	run, err := s.ctrl.StartRun(r.Context(), controller.RunRequest{Kind: store.KindRestore, TargetID: id, Restore: &req})
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	s.store.Audit(r.Context(), "restore", fmt.Sprintf("target %d: %d path(s) to %s:%s (overwrite=%v), run %d",
		id, len(req.Paths), req.DestNode, req.DestPath, req.Overwrite, run.ID))
	writeJSON(w, http.StatusAccepted, run)
}

type deleteReq struct {
	controller.DeleteRequest
	Confirm string `json:"confirm"`
}

func (s *Server) handleDeletePaths(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	var req deleteReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Confirm != "delete" {
		writeError(w, http.StatusBadRequest, `type "delete" to confirm`)
		return
	}
	d := req.DeleteRequest
	run, err := s.ctrl.StartRun(r.Context(), controller.RunRequest{Kind: store.KindDelete, JobID: id, Delete: &d})
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	s.store.Audit(r.Context(), "delete", fmt.Sprintf("job %d: %s (source=%v, targets=%v, permanent=%v), run %d",
		id, strings.Join(d.Paths, ", "), d.FromSource, d.TargetIDs, d.Permanent, run.ID))
	writeJSON(w, http.StatusAccepted, run)
}

// ---- browse ----

type browseFile struct {
	store.IndexEntry
	NotInSource bool `json:"not_in_source"`
}

func (s *Server) target(w http.ResponseWriter, r *http.Request) (store.Target, bool) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return store.Target{}, false
	}
	t, err := s.store.GetTarget(r.Context(), id)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return t, false
	}
	return t, true
}

func wrapFiles(t store.Target, in []store.IndexEntry) []browseFile {
	out := make([]browseFile, len(in))
	for i, e := range in {
		out[i] = browseFile{IndexEntry: e, NotInSource: t.LastScanRun > 0 && e.SeenRun < t.LastScanRun}
	}
	return out
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	t, ok := s.target(w, r)
	if !ok {
		return
	}
	dir, err := model.CleanRel(r.URL.Query().Get("dir"))
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	dirs, files, err := s.store.BrowseDir(r.Context(), t.ID, dir, t.LastScanRun)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "dirs": dirs, "files": wrapFiles(t, files)})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	t, ok := s.target(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeError(w, http.StatusBadRequest, "search needs at least 2 characters")
		return
	}
	hits, err := s.store.SearchIndex(r.Context(), t.ID, q, 500)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, wrapFiles(t, hits))
}

func (s *Server) handleNotInSource(w http.ResponseWriter, r *http.Request) {
	t, ok := s.target(w, r)
	if !ok {
		return
	}
	files, total, err := s.store.NotInSource(r.Context(), t.ID, t.LastScanRun, min(max(intQuery(r, "limit", 200), 1), 1000), max(intQuery(r, "offset", 0), 0))
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "files": files})
}

func (s *Server) handleForecast(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	f, err := s.ctrl.TargetForecast(r.Context(), id)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// ---- runs ----

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.RunFilter{Limit: intQuery(r, "limit", 50), Offset: max(intQuery(r, "offset", 0), 0)}
	f.JobID = int64(intQuery(r, "job", 0))
	f.TargetID = int64(intQuery(r, "target", 0))
	f.Node = q.Get("node")
	if k := q.Get("kind"); k != "" {
		f.Kinds = strings.Split(k, ",")
	}
	if st := q.Get("status"); st != "" {
		f.Statuses = strings.Split(st, ",")
	}
	runs, total, err := s.store.ListRuns(r.Context(), f)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "runs": runs, "active": s.ctrl.ActiveRuns()})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	errs, err := s.store.ListRunErrors(r.Context(), id, 1000)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	out := map[string]any{"run": run, "errors": errs}
	if p, ok := s.ctrl.RunProgress(id); ok {
		out["progress"] = p
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.ctrl.CancelRun(r.Context(), id); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCancelAll(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"stopped": s.ctrl.CancelAll(r.Context())})
}

func (s *Server) handleApproveRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	run, err := s.ctrl.ApproveRun(r.Context(), id)
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleDeclineRun(w http.ResponseWriter, r *http.Request) {
	s.runAction(w, r, s.ctrl.DeclineRun)
}

func (s *Server) handleRetryRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	run, err := s.ctrl.RetryRun(r.Context(), id)
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleAckRun(w http.ResponseWriter, r *http.Request) {
	s.runAction(w, r, s.ctrl.AcknowledgeRun)
}

func (s *Server) runAction(w http.ResponseWriter, r *http.Request, fn func(context.Context, int64) error) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := fn(r.Context(), id); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	days := min(max(intQuery(r, "days", 30), 1), 366)
	stats, err := s.store.DailyStats(r.Context(), s.now().AddDate(0, 0, -days), int64(intQuery(r, "target", 0)))
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---- settings and system ----

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	st, err := s.ctrl.Settings(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	st.PushoverToken, st.EmailPassword = "", "" // write-only
	inUse, err := s.ctrl.FeaturesInUse(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	// The page asks before switching off a feature that is in use.
	writeJSON(w, http.StatusOK, struct {
		controller.Settings
		FeaturesInUse controller.FeaturesInUse `json:"features_in_use"`
	}{st, inUse})
}

type settingsReq struct {
	controller.Settings
	ClearPushoverToken bool `json:"clear_pushover_token"`
	ClearEmailPassword bool `json:"clear_email_password"`
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	// Fields the request leaves out keep their stored values (the secrets
	// are never sent back, an empty one keeps the saved one anyway).
	cur, err := s.ctrl.Settings(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	cur.PushoverToken, cur.EmailPassword = "", ""
	req := settingsReq{Settings: cur}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.ctrl.SaveSettings(r.Context(), req.Settings, controller.ClearSecrets{PushoverToken: req.ClearPushoverToken, EmailPassword: req.ClearEmailPassword}); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	if err := s.ctrl.TestNotification(r.Context(), r.URL.Query().Get("channel")); err != nil {
		s.fail(w, r, err, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.ctrl.RunMaintenance(r.Context()))
}

// handleTrash counts the trash of every job root, or with ?job= of one job.
func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	t, err := s.ctrl.Trash(r.Context(), int64(max(intQuery(r, "job", 0), 0)))
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

type emptyTrashReq struct {
	Node    string `json:"node"`
	Root    string `json:"root"`
	Confirm string `json:"confirm"`
}

func (s *Server) handleEmptyTrash(w http.ResponseWriter, r *http.Request) {
	var req emptyTrashReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Confirm != "empty" {
		writeError(w, http.StatusBadRequest, `type "empty" to confirm`)
		return
	}
	n, err := s.ctrl.EmptyTrash(r.Context(), req.Node, req.Root)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"batches_removed": n})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.ListAudit(r.Context(), min(max(intQuery(r, "limit", 100), 1), 1000), max(intQuery(r, "offset", 0), 0))
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	if a == nil {
		a = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.serveLogs(w, r, s.ctrl.Logs)
}

func (s *Server) handleFileLogs(w http.ResponseWriter, r *http.Request) {
	s.serveLogs(w, r, s.ctrl.FileLogs)
}

func (s *Server) serveLogs(w http.ResponseWriter, r *http.Request, query func(context.Context, logbook.Filter) ([]logbook.Entry, int, error)) {
	q := r.URL.Query()
	f := logbook.Filter{
		Level:    intQuery(r, "level", 0),
		Kind:     q.Get("kind"),
		Node:     q.Get("node"),
		JobID:    int64(intQuery(r, "job", 0)),
		TargetID: int64(intQuery(r, "target", 0)),
		RunID:    int64(intQuery(r, "run", 0)),
		Search:   q.Get("q"),
		Sort:     q.Get("sort"),
		Asc:      q.Get("dir") == "asc",
		Limit:    min(max(intQuery(r, "limit", 100), 1), 1000),
		Offset:   max(intQuery(r, "offset", 0), 0),
	}
	if h := intQuery(r, "hours", 0); h > 0 {
		f.Since = s.now().Add(-time.Duration(h) * time.Hour).UnixMilli()
	}
	entries, total, err := query(r.Context(), f)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "entries": entries, "level": s.ctrl.LogLevel()})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	ex, err := s.ctrl.ExportConfig(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.Encode(ex)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="bfbackup-config-%s.json"`, s.now().Format("20060102-150405")))
	w.Write(buf.Bytes())
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var ex store.ConfigExport
	if !readJSONLimit(w, r, &ex, maxImport) {
		return
	}
	if err := s.ctrl.ImportConfig(r.Context(), &ex); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobs": len(ex.Jobs), "nodes": len(ex.Nodes)})
}

func (s *Server) handleDBSnapshot(w http.ResponseWriter, r *http.Request) {
	dir, err := os.MkdirTemp("", "bfb-snap-")
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "bfbackup.db")
	if err := s.store.Snapshot(r.Context(), p); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	defer f.Close()
	s.store.Audit(r.Context(), "db_download", "database snapshot downloaded from "+clientIP(r))
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="bfbackup-%s.db"`, s.now().Format("20060102-150405")))
	http.ServeContent(w, r, "", time.Time{}, f)
}

// handleStacks shows what every goroutine is doing right now, to find out
// where a run hangs without stopping the server.
func (s *Server) handleStacks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	pprof.Lookup("goroutine").WriteTo(w, 2)
}
