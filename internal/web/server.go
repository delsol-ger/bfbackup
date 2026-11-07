// Package web serves the JSON API and the embedded single-page UI.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"bfbackup/internal/controller"
	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

//go:embed static
var staticFiles embed.FS

func init() {
	// Not in Go's built-in table; with nosniff the browser needs the right type.
	mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// Server is the HTTP layer.
type Server struct {
	ctrl    *controller.Controller
	store   *store.Store
	log     *slog.Logger
	now     func() time.Time
	limit   *limiter
	setupMu sync.Mutex
	static  fs.FS
	etags   map[string]string // "/path" -> quoted content hash
}

// New creates the HTTP server for a controller.
func New(c *controller.Controller, log *slog.Logger) *Server {
	sub, _ := fs.Sub(staticFiles, "static")
	s := &Server{ctrl: c, store: c.Store(), log: log, now: time.Now, static: sub, etags: map[string]string{}}
	fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		s.etags["/"+p] = `"` + hex.EncodeToString(sum[:8]) + `"`
		return nil
	})
	s.etags["/"] = s.etags["/index.html"]
	s.limit = newLimiter(5, 5*time.Minute, func() time.Time { return s.now() })
	return s
}

// InitPassword sets the admin password from the environment if none is set.
func (s *Server) InitPassword(ctx context.Context, pw string) error {
	if pw == "" {
		return nil
	}
	h, err := s.passwordHash(ctx)
	if err != nil || h != "" {
		return err
	}
	return s.SetPassword(ctx, pw)
}

const (
	maxBody   = 1 << 20
	maxImport = 50 << 20
)

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	public := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
	private := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAuth(h)) }

	public("GET /api/auth/state", s.handleAuthState)
	public("POST /api/auth/setup", s.handleSetup)
	public("POST /api/auth/login", s.handleLogin)
	public("POST /api/auth/logout", s.handleLogout)
	private("POST /api/auth/password", s.handleChangePassword)

	private("GET /api/meta", s.handleMeta)
	private("GET /api/overview", s.handleOverview)

	private("GET /api/nodes", s.handleNodes)
	private("GET /api/nodes/usage", s.handleNodesUsage)
	private("POST /api/nodes", s.handleAddNode)
	private("PATCH /api/nodes/{id}", s.handleUpdateNode)
	private("DELETE /api/nodes/{id}", s.handleDeleteNode)
	private("POST /api/nodes/{id}/token", s.handleRotateToken)
	private("POST /api/nodes/{id}/reconnect", s.handleReconnectNode)
	private("GET /api/nodes/{id}/info", s.handleNodeInfo)
	private("GET /api/nodes/{id}/ls", s.handleListDir)
	private("POST /api/nodes/{id}/mkdir", s.handleMkdir)

	private("GET /api/drives", s.handleDrives)
	private("POST /api/drives", s.handleInitDrive)
	private("PATCH /api/drives/{uuid}", s.handleUpdateDrive)
	private("DELETE /api/drives/{uuid}", s.handleForgetDrive)
	private("POST /api/drives/{uuid}/run", s.handleRunDrive)

	private("GET /api/jobs", s.handleJobs)
	private("POST /api/jobs", s.handleCreateJob)
	private("GET /api/jobs/{id}", s.handleGetJob)
	private("PUT /api/jobs/{id}", s.handleUpdateJob)
	private("DELETE /api/jobs/{id}", s.handleDeleteJob)
	private("POST /api/jobs/{id}/targets", s.handleAddTarget)
	private("POST /api/jobs/{id}/delete", s.handleDeletePaths)

	private("PUT /api/targets/{id}", s.handleUpdateTarget)
	private("DELETE /api/targets/{id}", s.handleDeleteTarget)
	private("POST /api/targets/{id}/run", s.handleRunTarget)
	private("POST /api/targets/{id}/restore", s.handleRestore)
	private("GET /api/targets/{id}/browse", s.handleBrowse)
	private("GET /api/targets/{id}/search", s.handleSearch)
	private("GET /api/targets/{id}/not-in-source", s.handleNotInSource)
	private("GET /api/targets/{id}/forecast", s.handleForecast)

	private("GET /api/runs", s.handleRuns)
	private("GET /api/runs/{id}", s.handleRun)
	private("POST /api/runs/{id}/cancel", s.handleCancelRun)
	private("POST /api/runs/cancel-all", s.handleCancelAll)
	private("POST /api/runs/{id}/approve", s.handleApproveRun)
	private("POST /api/runs/{id}/decline", s.handleDeclineRun)
	private("POST /api/runs/{id}/ack", s.handleAckRun)
	private("POST /api/runs/{id}/retry", s.handleRetryRun)

	private("GET /api/stats", s.handleStats)
	private("GET /api/settings", s.handleGetSettings)
	private("PUT /api/settings", s.handlePutSettings)
	private("POST /api/settings/test-notification", s.handleTestNotification)
	private("POST /api/maintenance", s.handleMaintenance)
	private("GET /api/trash", s.handleTrash)
	private("POST /api/trash/empty", s.handleEmptyTrash)
	private("GET /api/audit", s.handleAudit)
	private("GET /api/logs", s.handleLogs)
	private("GET /api/filelogs", s.handleFileLogs)
	private("GET /api/system/export", s.handleExport)
	private("POST /api/system/import", s.handleImport)
	private("GET /api/system/db", s.handleDBSnapshot)
	private("GET /api/system/stacks", s.handleStacks)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "unknown API endpoint")
	})
	mux.Handle("/", s.staticHandler())
	return s.securityHeaders(s.csrf(mux))
}

func (s *Server) staticHandler() http.Handler {
	files := http.FileServer(http.FS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always revalidate; the ETag makes that a cheap 304. Without this,
		// browsers keep an old app.js after an update.
		w.Header().Set("Cache-Control", "no-cache")
		if tag, ok := s.etags[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// csrf requires a custom header on every state-changing API request.
// Browsers do not send custom headers cross-origin without a CORS preflight,
// which this server never allows.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(csrfHeader) != "1" {
				writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(w, r) {
			writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return readJSONLimit(w, r, v, maxBody)
}

// fail maps errors to status codes. def is used for unknown errors.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error, def int) {
	status := def
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, controller.ErrBusy):
		status = http.StatusConflict
	case errors.Is(err, model.ErrOffline):
		status = http.StatusServiceUnavailable
	case errors.Is(err, model.ErrOutsideRoot), errors.Is(err, model.ErrInvalidPath):
		status = http.StatusBadRequest
	}
	if status >= 500 {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeError(w, status, err.Error())
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func intQuery(r *http.Request, key string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil {
		return v
	}
	return def
}
