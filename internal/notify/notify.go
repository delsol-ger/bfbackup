// Package notify sends notifications through Pushover and email (SMTPS);
// add another channel by implementing Notifier.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Event kinds a user can subscribe to.
const (
	RunFailed        = "run_failed"
	RunWarning       = "run_warning"
	RunSuccess       = "run_success"
	ApprovalRequired = "approval_required"
	VerifyProblem    = "verify_problem"
	NodeOffline      = "node_offline"
	LowSpace         = "low_space"
	DriveDone        = "drive_done"
	DriveOverdue     = "drive_overdue"
	Test             = "test"
)

// AllKinds lists the subscribable kinds with a description for the UI.
var AllKinds = []struct {
	Kind, Label string
	Default     bool
}{
	{RunFailed, "A run failed", true},
	{RunWarning, "A run finished with file errors", true},
	{RunSuccess, "A backup finished successfully", false},
	{ApprovalRequired, "A backup waits for approval (mass change detected)", true},
	{VerifyProblem, "A verify found missing or damaged files", true},
	{NodeOffline, "A peer is offline", true},
	{LowSpace, "A target is running out of space", true},
	{DriveDone, "A USB drive finished its backups (safe to unmount)", true},
	{DriveOverdue, "A USB drive was not connected for a long time", true},
}

// Event is one notification.
type Event struct {
	Kind     string
	Title    string
	Message  string
	Priority int // -2..2 (Pushover semantics); 1 = high
}

// Notifier delivers events.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

// PushoverURL is the API endpoint.
const PushoverURL = "https://api.pushover.net/1/messages.json"

// Pushover sends events through the Pushover API.
type Pushover struct {
	Token    string // application token
	User     string // user or group key
	Endpoint string // defaults to PushoverURL (tests override it)
	Client   *http.Client
}

// Notify posts one message.
func (p Pushover) Notify(ctx context.Context, ev Event) error {
	if p.Token == "" || p.User == "" {
		return errors.New("pushover: application token and user key are required")
	}
	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = PushoverURL
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	form := url.Values{
		"token":    {p.Token},
		"user":     {p.User},
		"title":    {truncate(ev.Title, 250)},
		"message":  {truncate(ev.Message, 1024)},
		"priority": {fmt.Sprint(ev.Priority)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		// Never include the URL-encoded form (it holds the token) in errors.
		return errors.New("pushover: request failed: " + scrub(err.Error(), p.Token, p.User))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var r struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	json.Unmarshal(body, &r)
	if resp.StatusCode != http.StatusOK || r.Status != 1 {
		return fmt.Errorf("pushover: HTTP %d: %s", resp.StatusCode, strings.Join(r.Errors, "; "))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func scrub(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}
