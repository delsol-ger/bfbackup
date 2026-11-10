// Command bfbackup runs a BFBackup controller or peer.
//
//	bfbackup controller   web UI, scheduler, database (BFB_* env vars)
//	bfbackup peer         connects to a controller (BFB_CONTROLLER, BFB_TOKEN)
//	bfbackup version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"bfbackup/internal/config"
	"bfbackup/internal/controller"
	"bfbackup/internal/endpoint"
	"bfbackup/internal/fsx"
	"bfbackup/internal/hooks"
	"bfbackup/internal/peer"
	"bfbackup/internal/proto"
	"bfbackup/internal/web"
)

// version is the release. Bump it for every new build; -ldflags
// "-X main.version=..." (Docker build arg VERSION) overrides it.
var version = "1.16.1"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	role := ""
	if len(args) > 0 {
		role = args[0]
	}
	if role == "version" || role == "--version" {
		fmt.Println(version)
		return 0
	}
	cfg, err := config.Load(role, os.Getenv)
	var connLog *peer.ConnLog
	if cfg.Role == config.RolePeer {
		// The connection log also records configuration errors, which stop
		// the peer before it ever tries to connect.
		connLog = openConnLog(cfg.DataDir)
		defer connLog.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bfbackup:", err)
		connLog.Printf("configuration error, the peer does not start: %v", err)
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	if err := os.MkdirAll(cfg.HooksDir, 0o755); err != nil {
		log.Warn("cannot create hooks folder", "dir", cfg.HooksDir, "err", err)
	}
	roots, notes, err := fsx.HostRoots(cfg.Roots)
	if err != nil {
		log.Error("cannot use roots", "err", err)
		connLog.Printf("the peer does not start: %v", err)
		return 1
	}
	for _, n := range notes {
		log.Warn(n)
		connLog.Printf("%s", n)
	}
	cfg.Roots = roots
	files, err := fsx.New(cfg.Roots, fsx.Options{UID: cfg.UID, GID: cfg.GID})
	if err != nil {
		log.Error("cannot open roots", "err", err)
		connLog.Printf("cannot open BFB_ROOTS %v, the peer does not start: %v", cfg.Roots, err)
		return 1
	}
	defer files.Close()
	local := &endpoint.Local{FS: files, Hooks: hooks.Runner{Dir: cfg.HooksDir}, Version: version}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("starting bfbackup", "role", cfg.Role, "version", version, "roots", cfg.Roots)

	switch cfg.Role {
	case config.RoleController:
		err = runController(ctx, cfg, local, log)
	case config.RolePeer:
		err = runPeer(ctx, cfg, local, log, connLog)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("stopped with error", "err", err)
		return 1
	}
	log.Info("stopped")
	return 0
}

func runController(ctx context.Context, cfg config.Config, local *endpoint.Local, log *slog.Logger) error {
	tlsSec, err := proto.NewTLS()
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	c, err := controller.New(controller.Config{
		DataDir:    cfg.DataDir,
		Version:    version,
		Local:      local,
		Security:   proto.Auto{TLS: tlsSec},
		RequireTLS: cfg.Transport == config.TransportTLS,
		Log:        log,
	})
	if err != nil {
		return err
	}
	defer c.Close()

	ln, err := net.Listen("tcp", cfg.PeerAddr)
	if err != nil {
		return fmt.Errorf("peer port: %w", err)
	}
	c.Start(ctx, ln)

	ws := web.New(c, log)
	if err := ws.InitPassword(ctx, cfg.AdminPassword); err != nil {
		return fmt.Errorf("BFB_ADMIN_PASSWORD: %w", err)
	}
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           ws.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("controller ready", "web", cfg.HTTPAddr, "peers", cfg.PeerAddr, "transport", cfg.Transport)

	select {
	case <-ctx.Done():
	case err := <-errc:
		return fmt.Errorf("web server: %w", err)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	return nil
}

func runPeer(ctx context.Context, cfg config.Config, local *endpoint.Local, log *slog.Logger, connLog *peer.ConnLog) error {
	host := cfg.Hostname
	if host == "" {
		host, _ = os.Hostname()
	}
	var sec proto.Security = &proto.TLS{}
	if cfg.Transport == config.TransportPlain {
		sec = proto.Plain{}
	}
	a := &peer.Agent{
		Addr:     cfg.ControllerAddr,
		Token:    cfg.Token,
		EP:       local,
		Security: sec,
		Info:     proto.PeerInfo{Hostname: host, Version: version},
		Log:      log,
		ConnLog:  connLog,
	}
	return a.Run(ctx)
}

// connLogMax caps the connection log; one older file (.1) is kept.
const connLogMax = 5 << 20

// openConnLog opens <data>/connection.log. Without a writable data folder
// the peer still runs; the reason is printed to the container log.
func openConnLog(dataDir string) *peer.ConnLog {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "bfbackup: no connection log:", err)
		return nil
	}
	l, err := peer.OpenConnLog(filepath.Join(dataDir, peer.ConnLogName), connLogMax)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bfbackup: no connection log:", err)
		return nil
	}
	return l
}
