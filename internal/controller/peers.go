package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/notify"
	"bfbackup/internal/proto"
	"bfbackup/internal/rpc"
	"bfbackup/internal/store"
)

type peerConn struct {
	sess        *yamux.Session
	remote      *rpc.Remote
	addr        string
	connectedAt time.Time
	info        proto.PeerInfo
	transport   string
}

// ServePeers accepts peer connections until ctx ends.
func (c *Controller) ServePeers(ctx context.Context, ln net.Listener) {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			c.log.Error("peer listener stopped", "err", err)
			return
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.handlePeer(ctx, conn)
		}()
	}
}

func (c *Controller) handlePeer(ctx context.Context, raw net.Conn) {
	addr := raw.RemoteAddr().String()
	if tc, ok := raw.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}
	raw.SetDeadline(time.Now().Add(15 * time.Second))
	conn, err := c.cfg.Security.Server(raw)
	if err != nil {
		c.log.Warn("peer transport setup failed", "addr", addr, "err", err)
		raw.Close()
		return
	}
	transport := proto.Transport(conn)
	if transport != "tls" && c.cfg.RequireTLS {
		nodeID := proto.RefuseHandshake(conn, "this controller accepts only encrypted connections: update the peer, and remove BFB_TRANSPORT=plain from it")
		c.log.Warn("unencrypted peer refused", "addr", addr, "node", nodeID)
		c.logEvent(logbook.Entry{Level: logbook.LevelError, Kind: logbook.KindPeer, Node: nodeID,
			Message: fmt.Sprintf("connection from %s refused: unencrypted, and the controller requires TLS (BFB_TRANSPORT=tls)", addr)})
		conn.Close()
		return
	}
	nodeID, info, err := proto.ServerHandshake(conn, func(id string) ([]byte, error) {
		return c.store.NodeSecret(ctx, id)
	})
	if err != nil {
		c.log.Warn("peer authentication failed", "addr", addr, "node", nodeID, "err", err)
		c.logEvent(logbook.Entry{Level: logbook.LevelError, Kind: logbook.KindPeer, Node: nodeID,
			Message: fmt.Sprintf("connection from %s refused: authentication failed: %v", addr, err)})
		time.Sleep(time.Second) // slow down guessing
		conn.Close()
		return
	}
	conn.SetDeadline(time.Time{})
	sess, err := yamux.Server(conn, rpc.SessionConfig(nil))
	if err != nil {
		conn.Close()
		return
	}
	pc := &peerConn{sess: sess, remote: &rpc.Remote{Session: sess}, addr: addr, connectedAt: c.cfg.Now(), info: info, transport: transport}

	c.mu.Lock()
	old := c.peers[nodeID]
	c.peers[nodeID] = pc
	wasNotified := c.offNotify[nodeID]
	delete(c.offSince, nodeID)
	delete(c.offNotify, nodeID)
	c.mu.Unlock()
	if old != nil {
		old.sess.Close()
	}
	// last_seen is kept fresh while a peer is online, so it tells how long
	// the peer was away.
	prev, perr := c.store.GetNode(ctx, nodeID)
	cameBack := perr == nil && (prev.LastSeen == 0 || c.cfg.Now().Sub(time.Unix(prev.LastSeen, 0)) >= reconnectGrace)
	c.store.TouchNode(ctx, nodeID, info.Hostname, info.Version)
	if cameBack {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.runOnConnect(ctx, nodeID)
		}()
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.saveDisks(ctx, nodeID, pc.remote)
	}()
	c.log.Info("peer connected", "node", nodeID, "addr", addr, "hostname", info.Hostname, "version", info.Version, "transport", transport)
	how := "encrypted"
	if transport != "tls" {
		how = "unencrypted"
	}
	c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindPeer, Node: nodeID,
		Message: fmt.Sprintf("connected from %s (host %s, version %s, %s)", addr, info.Hostname, info.Version, how)})
	if info.Version != c.cfg.Version {
		c.log.Warn("peer runs a different version", "node", nodeID, "peer", info.Version, "controller", c.cfg.Version)
		c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindPeer, Node: nodeID,
			Message: fmt.Sprintf("runs version %s, the controller runs %s", info.Version, c.cfg.Version)})
	}
	if wasNotified {
		c.Notify(ctx, notify.Event{Kind: notify.NodeOffline, Title: "Peer back online", Message: c.nodeName(ctx, nodeID) + " is connected again."})
	}

	select {
	case <-sess.CloseChan():
	case <-ctx.Done():
		sess.Close()
	}
	c.mu.Lock()
	if c.peers[nodeID] == pc {
		delete(c.peers, nodeID)
		c.offSince[nodeID] = c.cfg.Now()
	}
	c.mu.Unlock()
	c.store.SeenNode(context.WithoutCancel(ctx), nodeID)
	c.log.Info("peer disconnected", "node", nodeID, "addr", addr)
	c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindPeer, Node: nodeID, Message: "disconnected"})
}

// reconnectGrace: a peer that was away for less than this only had a
// network blip (or the controller restarted); it does not start backups.
const reconnectGrace = 5 * time.Minute

// runOnConnect starts the backups that wait for this peer: enabled targets
// with "run when the peer comes online" whose target or source is the peer.
// The other side must be online too; when it connects later, it starts them.
func (c *Controller) runOnConnect(ctx context.Context, nodeID string) {
	jobs, err := c.store.ListJobs(ctx)
	if err != nil {
		c.log.Error("run on connect: list jobs", "err", err)
		return
	}
	name := c.nodeName(ctx, nodeID)
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		for _, t := range j.Targets {
			if !t.Enabled || !t.RunOnConnect || (t.NodeID != nodeID && j.SourceNode != nodeID) {
				continue
			}
			other := t.NodeID
			if other == nodeID {
				other = j.SourceNode
			}
			if _, err := c.Endpoint(other); err != nil {
				c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindSchedule, JobID: j.ID, TargetID: t.ID,
					Node: t.NodeID, SrcNode: j.SourceNode,
					Message: fmt.Sprintf("%s came online, but %s is offline; the backup starts when both are online", name, c.nodeName(ctx, other))})
				continue
			}
			c.startScheduled(ctx, RunRequest{Kind: store.KindBackup, TargetID: t.ID, Trigger: name + " came online"})
		}
	}
}

// SetNodeOffline sets whether and after how many hours a peer's absence is
// reported.
func (c *Controller) SetNodeOffline(ctx context.Context, id string, notify bool, hours int) error {
	if err := c.store.SetNodeOffline(ctx, id, notify, hours); err != nil {
		return err
	}
	if notify {
		c.audit(ctx, "node_offline_alert", "peer %s: notify when offline for %d hour(s)", id, hours)
	} else {
		c.audit(ctx, "node_offline_alert", "peer %s: no offline notification", id)
	}
	return nil
}

// saveDisks remembers a newly connected peer's disks, so the Peers page can
// show them after it goes offline even if nobody looked while it was online.
func (c *Controller) saveDisks(ctx context.Context, nodeID string, ep endpoint.Endpoint) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if info, err := ep.Info(ctx); err == nil && info.Disks != nil {
		c.store.SetNodeDisks(ctx, nodeID, info.Disks)
	}
}

func (c *Controller) nodeName(ctx context.Context, id string) string {
	if id == model.LocalNodeID {
		return "Controller (local)"
	}
	if uuid, ok := model.DriveUUID(id); ok {
		return c.driveName(ctx, uuid)
	}
	if n, err := c.store.GetNode(ctx, id); err == nil {
		return n.Name
	}
	return id
}

// Endpoint returns the endpoint for a node, or model.ErrOffline.
func (c *Controller) Endpoint(nodeID string) (endpoint.Endpoint, error) {
	if nodeID == model.LocalNodeID {
		return c.cfg.Local, nil
	}
	if uuid, ok := model.DriveUUID(nodeID); ok {
		return c.driveEndpoint(uuid)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.peers[nodeID]
	if !ok || p.sess.IsClosed() {
		return nil, fmt.Errorf("%s: %w", nodeID, model.ErrOffline)
	}
	// Not p.remote: the endpoint follows the peer to its next connection.
	return c.liveEndpoint(nodeID), nil
}

// NodeView is a node with its live connection state.
type NodeView struct {
	store.Node
	Online      bool   `json:"online"`
	Local       bool   `json:"local"`
	Addr        string `json:"addr,omitempty"`
	ConnectedAt int64  `json:"connected_at,omitempty"`
	Transport   string `json:"transport,omitempty"` // "tls" or "plain" while online
}

// Nodes lists the local node and all peers.
func (c *Controller) Nodes(ctx context.Context) ([]NodeView, error) {
	nodes, err := c.store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := []NodeView{{Node: store.Node{ID: model.LocalNodeID, Name: "Controller (local)", Version: c.cfg.Version}, Online: true, Local: true}}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range nodes {
		v := NodeView{Node: n}
		if p, ok := c.peers[n.ID]; ok && !p.sess.IsClosed() {
			v.Online, v.Addr, v.ConnectedAt, v.Transport = true, p.addr, p.connectedAt.Unix(), p.transport
		}
		out = append(out, v)
	}
	return out, nil
}

func (c *Controller) nodeExists(ctx context.Context, id string) error {
	if id == model.LocalNodeID {
		return nil
	}
	if uuid, ok := model.DriveUUID(id); ok {
		if _, err := c.store.GetDrive(ctx, uuid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("unknown USB drive %q", uuid)
			}
			return err
		}
		return nil
	}
	if _, err := c.store.GetNode(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("unknown node %q", id)
		}
		return err
	}
	return nil
}

// AddNode registers a peer and returns its token (shown once).
func (c *Controller) AddNode(ctx context.Context, name string) (store.Node, string, error) {
	tok := proto.NewToken()
	n, err := c.store.CreateNode(ctx, tok.NodeID, name, tok.Secret)
	if err != nil {
		return n, "", err
	}
	c.audit(ctx, "node_add", "added peer %q (%s)", n.Name, n.ID)
	return n, tok.String(), nil
}

// RotateToken issues a new token and disconnects the peer.
func (c *Controller) RotateToken(ctx context.Context, id string) (string, error) {
	tok := proto.NewTokenFor(id)
	if err := c.store.SetNodeSecret(ctx, id, tok.Secret); err != nil {
		return "", err
	}
	c.disconnect(id)
	c.audit(ctx, "node_token", "new token for peer %s", id)
	return tok.String(), nil
}

// RenameNode changes a peer's name.
func (c *Controller) RenameNode(ctx context.Context, id, name string) error {
	if err := c.store.RenameNode(ctx, id, name); err != nil {
		return err
	}
	c.audit(ctx, "node_rename", "peer %s renamed to %q", id, name)
	return nil
}

// RemoveNode deletes an unused peer and disconnects it.
func (c *Controller) RemoveNode(ctx context.Context, id string) error {
	if err := c.store.DeleteNode(ctx, id); err != nil {
		return err
	}
	c.disconnect(id)
	c.mu.Lock()
	delete(c.offSince, id)
	delete(c.offNotify, id)
	c.mu.Unlock()
	c.audit(ctx, "node_remove", "removed peer %s", id)
	return nil
}

// ReconnectPeer drops a peer's connection, e.g. one that hangs. The peer
// dials in again within seconds; runs using it fail like on any network
// drop.
func (c *Controller) ReconnectPeer(ctx context.Context, id string) error {
	c.mu.Lock()
	p := c.peers[id]
	c.mu.Unlock()
	if p == nil {
		return fmt.Errorf("%s: %w", c.nodeName(ctx, id), model.ErrOffline)
	}
	p.sess.Close()
	c.audit(ctx, "node_reconnect", "dropped the connection of peer %s so it reconnects", id)
	return nil
}

func (c *Controller) disconnect(id string) {
	c.mu.Lock()
	p := c.peers[id]
	c.mu.Unlock()
	if p != nil {
		p.sess.Close()
	}
}

// NodeUsage is a node with its disks and the backups stored on it.
type NodeUsage struct {
	NodeView
	Disks       []model.DiskInfo `json:"disks"`
	DiskError   string           `json:"disk_error,omitempty"`
	DisksStale  bool             `json:"disks_stale,omitempty"` // Disks are the last known ones
	DisksAt     int64            `json:"disks_at,omitempty"`
	BackupFiles int64            `json:"backup_files"`
	BackupBytes int64            `json:"backup_bytes"`
}

// lastKnown shows the disks a node reported before it went offline.
func (u *NodeUsage) lastKnown(d store.NodeDisks) {
	if d.At == 0 {
		return
	}
	u.Disks, u.DisksAt, u.DisksStale = d.Disks, d.At, true
}

// NodesUsage lists all nodes with live disk usage. Online nodes are asked in
// parallel; a slow node only delays the answer by a few seconds. Offline
// peers show the disks they reported last.
func (c *Controller) NodesUsage(ctx context.Context) ([]NodeUsage, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	targets, err := c.store.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	cached, err := c.store.ListNodeDisks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NodeUsage, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		out[i] = NodeUsage{NodeView: n, Disks: []model.DiskInfo{}}
		for _, t := range targets {
			if t.NodeID == n.ID {
				out[i].BackupFiles += t.StoredFiles
				out[i].BackupBytes += t.StoredBytes
			}
		}
		if !n.Online {
			out[i].lastKnown(cached[n.ID])
			continue
		}
		wg.Add(1)
		go func(u *NodeUsage) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			info, err := c.NodeInfo(ctx, u.ID)
			if err != nil {
				u.DiskError = err.Error()
				u.lastKnown(cached[u.ID])
				return
			}
			if info.Disks != nil {
				u.Disks = info.Disks
			}
			if !u.Local {
				c.store.SetNodeDisks(ctx, u.ID, u.Disks)
			}
		}(&out[i])
	}
	wg.Wait()
	return out, nil
}
