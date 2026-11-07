package controller

import (
	"context"
	"fmt"
	"sort"

	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

// The run queue. Every run waits here until it may start:
//
//   - a peer takes part in one run at a time (as source, target or restore
//     destination); the controller's own node is not limited this way,
//     only by the number of parallel runs;
//   - a target has one run at a time, whatever node it is on;
//   - heavy runs (backup, verify, rebuild) need one of the parallel slots.
//
// The queue is first in, first out. A waiting run reserves its peers and
// targets, so later runs never overtake it there, and it takes everything it
// needs at once, so two runs never wait for each other. Runs that become due
// at the same moment (scheduler, peers coming online) queue in the order
// they are started: jobs in list order, a target's backup before its verify.

// runNodes returns the peers a run uses, without the controller.
func runNodes(job store.Job, targets []int64, req RunRequest) []string {
	byID := map[int64]string{}
	for _, t := range job.Targets {
		byID[t.ID] = t.NodeID
	}
	var nodes []string
	add := func(n string) {
		if n == "" || n == model.LocalNodeID {
			return
		}
		for _, have := range nodes {
			if have == n {
				return
			}
		}
		nodes = append(nodes, n)
	}
	switch req.Kind {
	case store.KindBackup, store.KindDryRun:
		add(job.SourceNode)
	case store.KindRestore:
		dest := job.SourceNode
		if req.Restore != nil && req.Restore.DestNode != "" {
			dest = req.Restore.DestNode
		}
		add(dest)
	case store.KindDelete:
		if req.Delete != nil && req.Delete.FromSource {
			add(job.SourceNode)
		}
	}
	for _, t := range targets {
		add(byID[t])
	}
	return nodes
}

// sameKindLocked returns an active or queued run of the same kind on one of
// the targets, or 0. Restores and deletes always queue: each one is for
// other files.
func (c *Controller) sameKindLocked(kind string, targets []int64) int64 {
	if kind == store.KindRestore || kind == store.KindDelete {
		return 0
	}
	for _, ar := range c.active {
		if ar.run.Kind != kind {
			continue
		}
		for _, t := range ar.targets {
			for _, want := range targets {
				if t == want {
					return ar.run.ID
				}
			}
		}
	}
	return 0
}

// pumpLocked starts every queued run that may start now, in queue order, and
// tells the others what they wait for.
func (c *Controller) pumpLocked() {
	usedT := map[int64]int64{}  // target -> run that has or reserved it
	usedN := map[string]int64{} // peer -> run that has or reserved it
	heavy := 0
	for _, ar := range c.active {
		if !ar.granted {
			continue
		}
		for _, t := range ar.targets {
			usedT[t] = ar.run.ID
		}
		for _, n := range ar.nodes {
			usedN[n] = ar.run.ID
		}
		if ar.heavy {
			heavy++
		}
	}
	rest := c.queue[:0]
	for _, ar := range c.queue {
		wait := ""
		for _, t := range ar.targets {
			if id, ok := usedT[t]; ok && wait == "" {
				wait = fmt.Sprintf("waiting for run #%d on the same target", id)
			}
		}
		for _, n := range ar.nodes {
			if id, ok := usedN[n]; ok && wait == "" {
				wait = fmt.Sprintf("waiting for run #%d on %s", id, ar.names[n])
			}
		}
		if wait == "" && ar.heavy && heavy >= c.heavyCap {
			wait = fmt.Sprintf("waiting for a free slot (%d parallel run(s))", c.heavyCap)
		}
		// Taken or reserved either way: later runs must not overtake this one.
		for _, t := range ar.targets {
			if _, ok := usedT[t]; !ok {
				usedT[t] = ar.run.ID
			}
		}
		for _, n := range ar.nodes {
			if _, ok := usedN[n]; !ok {
				usedN[n] = ar.run.ID
			}
		}
		if wait != "" {
			ar.waiting = wait
			rest = append(rest, ar)
			continue
		}
		ar.granted, ar.waiting = true, ""
		if ar.heavy {
			heavy++
		}
		close(ar.start)
	}
	for i := len(rest); i < len(c.queue); i++ {
		c.queue[i] = nil
	}
	c.queue = rest
}

// dequeueLocked removes a run that gives up waiting.
func (c *Controller) dequeueLocked(ar *activeRun) {
	for i, q := range c.queue {
		if q == ar {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			return
		}
	}
}

// waitTurn blocks until the run may start. It returns false when the run was
// cancelled while it waited.
func (c *Controller) waitTurn(ctx context.Context, ar *activeRun) bool {
	select {
	case <-ar.start:
		return true
	case <-ctx.Done():
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ar.granted {
		return true // started at the same moment; the run sees the cancel itself
	}
	c.dequeueLocked(ar)
	return false
}

// releaseLocked drops a finished run and lets the queue move on.
func (c *Controller) releaseLocked(ar *activeRun) {
	delete(c.active, ar.run.ID)
	c.dequeueLocked(ar)
	c.pumpLocked()
}

// sortedActiveLocked lists running runs first (and runs that gave up waiting
// and are finishing), then the queue in order.
func (c *Controller) sortedActiveLocked() []*activeRun {
	queued := map[*activeRun]bool{}
	for _, ar := range c.queue {
		queued[ar] = true
	}
	out := make([]*activeRun, 0, len(c.active))
	for _, ar := range c.active {
		if !queued[ar] {
			out = append(out, ar)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].run.ID < out[j].run.ID })
	return append(out, c.queue...)
}
