// Command shpellcast is a self-hosted service that turns the output of any
// command-line program into a live web page.
//
//	long_job | nc host 1337        -> https://host/s/<random-id>
//	long_job | curl -T- host/i     -> same, over HTTP
//
// It is a single static binary with no runtime dependencies, no database
// and no accounts. Session URLs are unguessable capabilities that expire.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/purvesh/shpellcast/internal/config"
	"github.com/purvesh/shpellcast/internal/server"
)

// Set at build time:
//
//	go build -ldflags "-X main.version=1.0.0 -X main.commit=abc123"
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "shpellcast: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Parse(os.Args[1:], os.Stderr)
	if err != nil {
		return err
	}

	server.Version = version

	if cfg.Version {
		fmt.Printf("shpellcast %s (commit %s, built %s, %s/%s, %s)\n",
			version, commit, date, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return nil
	}

	log := newLogger(cfg.LogLevel)
	log.Info("starting shpellcast",
		"version", version,
		"commit", commit,
		"http", cfg.HTTPAddr,
		"tcp", cfg.TCPAddr,
		"buffer", config.HumanBytes(int64(cfg.BufferBytes)),
		"quota", config.HumanBytes(cfg.QuotaBytes),
		"spill_dir", cfg.SpillDir,
		"trust_proxy", cfg.TrustProxy,
	)

	// State the capacity envelope at startup. Memory is the product of the
	// session cap and the per-session buffer, and it is the number that
	// decides whether this process fits in its cgroup.
	log.Info("capacity",
		"max_sessions", cfg.MaxSessions,
		"max_per_client", cfg.MaxPerClient,
		"buffer_per_session", config.HumanBytes(int64(cfg.BufferBytes)),
		"buffer_memory_at_cap", config.HumanBytes(int64(cfg.MaxSessions)*int64(cfg.BufferBytes)),
	)
	if cfg.MaxPerClient > 0 && cfg.MaxPerClient < 100 {
		log.Warn("-max-per-client is low; producers sharing a source address "+
			"(NAT, CI runners, or a reverse proxy without -trust-proxy) are counted as one client "+
			"and will be refused past this many concurrent sessions",
			"max_per_client", cfg.MaxPerClient)
	}

	if cfg.BaseURL == "" {
		log.Warn("no -base-url set; links are derived from the request Host header. " +
			"Set -base-url to your public origin when running behind a reverse proxy.")
	}

	srv, err := server.New(cfg, log)
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM drain live connections instead of killing them, so a
	// restart does not lose the tail of someone's running job.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return srv.Run(ctx)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	// Plain text to stderr: systemd/journald and docker logs both handle
	// it, and it stays readable when someone runs the binary by hand.
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
