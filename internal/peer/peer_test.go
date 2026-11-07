package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"bfbackup/internal/proto"
)

func TestConnLogRotates(t *testing.T) {
	p := filepath.Join(t.TempDir(), ConnLogName)
	l, err := OpenConnLog(p, 300)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		l.Printf("line %02d %s", i, strings.Repeat("x", 20))
	}
	l.Close()
	cur, _ := os.ReadFile(p)
	old, err := os.ReadFile(p + ".1")
	if err != nil || len(cur) > 300 || len(old) > 300 || !strings.Contains(string(cur), "line 19") {
		t.Fatalf("rotation: current %d bytes, old %d bytes (%v)", len(cur), len(old), err)
	}
	// Reopening appends instead of truncating (no cap here: the file is
	// already close to 300 bytes and would rightly rotate).
	l, _ = OpenConnLog(p, 0)
	l.Printf("again")
	l.Close()
	if cur2, _ := os.ReadFile(p); !strings.Contains(string(cur2), "line 19") || !strings.Contains(string(cur2), "again") {
		t.Fatalf("reopen lost lines: %q", cur2)
	}
	var none *ConnLog
	none.Printf("no file: must not panic")
	none.Close()
}

func TestExplain(t *testing.T) {
	for want, err := range map[string]error{
		"does not resolve":                &net.DNSError{Err: "no such host", Name: "ctrl"},
		"nothing listens on that port":    &stageErr{"connecting", fmt.Errorf("dial: %w", syscall.ECONNREFUSED)},
		"belongs to another controller":   proto.ErrControllerAuth,
		"does not know this peer":         fmt.Errorf("%w: controller says: unknown node; add the peer", proto.ErrAuth),
		"incompatible versions":           fmt.Errorf("%w: controller says: protocol version 9 not supported", proto.ErrAuth),
		"the token is outdated":           fmt.Errorf("%w: invalid token", proto.ErrAuth),
		"something other than the BFBack": &stageErr{"authenticating", io.ErrUnexpectedEOF},
		"no answer in time":               &stageErr{"connecting", os.ErrDeadlineExceeded},
		"older BFBackup without encrypt":  &stageErr{"setting up tls transport", io.EOF},
		"remove BFB_TRANSPORT=plain":      fmt.Errorf("%w: controller says: this controller accepts only encrypted connections", proto.ErrAuth),
	} {
		if got := Explain(err); !strings.Contains(got, want) {
			t.Errorf("Explain(%v) = %q, want it to contain %q", err, got, want)
		}
	}
	if Explain(errors.New("something else")) != "" || Explain(nil) != "" {
		t.Error("unknown errors need no hint")
	}
}

func TestRunLogsFailuresAndCondensesRepeats(t *testing.T) {
	// A port nobody listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	p := filepath.Join(t.TempDir(), ConnLogName)
	cl, err := OpenConnLog(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Addr: addr, Token: proto.NewToken(), Info: proto.PeerInfo{Hostname: "h", Version: "test"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ConnLog: cl, MinBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "times in a row") || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	cl.Close()

	b, _ := os.ReadFile(p)
	log := string(b)
	for _, want := range []string{
		"peer starting: version test, host h, node " + a.Token.NodeID + ", controller " + addr,
		"attempt 1 failed while connecting to " + addr,
		"hint: the controller host answered, but nothing listens on that port",
		"the same error is now logged only every 10 attempts",
		"failed again (11 times in a row)",
		"peer stopping",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("connection log lacks %q:\n%s", want, log)
		}
	}
	if n := strings.Count(log, "hint:"); n != repeatFull {
		t.Errorf("%d hints, want %d (repeats must be condensed)", n, repeatFull)
	}
	if strings.Contains(log, a.Token.String()) {
		t.Error("the token secret must never be logged")
	}
}
