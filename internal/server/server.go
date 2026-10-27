// Package server wires shpellcast's two ingest paths (raw TCP and HTTP
// chunked upload) to its two egress paths (a browser viewer over SSE and
// a plain-text tail for curl).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/purvesh/shpellcast/internal/config"
	"github.com/purvesh/shpellcast/internal/session"
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

// Server owns the listeners and the session store.
type Server struct {
	cfg   config.Config
	log   *slog.Logger
	store *session.Store
	mux   *http.ServeMux

	httpSrv *http.Server
	tcpLn   net.Listener
	httpLn  net.Listener

	ready chan struct{} // closed once both listeners are bound

	wg        sync.WaitGroup
	startedAt time.Time
}

// New constructs a Server. It does not bind any ports; call Run.
func New(cfg config.Config, log *slog.Logger) (*Server, error) {
	store, err := session.NewStore(session.Options{
		BufferBytes:  cfg.BufferBytes,
		QuotaBytes:   cfg.QuotaBytes,
		MaxSessions:  cfg.MaxSessions,
		MaxPerClient: cfg.MaxPerClient,
		IdleTTL:      cfg.IdleTTL,
		ClosedTTL:    cfg.ClosedTTL,
		MaxTTL:       cfg.MaxTTL,
		SpillDir:     cfg.SpillDir,
		IDLength:     cfg.IDLength,
	})
	if err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}

	s := &Server{
		cfg:       cfg,
		log:       log,
		store:     store,
		mux:       http.NewServeMux(),
		startedAt: time.Now(),
		ready:     make(chan struct{}),
	}
	s.routes()

	s.httpSrv = &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: s.middleware(s.mux),
		// Streaming responses and long uploads must not be guillotined,
		// so only the header read is bounded. Idle connections are.
		ReadHeaderTimeout: cfg.ReadTimeout,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slogErrorLog(log),
	}
	return s, nil
}

// Run binds the listeners and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	lnCfg := net.ListenConfig{KeepAlive: 30 * time.Second}

	httpLn, err := lnCfg.Listen(ctx, "tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http %s: %w", s.cfg.HTTPAddr, err)
	}
	s.httpLn = httpLn
	s.log.Info("http listening", "addr", httpLn.Addr().String(), "base_url", s.publicBase(nil))

	if s.cfg.TCPAddr != "" {
		tcpLn, err := lnCfg.Listen(ctx, "tcp", s.cfg.TCPAddr)
		if err != nil {
			_ = httpLn.Close()
			return fmt.Errorf("listen tcp %s: %w", s.cfg.TCPAddr, err)
		}
		s.tcpLn = tcpLn
		s.log.Info("tcp ingest listening", "addr", tcpLn.Addr().String())
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveTCP(ctx, tcpLn)
		}()
	}

	close(s.ready)

	errCh := make(chan error, 1)
	go func() {
		if err := s.httpSrv.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			s.shutdown()
			return err
		}
	}

	s.log.Info("shutting down")
	s.shutdown()
	return nil
}

// Ready is closed once both listeners are bound. Useful for tests and for
// readiness probes that must not race with startup.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Addrs reports the addresses actually bound, which differ from the
// configured ones when port 0 was requested.
func (s *Server) Addrs() (httpAddr, tcpAddr string) {
	if s.httpLn != nil {
		httpAddr = s.httpLn.Addr().String()
	}
	if s.tcpLn != nil {
		tcpAddr = s.tcpLn.Addr().String()
	}
	return
}

func (s *Server) shutdown() {
	if s.tcpLn != nil {
		_ = s.tcpLn.Close()
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(shutCtx)
	s.store.Close() // releases live viewers so handlers can return
	s.wg.Wait()
}

// publicBase returns the origin to use when building session links.
// Preference: explicit -base-url, then forwarded headers when the proxy
// is trusted, then the request Host, then the listen address.
func (s *Server) publicBase(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
	}
	if r != nil {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		host := r.Host
		if s.cfg.TrustProxy {
			if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
				scheme = firstToken(v)
			}
			if v := r.Header.Get("X-Forwarded-Host"); v != "" {
				host = firstToken(v)
			}
		}
		if host != "" {
			return scheme + "://" + host
		}
	}
	host := s.cfg.HTTPAddr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	return "http://" + host
}

func (s *Server) sessionURL(r *http.Request, sid string) string {
	return s.publicBase(r) + "/s/" + sid
}

func firstToken(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// clientIP extracts a client identifier for per-client quotas.
// X-Forwarded-For is only honoured when the operator has declared the
// server to be behind a trusted proxy; otherwise it is trivially spoofed
// and would let one host bypass every per-client limit.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return firstToken(v)
		}
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return strings.TrimSpace(v)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
