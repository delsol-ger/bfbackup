package web

import (
	"errors"
	"net/http"

	"bfbackup/internal/controller"
	"bfbackup/internal/store"
)

// ---- USB drives ----

func (s *Server) handleDrives(w http.ResponseWriter, r *http.Request) {
	v, err := s.ctrl.Drives(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleInitDrive(w http.ResponseWriter, r *http.Request) {
	var req controller.InitDriveRequest
	if !readJSON(w, r, &req) {
		return
	}
	d, err := s.ctrl.InitDrive(r.Context(), req)
	if err != nil {
		var pe *controller.PartialError
		if errors.As(err, &pe) {
			// The drive is set up; something after it failed.
			writeJSON(w, http.StatusCreated, map[string]any{"drive": d, "warning": err.Error()})
			return
		}
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"drive": d})
}

// driveReq changes a drive; all fields are sent.
type driveReq struct {
	Name        string `json:"name"`
	OnConnect   bool   `json:"on_connect"`
	VerifyAfter bool   `json:"verify_after"`
	OverdueDays int    `json:"overdue_days"`
}

func (s *Server) handleUpdateDrive(w http.ResponseWriter, r *http.Request) {
	var req driveReq
	if !readJSON(w, r, &req) {
		return
	}
	d := store.Drive{UUID: r.PathValue("uuid"), Name: req.Name, OnConnect: req.OnConnect, VerifyAfter: req.VerifyAfter, OverdueDays: req.OverdueDays}
	if err := s.ctrl.UpdateDrive(r.Context(), d); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleForgetDrive(w http.ResponseWriter, r *http.Request) {
	if err := s.ctrl.ForgetDrive(r.Context(), r.PathValue("uuid")); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type driveRunReq struct {
	Kind   string `json:"kind"`
	Verify bool   `json:"verify"`
}

func (s *Server) handleRunDrive(w http.ResponseWriter, r *http.Request) {
	var req driveRunReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Kind == "" {
		req.Kind = store.KindBackup
	}
	n, err := s.ctrl.RunDrive(r.Context(), r.PathValue("uuid"), req.Kind, req.Verify, "manual")
	if err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"queued": n})
}
