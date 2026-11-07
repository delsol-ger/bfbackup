package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"bfbackup/internal/model"
)

// An endpoint taken before a drop keeps working once the peer is back,
// and waiting for a peer that stays away gives up.
func TestEndpointFollowsReconnect(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	ep, err := w.c.Endpoint(w.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ep.Info(w.ctx); err != nil {
		t.Fatal(err)
	}

	w.goOffline()
	if _, err := ep.Info(w.ctx); !errors.Is(err, model.ErrOffline) {
		t.Fatalf("info while offline: %v", err)
	}
	waiter := ep.(*liveEndpoint)
	start := time.Now()
	if err := waiter.WaitOnline(w.ctx, 100*time.Millisecond); !errors.Is(err, model.ErrOffline) {
		t.Fatalf("wait err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("waited too long")
	}
	// Gone longer than the limit already: no second wait.
	start = time.Now()
	waiter.WaitOnline(w.ctx, 100*time.Millisecond)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("waited again for a peer that was given up")
	}

	w.startPeer(w.token)
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	if err := waiter.WaitOnline(ctx, time.Hour); err != nil {
		t.Fatalf("wait for reconnect: %v", err)
	}
	if _, err := ep.Info(w.ctx); err != nil {
		t.Fatalf("old endpoint after reconnect: %v", err)
	}
}
