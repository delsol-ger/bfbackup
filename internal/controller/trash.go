package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

// trashTimeout bounds counting one root's trash; a huge trash is walked
// file by file on its node.
var trashTimeout = 2 * time.Minute

// TrashUse is a job that uses a root, as its source or as a target.
type TrashUse struct {
	JobID   int64  `json:"job_id"`
	JobName string `json:"job_name"`
	Role    string `json:"role"` // "source" or "target"
}

// TrashRoot is the trash below one job root on one node.
type TrashRoot struct {
	Node string     `json:"node"`
	Root string     `json:"root"`
	Uses []TrashUse `json:"uses"`
	// Status: "ok", "offline", "old_peer" (the peer cannot count its
	// trash yet) or "error" (see Error).
	Status  string             `json:"status"`
	Error   string             `json:"error,omitempty"`
	Batches []model.TrashBatch `json:"batches"`
	Files   int64              `json:"files"`
	Bytes   int64              `json:"bytes"`
	Oldest  int64              `json:"oldest,omitempty"`   // unix seconds of the oldest batch
	PurgeAt int64              `json:"purge_at,omitempty"` // when maintenance removes that batch
}

// TrashOverview is the trash of every job root, one row per node and root.
type TrashOverview struct {
	RetentionDays int         `json:"retention_days"`
	CheckedAt     int64       `json:"checked_at"`
	Roots         []TrashRoot `json:"roots"`
}

// trashRoots lists the distinct (node, root) pairs of the jobs, or of one
// job when jobID is not 0.
func (c *Controller) trashRoots(ctx context.Context, jobID int64) ([]TrashRoot, error) {
	jobs, err := c.store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	var out []TrashRoot
	at := map[string]int{}
	add := func(node, root string, u TrashUse) {
		k := node + "\x00" + root
		i, ok := at[k]
		if !ok {
			i = len(out)
			at[k] = i
			out = append(out, TrashRoot{Node: node, Root: root, Batches: []model.TrashBatch{}})
		}
		out[i].Uses = append(out[i].Uses, u)
	}
	for _, j := range jobs {
		if jobID != 0 && j.ID != jobID {
			continue
		}
		add(j.SourceNode, j.SourcePath, TrashUse{JobID: j.ID, JobName: j.Name, Role: "source"})
		for _, t := range j.Targets {
			add(t.NodeID, t.Path, TrashUse{JobID: j.ID, JobName: j.Name, Role: "target"})
		}
	}
	if jobID != 0 && len(out) == 0 {
		return nil, fmt.Errorf("job %d: %w", jobID, store.ErrNotFound)
	}
	return out, nil
}

// Trash counts the trash below every job root (jobID 0) or below the roots
// of one job. Offline nodes and older peers are listed without numbers.
func (c *Controller) Trash(ctx context.Context, jobID int64) (TrashOverview, error) {
	s, err := c.Settings(ctx)
	if err != nil {
		return TrashOverview{}, err
	}
	roots, err := c.trashRoots(ctx, jobID)
	if err != nil {
		return TrashOverview{}, err
	}
	retention := time.Duration(s.TrashRetentionDays) * 24 * time.Hour
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range roots {
		wg.Add(1)
		go func(r *TrashRoot) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c.countTrash(ctx, r, retention)
		}(&roots[i])
	}
	wg.Wait()
	sort.SliceStable(roots, func(i, j int) bool {
		if roots[i].Node != roots[j].Node {
			return roots[i].Node == model.LocalNodeID || roots[j].Node != model.LocalNodeID && roots[i].Node < roots[j].Node
		}
		return roots[i].Root < roots[j].Root
	})
	return TrashOverview{RetentionDays: s.TrashRetentionDays, CheckedAt: c.cfg.Now().Unix(), Roots: roots}, nil
}

func (c *Controller) countTrash(ctx context.Context, r *TrashRoot, retention time.Duration) {
	ep, err := c.Endpoint(r.Node)
	if err != nil {
		r.Status = "offline"
		return
	}
	c.countTrashWith(ctx, r, ep, retention)
}

func (c *Controller) countTrashWith(ctx context.Context, r *TrashRoot, ep endpoint.Endpoint, retention time.Duration) {
	l, ok := ep.(endpoint.TrashLister)
	if !ok {
		r.Status = "old_peer"
		return
	}
	ctx, cancel := context.WithTimeout(ctx, trashTimeout)
	defer cancel()
	batches, err := l.TrashInfo(ctx, r.Root)
	switch {
	case errors.Is(err, model.ErrUnsupported):
		r.Status = "old_peer"
		return
	case errors.Is(err, model.ErrOffline):
		r.Status = "offline"
		return
	case err != nil:
		r.Status, r.Error = "error", err.Error()
		return
	}
	r.Status, r.Batches = "ok", batches
	for _, b := range batches {
		r.Files += b.Files
		r.Bytes += b.Bytes
		if r.Oldest == 0 || b.Time < r.Oldest {
			r.Oldest = b.Time
		}
	}
	if r.Oldest != 0 {
		r.PurgeAt = time.Unix(r.Oldest, 0).Add(retention).Unix()
	}
}

// EmptyTrash removes every trash batch below root on node at once. Only
// job roots are accepted. It returns how many batches were removed.
func (c *Controller) EmptyTrash(ctx context.Context, node, root string) (int, error) {
	roots, err := c.trashRoots(ctx, 0)
	if err != nil {
		return 0, err
	}
	known := false
	for _, r := range roots {
		known = known || r.Node == node && r.Root == root
	}
	if !known {
		return 0, fmt.Errorf("%s is not a job folder on that node: %w", root, model.ErrInvalidPath)
	}
	ep, err := c.Endpoint(node)
	if err != nil {
		return 0, err
	}
	// A cutoff in the future takes every batch, also one a run is filling
	// right now and one stamped by a peer whose clock runs ahead.
	n, err := ep.PurgeTrash(ctx, root, c.cfg.Now().Add(24*time.Hour))
	if n > 0 || err == nil {
		c.audit(ctx, "trash_empty", "emptied the trash of %s on %s: %d batches removed", root, c.nodeName(ctx, node), n)
	}
	if err != nil {
		c.logEvent(logbook.Entry{Level: logbook.LevelError, Kind: logbook.KindSystem, Node: node, Message: fmt.Sprintf("emptying the trash of %s failed: %v", root, err)})
		return n, err
	}
	return n, nil
}
