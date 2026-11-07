package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"bfbackup/internal/logbook"
)

// defaultStallMinutes is used when the stall_minutes setting is missing.
const defaultStallMinutes = 30

// Tests shorten these.
var (
	stallCheck = time.Minute // how often the watchdog looks at a run
	stallUnit  = time.Minute // unit of the stall_minutes setting
)

// stallLimit is how long a run may go without progress; 0 means no limit.
// It reads the raw setting, so it works before the settings page knows it.
func (c *Controller) stallLimit(ctx context.Context) time.Duration {
	n := defaultStallMinutes
	if all, err := c.store.Settings(ctx); err == nil {
		if v, ok := all["stall_minutes"]; ok {
			if m, err := strconv.Atoi(v); err == nil && m >= 0 {
				n = m
			}
		}
	}
	return time.Duration(n) * stallUnit
}

// progressMark is what has to change for a run to count as moving.
type progressMark struct {
	phase                                        string
	scanned, files, bytesDn, hashed, dirs, steps int64
	waiting                                      bool // waits for a peer, which has its own limit
}

func markOf(ar *activeRun) progressMark {
	s := ar.progress.Snapshot()
	return progressMark{s.Phase, s.Scanned, s.FilesDone, s.BytesDone, s.HashedBytes, s.DirsRead, s.Steps, s.Waiting != ""}
}

// watchStall cancels the run when its progress does not move for the stall
// limit (a hung peer, disk or connection). Hooks are not watched: they may
// legitimately run long without progress. Neither is waiting for a peer to
// reconnect: that gives up on its own. Call the returned function with
// the run's error once the run has returned; it turns the watchdog's
// cancellation into a "stalled" failure.
func (c *Controller) watchStall(ctx context.Context, ar *activeRun) func(error) error {
	limit := c.stallLimit(ctx)
	c.mu.Lock()
	ar.stall = limit
	c.mu.Unlock()
	if limit <= 0 {
		return func(err error) error { return err }
	}
	var stalled atomic.Bool
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(stallCheck)
		defer t.Stop()
		last, since := markOf(ar), time.Now()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			m := markOf(ar)
			if m != last || m.waiting || strings.Contains(m.phase, "hook") {
				last, since = m, time.Now()
				continue
			}
			if time.Since(since) >= limit {
				stalled.Store(true)
				ar.act.logRun(logbook.LevelError, "no progress for %s while %s: stopping the run", limit, m.phase)
				ar.cancel()
				return
			}
		}
	}()
	return func(err error) error {
		close(done)
		if stalled.Load() && errors.Is(err, context.Canceled) {
			return fmt.Errorf("stalled: no progress for %s, the run was stopped. Check the connection to the peers and their disks", limit)
		}
		return err
	}
}
