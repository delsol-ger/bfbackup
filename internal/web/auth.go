package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie   = "bfb_session"
	sessionLifetime = 30 * 24 * time.Hour // sliding: every use renews it
	sessionRenew    = time.Hour           // renew at most this often, so polling does not write each time
	passwordKey     = "admin_password_hash"
	csrfHeader      = "X-BFB-Request"
	minPasswordLen  = 8
	bcryptCost      = 12
)

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func (s *Server) passwordHash(ctx context.Context) (string, error) {
	return s.store.Setting(ctx, passwordKey, "")
}

// SetPassword stores a new admin password (bcrypt).
func (s *Server) SetPassword(ctx context.Context, pw string) error {
	if len(pw) < minPasswordLen {
		return errors.New("the password must have at least 8 characters")
	}
	if len(pw) > 72 {
		return errors.New("the password must not be longer than 72 bytes")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return err
	}
	return s.store.SetSetting(ctx, passwordKey, string(h))
}

func (s *Server) checkPassword(ctx context.Context, pw string) (bool, error) {
	h, err := s.passwordHash(ctx)
	if err != nil || h == "" {
		return false, err
	}
	return bcrypt.CompareHashAndPassword([]byte(h), []byte(pw)) == nil, nil
}

func (s *Server) newSession(w http.ResponseWriter, r *http.Request) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	exp := s.now().Add(sessionLifetime)
	if err := s.store.CreateSession(r.Context(), hashToken(tok), exp); err != nil {
		return err
	}
	setSessionCookie(w, r, tok, exp)
	return nil
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, tok string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		// Behind an HTTPS reverse proxy the request itself is plain HTTP.
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteStrictMode,
	})
}

// authenticated checks the session cookie and slides its expiry forward.
func (s *Server) authenticated(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	h := hashToken(c.Value)
	exp, err := s.store.SessionExpires(r.Context(), h)
	now := s.now()
	if err != nil || !exp.After(now) {
		return false
	}
	if exp.Sub(now) < sessionLifetime-sessionRenew {
		next := now.Add(sessionLifetime)
		if err := s.store.ExtendSession(r.Context(), h, next); err == nil {
			setSessionCookie(w, r, c.Value, next)
		}
	}
	return true
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// limiter blocks an IP after too many failed logins.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
	now    func() time.Time
}

func newLimiter(max int, window time.Duration, now func() time.Time) *limiter {
	return &limiter{max: max, window: window, fails: map[string][]time.Time{}, now: now}
}

func (l *limiter) recent(ip string) []time.Time {
	cut := l.now().Add(-l.window)
	kept := l.fails[ip][:0]
	for _, t := range l.fails[ip] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, ip)
		return nil
	}
	l.fails[ip] = kept
	return kept
}

func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip)) >= l.max
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip] = append(l.recent(ip), l.now())
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- handlers ----

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	h, err := s.passwordHash(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"setup_required": h == "", "authenticated": h != "" && s.authenticated(w, r), "version": s.ctrl.Version()})
}

type passwordReq struct {
	Password string `json:"password"`
	Current  string `json:"current"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req passwordReq
	if !readJSON(w, r, &req) {
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if h, err := s.passwordHash(r.Context()); err != nil || h != "" {
		s.fail(w, r, errors.New("the password is already set"), http.StatusConflict)
		return
	}
	if err := s.SetPassword(r.Context(), req.Password); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	s.store.Audit(r.Context(), "auth", "admin password set from "+clientIP(r))
	if err := s.newSession(w, r); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.limit.blocked(ip) {
		s.fail(w, r, errors.New("too many failed logins, try again in a few minutes"), http.StatusTooManyRequests)
		return
	}
	var req passwordReq
	if !readJSON(w, r, &req) {
		return
	}
	ok, err := s.checkPassword(r.Context(), req.Password)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	if !ok {
		s.limit.fail(ip)
		s.log.Warn("failed login", "ip", ip)
		s.fail(w, r, errors.New("wrong password"), http.StatusUnauthorized)
		return
	}
	s.limit.reset(ip)
	if err := s.newSession(w, r); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.DeleteSession(r.Context(), hashToken(c.Value))
	}
	clearCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordReq
	if !readJSON(w, r, &req) {
		return
	}
	ok, err := s.checkPassword(r.Context(), req.Current)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	if !ok {
		s.fail(w, r, errors.New("the current password is wrong"), http.StatusBadRequest)
		return
	}
	if err := s.SetPassword(r.Context(), req.Password); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	s.store.DeleteAllSessions(r.Context())
	s.store.Audit(r.Context(), "auth", "admin password changed from "+clientIP(r))
	if err := s.newSession(w, r); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
