package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bfbackup/internal/controller"
	"bfbackup/internal/endpoint"
	"bfbackup/internal/fsx"
	"bfbackup/internal/hooks"
)

type client struct {
	t    *testing.T
	base string
	http *http.Client
	csrf bool
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if c.csrf {
		req.Header.Set(csrfHeader, "1")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, out
}

func setup(t *testing.T) (*client, *Server, string) {
	t.Helper()
	root := t.TempDir()
	f, err := fsx.New([]string{root}, fsx.Options{UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := controller.New(controller.Config{DataDir: filepath.Join(t.TempDir(), "data"), Version: "test",
		Local: &endpoint.Local{FS: f, Hooks: hooks.Runner{}}, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	s := New(c, log)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: srv.URL, http: &http.Client{Jar: jar}, csrf: true}, s, root
}

func TestAuthFlow(t *testing.T) {
	c, s, _ := setup(t)
	code, body := c.do("GET", "/api/auth/state", nil)
	if code != 200 || body["setup_required"] != true {
		t.Fatalf("state %d %v", code, body)
	}
	if code, _ := c.do("GET", "/api/overview", nil); code != 401 {
		t.Fatalf("unauthenticated overview = %d", code)
	}
	if code, _ := c.do("POST", "/api/auth/setup", map[string]string{"password": "short"}); code != 400 {
		t.Fatalf("short password = %d", code)
	}
	if code, _ := c.do("POST", "/api/auth/setup", map[string]string{"password": "correct horse"}); code != 200 {
		t.Fatalf("setup = %d", code)
	}
	if code, _ := c.do("GET", "/api/overview", nil); code != 200 {
		t.Fatalf("overview after setup = %d", code)
	}
	// Setup only works once, even for an attacker on the LAN.
	otherJar, _ := cookiejar.New(nil)
	other := &client{t: t, base: c.base, http: &http.Client{Jar: otherJar}, csrf: true}
	if code, _ := other.do("POST", "/api/auth/setup", map[string]string{"password": "attacker pw"}); code != 409 {
		t.Fatalf("second setup = %d", code)
	}

	// CSRF header is required for writes.
	c.csrf = false
	if code, _ := c.do("POST", "/api/auth/logout", nil); code != 403 {
		t.Fatalf("logout without csrf = %d", code)
	}
	c.csrf = true
	c.do("POST", "/api/auth/logout", nil)
	if code, _ := c.do("GET", "/api/overview", nil); code != 401 {
		t.Fatalf("after logout = %d", code)
	}

	// Rate limit.
	for i := 0; i < 5; i++ {
		if code, _ := other.do("POST", "/api/auth/login", map[string]string{"password": "wrong"}); code != 401 {
			t.Fatalf("wrong login = %d", code)
		}
	}
	if code, _ := other.do("POST", "/api/auth/login", map[string]string{"password": "correct horse"}); code != 429 {
		t.Fatalf("rate limit = %d", code)
	}
	s.now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	if code, _ := other.do("POST", "/api/auth/login", map[string]string{"password": "correct horse"}); code != 200 {
		t.Fatalf("login after window = %d", code)
	}
	s.now = time.Now

	// Password change logs out other sessions.
	if code, _ := other.do("POST", "/api/auth/password", map[string]string{"current": "correct horse", "password": "new password!"}); code != 200 {
		t.Fatalf("change = %d", code)
	}
	if code, _ := other.do("GET", "/api/overview", nil); code != 200 {
		t.Fatal("session after own password change must stay valid")
	}
}

func TestSlidingSession(t *testing.T) {
	c, s, _ := setup(t)
	login(t, c, s)
	base := time.Now()
	at := func(d time.Duration) { s.now = func() time.Time { return base.Add(d) } }
	defer func() { s.now = time.Now }()
	at(20 * 24 * time.Hour)
	if code, _ := c.do("GET", "/api/overview", nil); code != 200 {
		t.Fatalf("day 20 = %d", code)
	}
	// Day 45 is past the first 30 days, but the use on day 20 renewed the session.
	at(45 * 24 * time.Hour)
	if code, _ := c.do("GET", "/api/overview", nil); code != 200 {
		t.Fatalf("day 45 = %d", code)
	}
	at(100 * 24 * time.Hour)
	if code, _ := c.do("GET", "/api/overview", nil); code != 401 {
		t.Fatalf("day 100 after 55 idle days = %d", code)
	}

	// Behind an HTTPS reverse proxy the cookie is Secure.
	req, _ := http.NewRequest("POST", c.base+"/api/auth/login", strings.NewReader(`{"password":"test password"}`))
	req.Header.Set(csrfHeader, "1")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ck := resp.Header.Get("Set-Cookie"); !strings.Contains(ck, "Secure") {
		t.Fatalf("cookie behind proxy: %q", ck)
	}
}

func login(t *testing.T, c *client, s *Server) {
	t.Helper()
	if err := s.InitPassword(t.Context(), "test password"); err != nil {
		t.Fatal(err)
	}
	if code, _ := c.do("POST", "/api/auth/login", map[string]string{"password": "test password"}); code != 200 {
		t.Fatalf("login = %d", code)
	}
}

func waitRun(t *testing.T, c *client, id float64) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, body := c.do("GET", fmt.Sprintf("/api/runs/%d", int64(id)), nil)
		run := body["run"].(map[string]any)
		if st := run["status"]; st != "queued" && st != "running" && body["progress"] == nil {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return nil
}

func TestJobLifecycleOverAPI(t *testing.T) {
	c, s, root := setup(t)
	login(t, c, s)
	src := filepath.Join(root, "src")
	os.MkdirAll(filepath.Join(src, "docs"), 0o755)
	os.WriteFile(filepath.Join(src, "docs/a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(src, "b.txt"), []byte("world"), 0o644)

	code, job := c.do("POST", "/api/jobs", map[string]any{
		"name": "docs", "source_node": "local", "source_path": src, "enabled": true,
		"targets": []map[string]any{{"node_id": "local", "path": filepath.Join(root, "dst"), "enabled": true, "schedule": "@daily"}},
	})
	if code != 201 {
		t.Fatalf("create job %d %v", code, job)
	}
	jobID := job["id"].(float64)
	tid := job["targets"].([]any)[0].(map[string]any)["id"].(float64)

	if code, body := c.do("POST", "/api/jobs", map[string]any{"name": "", "source_node": "local", "source_path": src}); code != 400 {
		t.Fatalf("invalid job %d %v", code, body)
	}
	code, run := c.do("POST", fmt.Sprintf("/api/targets/%d/run", int64(tid)), map[string]any{"kind": "backup"})
	if code != 202 {
		t.Fatalf("run %d %v", code, run)
	}
	r := waitRun(t, c, run["id"].(float64))
	if r["status"] != "success" {
		t.Fatalf("run %v", r)
	}
	if code, _ := c.do("POST", fmt.Sprintf("/api/targets/%d/run", int64(tid)), map[string]any{"kind": "rm -rf"}); code != 400 {
		t.Fatal("bad kind accepted")
	}

	_, br := c.do("GET", fmt.Sprintf("/api/targets/%d/browse?dir=", int64(tid)), nil)
	if len(br["dirs"].([]any)) != 1 || len(br["files"].([]any)) != 1 {
		t.Fatalf("browse %v", br)
	}
	if code, _ := c.do("GET", fmt.Sprintf("/api/targets/%d/browse?dir=../x", int64(tid)), nil); code != 400 {
		t.Fatalf("traversal browse = %d", code)
	}

	// Delete needs the confirmation word.
	path := fmt.Sprintf("/api/jobs/%d/delete", int64(jobID))
	if code, _ := c.do("POST", path, map[string]any{"paths": []string{"docs"}, "from_source": true}); code != 400 {
		t.Fatalf("unconfirmed delete = %d", code)
	}
	code, run = c.do("POST", path, map[string]any{"paths": []string{"docs"}, "from_source": true, "confirm": "delete"})
	if code != 202 {
		t.Fatalf("delete %d %v", code, run)
	}
	waitRun(t, c, run["id"].(float64))
	if _, err := os.Stat(filepath.Join(src, "docs/a.txt")); !os.IsNotExist(err) {
		t.Fatal("source not deleted")
	}

	// Restore it back from the trash? No: from the backup it is gone too, so
	// restore b.txt to a new place instead.
	alt := filepath.Join(root, "alt")
	code, run = c.do("POST", fmt.Sprintf("/api/targets/%d/restore", int64(tid)), map[string]any{"paths": []string{"b.txt"}, "dest_node": "local", "dest_path": alt})
	if code != 202 {
		t.Fatalf("restore %d %v", code, run)
	}
	waitRun(t, c, run["id"].(float64))
	if b, _ := os.ReadFile(filepath.Join(alt, "b.txt")); string(b) != "world" {
		t.Fatal("restore content")
	}

	_, runs := c.do("GET", "/api/runs?limit=10", nil)
	if runs["total"].(float64) != 3 {
		t.Fatalf("runs total %v", runs["total"])
	}
	_, ls := c.do("GET", "/api/nodes/local/ls?path="+root, nil)
	if len(ls["entries"].([]any)) < 2 {
		t.Fatalf("ls %v", ls)
	}
	if code, _ := c.do("GET", "/api/nodes/local/ls?path=/etc", nil); code != 400 {
		t.Fatalf("ls outside root = %d", code)
	}
}

func TestSettingsExportImport(t *testing.T) {
	c, s, _ := setup(t)
	login(t, c, s)
	_, st := c.do("GET", "/api/settings", nil)
	st["pushover_token"] = "secret-app-token"
	st["pushover_user"] = "user-key"
	st["email_host"], st["email_user"], st["email_password"] = "smtp.example.com", "bfb@example.com", "secret-mail-pw"
	st["email_to"] = []string{"me@example.com"}
	if code, body := c.do("PUT", "/api/settings", st); code != 200 {
		t.Fatalf("put settings %d %v", code, body)
	}
	_, st = c.do("GET", "/api/settings", nil)
	if _, has := st["pushover_token"]; has || st["pushover_token_set"] != true {
		t.Fatalf("token leaked or not set: %v", st)
	}
	if _, has := st["email_password"]; has || st["email_password_set"] != true {
		t.Fatalf("email password leaked or not set: %v", st)
	}

	code, n := c.do("POST", "/api/nodes", map[string]string{"name": "colo"})
	if code != 201 || !strings.HasPrefix(n["token"].(string), "bfb1.") {
		t.Fatalf("add node %d %v", code, n)
	}

	req, _ := http.NewRequest("GET", c.base+"/api/system/export", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	export, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") || !bytes.Contains(export, []byte("colo")) {
		t.Fatal("export")
	}
	var ex map[string]any
	json.Unmarshal(export, &ex)
	if code, body := c.do("POST", "/api/system/import", ex); code != 200 {
		t.Fatalf("import %d %v", code, body)
	}

	req, _ = http.NewRequest("GET", c.base+"/api/system/db", nil)
	resp, _ = c.http.Do(req)
	db, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.HasPrefix(db, []byte("SQLite format 3")) {
		t.Fatal("db snapshot is not a SQLite file")
	}
	_, audit := c.do("GET", "/api/audit", nil)
	if audit["_raw"] == nil {
		t.Fatal("audit should be a list")
	}
}

func TestStaticAndHeaders(t *testing.T) {
	c, _, _ := setup(t)
	resp, err := http.Get(c.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") ||
		resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("static %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = http.Get(c.base + "/app.js")
	resp.Body.Close()
	tag := resp.Header.Get("ETag")
	if tag == "" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("app.js caching headers %v", resp.Header)
	}
	req, _ := http.NewRequest("GET", c.base+"/app.js", nil)
	req.Header.Set("If-None-Match", tag)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation status %d", resp.StatusCode)
	}
	if code, _ := c.do("GET", "/api/nope", nil); code != 404 {
		t.Fatalf("unknown api = %d", code)
	}
}
