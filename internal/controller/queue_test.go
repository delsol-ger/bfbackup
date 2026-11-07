package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

func TestQueueOrder(t *testing.T) {
	c := &Controller{active: map[int64]*activeRun{}, heavyCap: 2}
	add := func(id int64, heavy bool, targets []int64, nodes ...string) *activeRun {
		ar := &activeRun{run: store.Run{ID: id}, targets: targets, nodes: nodes, heavy: heavy,
			names: map[string]string{"p1": "one", "p2": "two", "p3": "three"}, start: make(chan struct{})}
		c.active[id] = ar
		c.queue = append(c.queue, ar)
		c.pumpLocked()
		return ar
	}
	r1 := add(1, true, []int64{10}, "p1")
	r2 := add(2, true, []int64{20}, "p1", "p2")
	r3 := add(3, true, []int64{30}, "p2") // p2 is free, but r2 came first
	r4 := add(4, true, []int64{40}, "p3") // free peer: second slot
	r5 := add(5, true, []int64{50})       // controller only, no slot left
	r6 := add(6, false, []int64{60})      // light run: no slot needed
	r7 := add(7, false, []int64{10})      // same target as r1
	for _, x := range []struct {
		ar      *activeRun
		granted bool
		wait    string
	}{{r1, true, ""}, {r2, false, "#1 on one"}, {r3, false, "#2 on two"}, {r4, true, ""},
		{r5, false, "free slot"}, {r6, true, ""}, {r7, false, "#1 on the same target"}} {
		if x.ar.granted != x.granted || !strings.Contains(x.ar.waiting, x.wait) {
			t.Fatalf("run %d: granted=%v waiting=%q", x.ar.run.ID, x.ar.granted, x.ar.waiting)
		}
	}
	if got := c.ActiveRuns(); len(got) != 7 || got[0].ID != 1 || got[1].ID != 4 || got[2].ID != 6 || got[3].ID != 2 || got[3].Waiting == "" {
		t.Fatalf("active order %+v", got)
	}

	// r1 ends: r2 gets both peers; r3 still waits for p2, r5 for a slot, r7 starts.
	c.releaseLocked(r1)
	if !r2.granted || r3.granted || r5.granted || !r7.granted {
		t.Fatalf("after r1: r2=%v r3=%v r5=%v r7=%v", r2.granted, r3.granted, r5.granted, r7.granted)
	}
	c.releaseLocked(r2)
	if !r3.granted || r5.granted {
		t.Fatalf("after r2: r3=%v r5=%v", r3.granted, r5.granted)
	}
	// A cancelled run leaves the queue; more slots start the rest.
	c.dequeueLocked(r5)
	delete(c.active, 5)
	r8 := add(8, true, []int64{80})
	c.heavyCap = 3
	c.pumpLocked()
	if !r8.granted || len(c.queue) != 0 {
		t.Fatalf("r8=%v queue=%d", r8.granted, len(c.queue))
	}

	if n := runNodes(store.Job{SourceNode: "p1", Targets: []store.Target{{ID: 1, NodeID: "p2"}, {ID: 2, NodeID: model.LocalNodeID}}},
		[]int64{1, 2}, RunRequest{Kind: store.KindBackup}); strings.Join(n, ",") != "p1,p2" {
		t.Fatalf("nodes %v", n)
	}
	if n := runNodes(store.Job{SourceNode: "p1", Targets: []store.Target{{ID: 1, NodeID: "p2"}}},
		[]int64{1}, RunRequest{Kind: store.KindRestore, Restore: &RestoreRequest{DestNode: "p3"}}); strings.Join(n, ",") != "p3,p2" {
		t.Fatalf("restore nodes %v", n)
	}
}

func TestOneRunPerPeer(t *testing.T) {
	w := newWorld(t)
	w.waitOnline(true)
	src := filepath.Join(w.ctrlRoot, "s")
	write(t, src, "f.txt", "x")
	os.MkdirAll(filepath.Join(w.ctrlRoot, "hooks"), 0o755)
	os.WriteFile(filepath.Join(w.ctrlRoot, "hooks", "slow.sh"), []byte("#!/bin/sh\nsleep 1\n"), 0o755)
	newJob := func(name, node, hook string) int64 {
		j := store.Job{Name: name, SourceNode: model.LocalNodeID, SourcePath: src, Enabled: true, PreHook: hook,
			Targets: []store.Target{{NodeID: node, Path: filepath.Join(w.peerRoot, name), Enabled: true}}}
		if node == model.LocalNodeID {
			j.Targets[0].Path = filepath.Join(w.ctrlRoot, name)
		}
		if err := w.c.CreateJob(w.ctx, &j); err != nil {
			t.Fatal(err)
		}
		return j.Targets[0].ID
	}
	ta, tb, tc := newJob("a", w.nodeID, "slow.sh"), newJob("b", w.nodeID, ""), newJob("c", model.LocalNodeID, "")

	// Scheduled at the same time: a starts, b waits for the peer, c (controller only) runs at once.
	day := w.c.cfg.Now()
	for _, id := range []int64{ta, tb, tc} {
		tg, _ := w.c.store.GetTarget(w.ctx, id)
		tg.Schedule = "* * * * *"
		w.c.UpdateTarget(w.ctx, &tg)
	}
	w.c.scheduleDue(w.ctx, day.Add(-2*time.Minute), day)
	runs, _, _ := w.c.store.ListRuns(w.ctx, store.RunFilter{})
	if len(runs) != 3 {
		t.Fatalf("runs %d", len(runs))
	}
	byTarget := map[int64]store.Run{}
	for _, r := range runs {
		byTarget[r.TargetID] = r
	}
	var waiting string
	for _, ar := range w.c.ActiveRuns() {
		if ar.TargetID == tb {
			waiting = ar.Waiting
		}
	}
	if !strings.Contains(waiting, "colo") {
		t.Fatalf("b waits for %q", waiting)
	}
	// A verify of a busy target queues behind it; the same backup again is refused.
	v, err := w.c.StartRun(w.ctx, RunRequest{Kind: store.KindVerifyQuick, TargetID: ta})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.c.StartRun(w.ctx, RunRequest{Kind: store.KindBackup, TargetID: ta}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second backup: %v", err)
	}

	a, b, c := w.wait(byTarget[ta]), w.wait(byTarget[tb]), w.wait(byTarget[tc])
	v = w.wait(v)
	for _, r := range []store.Run{a, b, c, v} {
		if r.Status != store.StatusSuccess {
			t.Fatalf("run %d %s %s", r.ID, r.Status, r.Error)
		}
	}
	if c.StartedAt > a.FinishedAt {
		t.Fatal("controller-only run waited for the peer")
	}
	// a, b and the verify all use the peer: one after the other.
	if b.StartedAt < a.FinishedAt || v.StartedAt < b.FinishedAt {
		t.Fatalf("overlap: a %d-%d, b %d-%d, verify %d-%d", a.StartedAt, a.FinishedAt, b.StartedAt, b.FinishedAt, v.StartedAt, v.FinishedAt)
	}

	// Stopping a queued run frees its place.
	first, err := w.c.StartRun(w.ctx, RunRequest{Kind: store.KindBackup, TargetID: ta})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := w.c.StartRun(w.ctx, RunRequest{Kind: store.KindBackup, TargetID: tb})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.c.CancelRun(context.Background(), queued.ID); err != nil {
		t.Fatal(err)
	}
	if r := w.wait(queued); r.Status != store.StatusCancelled {
		t.Fatalf("cancelled queued run %s", r.Status)
	}
	if r := w.wait(first); r.Status != store.StatusSuccess {
		t.Fatalf("first %s", r.Status)
	}
}
