package server

import (
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// statusRecorder captures the response code for access logging without
// buffering the body — important here, because handlers stream forever.
type statusRecorder struct {
	http.ResponseWriter
	code    int
	written int64
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

// Unwrap lets http.NewResponseController reach the real writer so that
// Flush and deadline control keep working through this wrapper.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// A panic in one streaming handler must not take down a server
		// that other people's long-running jobs are writing to.
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler",
					"err", rec, "path", r.URL.Path, "method", r.Method)
			}
		}()

		// Conservative, self-contained security headers. The viewer uses
		// only same-origin assets, so a strict CSP costs nothing and
		// removes the whole class of injection risk from piped output.
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		if strings.HasPrefix(r.URL.Path, "/s/") || r.URL.Path == "/" {
			h.Set("Content-Security-Policy",
				"default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
		}
		// Session URLs are unauthenticated capabilities: keep them out of
		// shared caches and out of referrers.
		h.Set("Cache-Control", "no-store")

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		if rec.code == 0 {
			rec.code = http.StatusOK
		}
		s.log.Debug("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.code,
			"bytes", rec.written,
			"dur", time.Since(start).String(),
			"ip", s.clientIP(r),
		)
	})
}

// slogErrorLog adapts slog to the *log.Logger that net/http expects for
// its internal error reporting, so everything lands in one stream.
func slogErrorLog(l *slog.Logger) *log.Logger {
	return log.New(slogWriter{l}, "", 0)
}

type slogWriter struct{ l *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.l.Warn("http server", "msg", strings.TrimSpace(string(p)))
	return len(p), nil
}
