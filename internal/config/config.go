// Package config reads the container configuration from environment
// variables. Everything else is configured in the web UI.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"bfbackup/internal/model"
	"bfbackup/internal/proto"
)

// Roles.
const (
	RoleController = "controller"
	RolePeer       = "peer"
)

// Transports (BFB_TRANSPORT).
const (
	TransportTLS   = "tls"
	TransportPlain = "plain"
	TransportAuto  = "auto"
)

// Config is the process configuration.
type Config struct {
	Role     string
	DataDir  string
	HooksDir string
	Roots    []string
	UID, GID int
	LogLevel slog.Level
	// Transport: peer "tls" (default) or "plain"; controller "auto"
	// (default, accepts both) or "tls" (refuses unencrypted peers).
	Transport string

	// controller
	HTTPAddr      string
	PeerAddr      string
	AdminPassword string

	// peer
	ControllerAddr string
	Token          proto.Token
	Hostname       string
}

// Load builds the configuration for role from getenv.
func Load(role string, getenv func(string) string) (Config, error) {
	env := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	if role == "" {
		role = env("BFB_ROLE", "")
	}
	c := Config{Role: role, DataDir: env("BFB_DATA", "/config"), UID: -1, GID: -1}
	if role != RoleController && role != RolePeer {
		return c, fmt.Errorf("role must be %q or %q (argument or BFB_ROLE), got %q", RoleController, RolePeer, role)
	}
	c.HooksDir = env("BFB_HOOKS", filepath.Join(c.DataDir, "hooks"))
	for _, r := range strings.Split(env("BFB_ROOTS", "/mnt"), ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		abs, err := model.CleanAbs(r)
		if err != nil {
			return c, fmt.Errorf("BFB_ROOTS: %q must be an absolute path", r)
		}
		c.Roots = append(c.Roots, abs)
	}
	if len(c.Roots) == 0 {
		return c, errors.New("BFB_ROOTS must list at least one folder")
	}
	var err error
	if c.UID, err = optInt(env("BFB_UID", ""), "BFB_UID"); err != nil {
		return c, err
	}
	if c.GID, err = optInt(env("BFB_GID", ""), "BFB_GID"); err != nil {
		return c, err
	}
	if (c.UID < 0) != (c.GID < 0) {
		return c, errors.New("set both BFB_UID and BFB_GID, or neither")
	}
	if err := c.LogLevel.UnmarshalText([]byte(env("BFB_LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("BFB_LOG_LEVEL: %w", err)
	}

	switch role {
	case RoleController:
		c.HTTPAddr = env("BFB_HTTP_ADDR", ":7700")
		c.PeerAddr = env("BFB_PEER_ADDR", ":7701")
		c.AdminPassword = getenv("BFB_ADMIN_PASSWORD")
		c.Transport = strings.ToLower(env("BFB_TRANSPORT", TransportAuto))
		if c.Transport != TransportAuto && c.Transport != TransportTLS {
			return c, fmt.Errorf("BFB_TRANSPORT must be %q or %q on the controller, got %q", TransportAuto, TransportTLS, c.Transport)
		}
	case RolePeer:
		c.Transport = strings.ToLower(env("BFB_TRANSPORT", TransportTLS))
		if c.Transport != TransportTLS && c.Transport != TransportPlain {
			return c, fmt.Errorf("BFB_TRANSPORT must be %q or %q on a peer, got %q", TransportTLS, TransportPlain, c.Transport)
		}
		addr := env("BFB_CONTROLLER", "")
		if addr == "" {
			return c, errors.New("BFB_CONTROLLER is required (host or host:port of the controller)")
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, "7701")
		}
		c.ControllerAddr = addr
		tok := env("BFB_TOKEN", "")
		if tok == "" {
			return c, errors.New("BFB_TOKEN is required (create the peer in the controller's web UI)")
		}
		if c.Token, err = proto.ParseToken(tok); err != nil {
			return c, fmt.Errorf("BFB_TOKEN: %w", err)
		}
		c.Hostname = env("BFB_HOSTNAME", "")
	}
	return c, nil
}

func optInt(v, name string) (int, error) {
	if v == "" {
		return -1, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return -1, fmt.Errorf("%s must be a non-negative number", name)
	}
	return n, nil
}
