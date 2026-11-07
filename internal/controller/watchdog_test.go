package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"bfbackup/internal/engine"
)

func TestWatchStall(t *testing.T) {
	oldCheck, oldUnit := stallCheck, stallUnit
	stallCheck, stallUnit = 5*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { stallCheck, stallUnit = oldCheck, oldUnit })
	w := newWorld(t)

	newRun := func(phase string) (*activeRun, context.Context) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		ar := &activeRun{cancel: cancel, progress: engine.NewProgress(), act: runActivity{book: w.c.book}}
		ar.progress.SetPhase(phase)
		return ar, ctx
	}

	// No progress: the run is stopped and reported as stalled.
	ar, ctx := newRun("transferring")
	finish := w.c.watchStall(ctx, ar)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stalled run was not stopped")
	}
	if err := finish(ctx.Err()); err == nil || !strings.Contains(err.Error(), "stalled") || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a stalled failure", err)
	}

	// Moving progress and long hooks keep the run alive.
	for _, phase := range []string{"transferring", "pre-hook"} {
		ar, ctx := newRun(phase)
		finish := w.c.watchStall(ctx, ar)
		end := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(end) {
			if phase == "transferring" {
				ar.progress.BytesDone.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if ctx.Err() != nil {
			t.Fatalf("%s: run was stopped although it was fine", phase)
		}
		if err := finish(nil); err != nil {
			t.Fatalf("%s: err = %v", phase, err)
		}
	}

	// 0 switches the watchdog off; a user cancel stays a cancel.
	w.c.store.SetSetting(w.ctx, "stall_minutes", "0")
	ar, ctx = newRun("transferring")
	finish = w.c.watchStall(ctx, ar)
	time.Sleep(200 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("watchdog ran although switched off")
	}
	ar.cancel()
	if err := finish(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestReconnectPeer(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	if err := w.c.ReconnectPeer(w.ctx, w.nodeID); err != nil {
		t.Fatal(err)
	}
	// The peer dials in again by itself.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := w.c.Endpoint(w.nodeID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("peer did not reconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := w.c.ReconnectPeer(w.ctx, "000000000000"); err == nil {
		t.Fatal("reconnecting an unknown peer worked")
	}
}
