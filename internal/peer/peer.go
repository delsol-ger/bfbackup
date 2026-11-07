// Package peer is the agent that runs on backup peers. It dials the
// controller, authenticates, and then serves endpoint calls until the
// connection drops, reconnecting with exponential backoff.
package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/proto"
	"bfbackup/internal/rpc"
)

// Agent connects one peer to its controller.
type Agent struct {
	Addr     string // controller host:port
	Token    proto.Token
	EP       endpoint.Endpoint
	Security proto.Security
	Info     proto.PeerInfo
	Log      *slog.Logger
	// ConnLog records every connection attempt (optional).
	ConnLog *ConnLog

	MinBackoff time.Duration // default 1s
	MaxBackoff time.Duration // default 60s

	// OnConnect is called after a successful handshake (tests, metrics).
	OnConnect func()

	resolved string // last logged DNS answer
	steps    []step // what the current attempt did, written with its outcome
}

type step struct {
	at  time.Time
	msg string
}

// note records a step of the current attempt. Steps are written when the
// attempt connects or its failure is logged in full, so condensed repeats
// stay condensed.
func (a *Agent) note(format string, args ...any) {
	if a.ConnLog != nil {
		a.steps = append(a.steps, step{time.Now(), fmt.Sprintf(format, args...)})
	}
}

func (a *Agent) flushSteps() {
	for _, s := range a.steps {
		a.ConnLog.printAt(s.at, s.msg)
	}
	a.steps = a.steps[:0]
}

// stageErr is a failed connection attempt and the step it failed in.
type stageErr struct {
	stage string
	err   error
}

func (e *stageErr) Error() string { return e.stage + ": " + e.err.Error() }
func (e *stageErr) Unwrap() error { return e.err }

// Identical failures in a row are logged in full this many times, then
// every repeatEvery-th time, so a controller that is down for days does not
// flood the connection log.
const (
	repeatFull  = 3
	repeatEvery = 10
)

// Run keeps the agent connected until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	minB, maxB := a.MinBackoff, a.MaxBackoff
	if minB <= 0 {
		minB = time.Second
	}
	if maxB <= 0 {
		maxB = time.Minute
	}
	transport := "plain"
	if a.Security != nil {
		transport = a.Security.Name()
	}
	a.ConnLog.Printf("peer starting: version %s, host %s, node %s, controller %s, transport %s",
		a.Info.Version, a.Info.Hostname, a.Token.NodeID, a.Addr, transport)
	backoff := minB
	lastFail, repeats := "", 0
	for attempt := 1; ; attempt++ {
		start := time.Now()
		connected, err := a.runOnce(ctx, attempt)
		if ctx.Err() != nil {
			if connected {
				a.ConnLog.Printf("disconnected after %s: the peer is stopping", time.Since(start).Round(time.Second))
			}
			a.ConnLog.Printf("peer stopping")
			return ctx.Err()
		}
		if time.Since(start) > time.Minute {
			backoff = minB // the connection was healthy for a while
		}
		auth := errors.Is(err, proto.ErrAuth) || errors.Is(err, proto.ErrControllerAuth)
		switch {
		case auth:
			a.Log.Error("authentication with controller failed; check BFB_TOKEN", "err", err, "retry_in", maxB)
			backoff = maxB
		default:
			a.Log.Warn("disconnected from controller", "err", err, "retry_in", backoff)
		}

		if connected {
			a.ConnLog.Printf("disconnected after %s: %v; reconnecting in %s", time.Since(start).Round(time.Second), err, backoff)
			lastFail, repeats = "", 0
		} else {
			if msg := err.Error(); msg == lastFail {
				repeats++
			} else {
				lastFail, repeats = msg, 0
			}
			switch {
			case repeats < repeatFull:
				a.flushSteps()
				a.ConnLog.Printf("attempt %d failed while %v; next attempt in %s", attempt, err, backoff)
				if hint := Explain(err); hint != "" {
					a.ConnLog.Printf("    hint: %s", hint)
				}
				if repeats == repeatFull-1 {
					a.ConnLog.Printf("    the same error is now logged only every %d attempts", repeatEvery)
				}
			case repeats%repeatEvery == 0:
				a.ConnLog.Printf("attempt %d failed again (%d times in a row): %v", attempt, repeats+1, err)
			}
			a.steps = a.steps[:0]
		}

		select {
		case <-ctx.Done():
			a.ConnLog.Printf("peer stopping")
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxB {
			backoff = maxB
		}
	}
}

// runOnce makes one connection and serves it until it ends. connected tells
// whether the handshake succeeded before the error.
func (a *Agent) runOnce(ctx context.Context, attempt int) (connected bool, err error) {
	if err := a.resolve(ctx); err != nil {
		return false, err
	}
	t0 := time.Now()
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", a.Addr)
	if err != nil {
		return false, &stageErr{"connecting to " + a.Addr, err}
	}
	a.note("attempt %d: TCP connection to %s open (local %s, %s)", attempt, raw.RemoteAddr(), raw.LocalAddr(), time.Since(t0).Round(time.Millisecond))
	sec := a.Security
	if sec == nil {
		sec = proto.Plain{}
	}
	raw.SetDeadline(time.Now().Add(15 * time.Second))
	conn, err := sec.Client(raw)
	if err != nil {
		raw.Close()
		return false, &stageErr{"setting up " + sec.Name() + " transport", err}
	}
	if sec.Name() != "plain" {
		a.note("attempt %d: %s transport set up", attempt, sec.Name())
	}
	if err := proto.ClientHandshake(conn, a.Token, a.Info); err != nil {
		conn.Close()
		return false, &stageErr{"authenticating with the controller", err}
	}
	conn.SetDeadline(time.Time{})

	sess, err := yamux.Client(conn, rpc.SessionConfig(nil))
	if err != nil {
		conn.Close()
		return false, &stageErr{"starting the session", err}
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()

	a.Log.Info("connected to controller", "addr", a.Addr, "node", a.Token.NodeID, "transport", sec.Name())
	a.flushSteps()
	a.ConnLog.Printf("attempt %d: authenticated as node %s, connected to the controller (transport %s)", attempt, a.Token.NodeID, sec.Name())
	if a.OnConnect != nil {
		a.OnConnect()
	}
	srv := &rpc.Server{EP: a.EP, Log: a.Log}
	err = srv.Serve(ctx, sess)
	if err != nil && !errors.Is(err, yamux.ErrSessionShutdown) {
		return true, fmt.Errorf("session: %w", err)
	}
	return true, errors.New("the connection was closed (controller restarted, or the network dropped it)")
}

// resolve looks up the controller's name and logs the answer when it
// changes, so a wrong or changed DNS entry shows up in the connection log.
func (a *Agent) resolve(ctx context.Context) error {
	host, _, err := net.SplitHostPort(a.Addr)
	if err != nil {
		return &stageErr{"reading the controller address " + a.Addr, err}
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return &stageErr{"looking up " + host, err}
	}
	if got := strings.Join(addrs, ", "); got != a.resolved {
		a.ConnLog.Printf("%s resolves to %s", host, got) // only on change, so always written
		a.resolved = got
	}
	return nil
}
