package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/purvesh/shpellcast/internal/session"
)

// serveTCP accepts raw producer connections. The protocol is deliberately
// the simplest thing that works everywhere:
//
//	$ long_job | nc cast.example.com 1337
//	shpellcast: https://cast.example.com/s/4q8z...
//
// On connect the server allocates a session and writes one banner line
// back, which netcat prints on the producer's terminal. Everything the
// client sends from then on is session output. Nothing to install, no
// protocol negotiation, works from any box that has nc, bash's
// /dev/tcp, socat, busybox or telnet.
func (s *Server) serveTCP(ctx context.Context, ln net.Listener) {
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// Back off on transient accept errors (fd exhaustion) rather
			// than spinning the CPU at 100%.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else if delay < time.Second {
				delay *= 2
			}
			s.log.Warn("tcp accept failed", "err", err, "retry_in", delay.String())
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
			continue
		}
		delay = 0

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleTCPConn(ctx, conn)
		}()
	}
}

func (s *Server) handleTCPConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	clientIP := "unknown"
	if host, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
		clientIP = host
	}

	sess, err := s.store.Create(session.SourceTCP, "", clientIP)
	if err != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(conn, "shpellcast: refused: %v\n", err)
		s.log.Warn("tcp session refused", "ip", clientIP, "err", err)
		return
	}
	defer s.store.Finish(sess)

	url := s.sessionURL(nil, sess.ID)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "shpellcast: %s\n", url); err != nil {
		s.log.Warn("tcp banner write failed", "id", sess.ID, "err", err)
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})

	s.log.Info("ingest started", "id", sess.ID, "source", "tcp", "ip", clientIP)

	// Close the connection as soon as the server is shutting down.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	total, err := s.copyWithIdleTimeout(sess, conn)
	s.log.Info("ingest finished", "id", sess.ID, "bytes", total, "err", errText(err))

	// Tell the producer where its output ended up, so the URL is still on
	// screen after a long run scrolled the banner away.
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if errors.Is(err, session.ErrQuotaExceeded) {
		fmt.Fprintf(conn, "\nshpellcast: session quota reached, output truncated: %s\n", url)
	} else {
		fmt.Fprintf(conn, "\nshpellcast: %s\n", url)
	}
}

// copyWithIdleTimeout streams conn into the session, dropping producers
// that go silent for longer than the configured idle window so that
// abandoned connections cannot pin file descriptors indefinitely.
func (s *Server) copyWithIdleTimeout(sess *session.Session, conn net.Conn) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		if s.cfg.TCPIdle > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.cfg.TCPIdle))
		}
		n, rerr := conn.Read(buf)
		if n > 0 {
			total += int64(n)
			if _, werr := sess.Write(buf[:n]); werr != nil {
				return total, werr
			}
		}
		if rerr != nil {
			var ne net.Error
			if errors.As(rerr, &ne) && ne.Timeout() {
				return total, fmt.Errorf("producer idle for %s", s.cfg.TCPIdle)
			}
			return total, ignoreEOF(rerr)
		}
	}
}

func ignoreEOF(err error) error {
	if err == nil {
		return nil
	}
	if err.Error() == "EOF" {
		return nil
	}
	return err
}
