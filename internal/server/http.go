package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/purvesh/shpellcast/internal/config"
	"github.com/purvesh/shpellcast/internal/session"
)

const (
	// sseQueue is how many chunks a viewer may fall behind before it is
	// dropped and told to reconnect. Generous enough to ride out a
	// garbage collection pause, small enough to bound memory per viewer.
	sseQueue = 512
	// coalesceMax bounds how much we merge into one SSE frame.
	coalesceMax = 64 << 10
	// writeDeadline stops a wedged viewer from pinning a goroutine.
	writeDeadline = 30 * time.Second
	// keepalive keeps proxies and load balancers from closing idle tails.
	keepalive = 20 * time.Second
)

func (s *Server) routes() {
	m := s.mux

	// Producer side.
	m.HandleFunc("GET /new", s.handleNew)
	m.HandleFunc("POST /i", s.handleIngest)
	m.HandleFunc("PUT /i", s.handleIngest)
	m.HandleFunc("POST /i/{id}", s.handleIngest)
	m.HandleFunc("PUT /i/{id}", s.handleIngest)

	// Consumer side.
	m.HandleFunc("GET /s/{id}", s.handleViewer)
	m.HandleFunc("GET /s/{id}/stream", s.handleStream)
	m.HandleFunc("GET /s/{id}/raw", s.handleRaw)
	m.HandleFunc("GET /s/{id}/download", s.handleDownload)
	m.HandleFunc("GET /s/{id}/meta", s.handleMeta)

	// Owner operations, authorised by the per-session token.
	m.HandleFunc("POST /s/{id}/close", s.handleClose)
	m.HandleFunc("DELETE /s/{id}", s.handleDelete)

	// Static and operational.
	m.HandleFunc("GET /healthz", s.handleHealth)
	m.HandleFunc("GET /metrics", s.handleMetrics)
	m.HandleFunc("GET /client.sh", s.handleClientScript)
	m.HandleFunc("GET /static/{file}", s.handleStatic)
	m.HandleFunc("GET /sessions", s.handleSessions)
	m.HandleFunc("GET /", s.handleIndex)
}

// ---------------------------------------------------------------- producer

func (s *Server) handleNew(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Create(session.SourceHTTP, sanitiseLabel(r.URL.Query().Get("label")), s.clientIP(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	base := s.publicBase(r)
	body := map[string]string{
		"id":         sess.ID,
		"url":        base + "/s/" + sess.ID,
		"stream_url": base + "/s/" + sess.ID + "/raw",
		"ingest_url": base + "/i/" + sess.ID,
		"token":      sess.Token,
	}
	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%s\n%s\n%s\n", sess.ID, body["url"], body["token"])
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// handleIngest accepts a streamed HTTP body. Two shapes are supported:
//
//	PUT /i            -> create a session, stream it, return the URL at the end
//	PUT /i/{id}       -> stream into a session pre-created with /new (token required)
//
// The second shape exists because curl does not surface the response
// until the upload finishes, so a client that wants the URL up front
// calls /new first. That is what client/shpellcast.sh does.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	var (
		sess  *session.Session
		owned bool // we created it here and must finish it
	)

	if sid := r.PathValue("id"); sid != "" {
		got, err := s.store.Get(sid)
		if err != nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		// Without this check anyone who learns a session URL could inject
		// output into someone else's stream.
		if !constantTimeEqual(requestToken(r), got.Token) {
			http.Error(w, "invalid or missing session token", http.StatusForbidden)
			return
		}
		if got.IsClosed() {
			http.Error(w, "session already closed", http.StatusConflict)
			return
		}
		sess = got
	} else {
		created, err := s.store.Create(session.SourceHTTP, sanitiseLabel(r.URL.Query().Get("label")), s.clientIP(r))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		sess, owned = created, true
		// Best effort: a client that does read headers early gets the URL now.
		w.Header().Set("X-Shpellcast-Url", s.sessionURL(r, sess.ID))
		w.Header().Set("X-Shpellcast-Id", sess.ID)
	}

	s.log.Info("ingest started", "id", sess.ID, "source", "http", "ip", s.clientIP(r))
	n, copyErr := s.pump(r.Context(), sess, r.Body)
	s.store.Finish(sess)
	s.log.Info("ingest finished", "id", sess.ID, "bytes", n, "err", errText(copyErr))

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if owned || copyErr != nil {
		status := http.StatusOK
		if errors.Is(copyErr, session.ErrQuotaExceeded) {
			status = http.StatusInsufficientStorage
		}
		w.WriteHeader(status)
	}
	fmt.Fprintf(w, "%s\n", s.sessionURL(r, sess.ID))
	if errors.Is(copyErr, session.ErrQuotaExceeded) {
		fmt.Fprintf(w, "note: session quota of %s reached; output was truncated\n",
			config.HumanBytes(s.cfg.QuotaBytes))
	}
}

// pump copies producer bytes into a session. It is shared by the TCP and
// HTTP ingest paths so both enforce the same quota semantics.
func (s *Server) pump(ctx context.Context, sess *session.Session, src io.Reader) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if _, werr := sess.Write(buf[:n]); werr != nil {
				return total, werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}

// ---------------------------------------------------------------- consumer

func (s *Server) handleViewer(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if _, err := s.store.Get(sid); err != nil {
		s.renderNotFound(w)
		return
	}
	page := strings.NewReplacer(
		"{{ID}}", sid,
		"{{VERSION}}", Version,
	).Replace(string(mustAsset("view.html")))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, page)
}

// handleStream is the live viewer transport: Server-Sent Events.
//
// SSE rather than WebSocket, deliberately: the payload is one-directional,
// it survives reverse proxies and corporate middleboxes that mangle
// upgrades, the browser reconnects on its own, and it needs no
// dependencies on either side. Chunks are base64 so that arbitrary binary
// output (including invalid UTF-8 and embedded newlines) can never break
// the line-oriented SSE framing.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // tell nginx not to buffer this
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	_ = rc.Flush()

	replayWanted := r.URL.Query().Get("replay") != "0"
	replay, ch, cancel := sess.Subscribe(sseQueue)
	defer cancel()

	st := sess.Stat()
	if meta, err := json.Marshal(st); err == nil {
		if !s.sseEvent(w, rc, "meta", meta) {
			return
		}
	}
	if replayWanted && len(replay) > 0 {
		if !s.sseData(w, rc, replay) {
			return
		}
	}
	if st.Closed {
		s.sseEvent(w, rc, "end", []byte(`{"reason":"closed"}`))
		return
	}

	tick := time.NewTicker(keepalive)
	defer tick.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-tick.C:
			_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}

		case chunk, ok := <-ch:
			if !ok {
				if sess.IsClosed() {
					s.sseEvent(w, rc, "end", []byte(`{"reason":"closed"}`))
				} else {
					// We were dropped for lagging; the client reconnects
					// and replays, so it sees a gap marker, not a stall.
					s.sseEvent(w, rc, "gap", []byte(`{"reason":"slow-client"}`))
				}
				return
			}
			// Merge whatever else is already queued into one frame.
			batch := chunk
		drain:
			for len(batch) < coalesceMax {
				select {
				case more, ok := <-ch:
					if !ok {
						break drain
					}
					batch = append(append(make([]byte, 0, len(batch)+len(more)), batch...), more...)
				default:
					break drain
				}
			}
			if !s.sseData(w, rc, batch) {
				return
			}
		}
	}
}

func (s *Server) sseData(w http.ResponseWriter, rc *http.ResponseController, b []byte) bool {
	enc := base64.StdEncoding.EncodeToString(b)
	return s.sseEvent(w, rc, "out", []byte(enc))
}

func (s *Server) sseEvent(w http.ResponseWriter, rc *http.ResponseController, event string, payload []byte) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// handleRaw is the terminal-native tail: plain bytes, no framing at all,
// so `curl -N host/s/<id>/raw` behaves like `tail -f` over HTTP.
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found\n", http.StatusNotFound)
		return
	}
	follow := r.URL.Query().Get("follow") != "0"

	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	replay, ch, cancel := sess.Subscribe(sseQueue)
	defer cancel()

	if len(replay) > 0 {
		_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))
		if _, err := w.Write(replay); err != nil {
			return
		}
	}
	_ = rc.Flush()
	if !follow {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case chunk, ok := <-ch:
			if !ok {
				if !sess.IsClosed() {
					_, _ = io.WriteString(w, "\n[shpellcast] viewer fell behind; reconnect to resume\n")
				}
				_ = rc.Flush()
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
	}
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found\n", http.StatusNotFound)
		return
	}
	data := sess.Snapshot()
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "shpellcast-"+sess.ID+".log"))
	_, _ = w.Write(data)
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, sess.Stat())
}

// ---------------------------------------------------------------- owner ops

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if !constantTimeEqual(requestToken(r), sess.Token) {
		http.Error(w, "invalid or missing session token", http.StatusForbidden)
		return
	}
	s.store.Finish(sess)
	writeJSON(w, http.StatusOK, map[string]string{"status": "closed", "id": sess.ID})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if !constantTimeEqual(requestToken(r), sess.Token) {
		http.Error(w, "invalid or missing session token", http.StatusForbidden)
		return
	}
	_ = s.store.Delete(sess.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "id": sess.ID})
}

// ---------------------------------------------------------------- static/ops

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.renderNotFound(w)
		return
	}
	base := s.publicBase(r)
	page := strings.NewReplacer(
		"{{BASE}}", base,
		"{{TCP_TARGET}}", s.tcpTarget(r),
		"{{VERSION}}", Version,
		"{{BUFFER}}", config.HumanBytes(int64(s.cfg.BufferBytes)),
		"{{QUOTA}}", config.HumanBytes(s.cfg.QuotaBytes),
		"{{IDLE_TTL}}", s.cfg.IdleTTL.String(),
	).Replace(string(mustAsset("index.html")))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, page)
}

// handleSessions lists live sessions. Off by default: a session URL is a
// capability, so publishing the list would hand every visitor access to
// everyone else's output.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.IndexEnabled {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "session listing is disabled; start with -index to enable it",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.store.List()})
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	data, ok := asset(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(data)
}

// handleClientScript serves the helper so a fresh machine can do
// `curl -fsSL http://host/client.sh -o ~/bin/shpellcast`.
func (s *Server) handleClientScript(w http.ResponseWriter, r *http.Request) {
	script := strings.NewReplacer(
		"{{BASE}}", s.publicBase(r),
		"{{TCP_TARGET}}", s.tcpTarget(r),
	).Replace(string(mustAsset("shpellcast.sh")))
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = io.WriteString(w, script)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	c := s.store.Counters()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"version":  Version,
		"uptime_s": int64(time.Since(s.startedAt).Seconds()),
		"sessions": c.Live,
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	c := s.store.Counters()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP shpellcast_sessions_live Currently retained sessions.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_sessions_live gauge\nshpellcast_sessions_live %d\n", c.Live)
	fmt.Fprintf(w, "# HELP shpellcast_sessions_created_total Sessions created since start.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_sessions_created_total counter\nshpellcast_sessions_created_total %d\n", c.Created)
	fmt.Fprintf(w, "# HELP shpellcast_sessions_reaped_total Sessions expired and removed.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_sessions_reaped_total counter\nshpellcast_sessions_reaped_total %d\n", c.Reaped)
	fmt.Fprintf(w, "# HELP shpellcast_sessions_rejected_total Session creations refused by a limit.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_sessions_rejected_total counter\nshpellcast_sessions_rejected_total %d\n", c.Rejected)
	// Split by reason: hitting the global cap is a capacity problem,
	// hitting the per-client cap is usually a configuration problem.
	fmt.Fprintf(w, "# HELP shpellcast_sessions_rejected_capacity_total Refused because the server-wide session cap was reached.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_sessions_rejected_capacity_total counter\nshpellcast_sessions_rejected_capacity_total %d\n", c.RejectedGlobal)
	fmt.Fprintf(w, "# HELP shpellcast_sessions_rejected_perclient_total Refused because one source address hit its session cap.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_sessions_rejected_perclient_total counter\nshpellcast_sessions_rejected_perclient_total %d\n", c.RejectedClient)
	fmt.Fprintf(w, "# HELP shpellcast_bytes_ingested_total Bytes accepted from producers.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_bytes_ingested_total counter\nshpellcast_bytes_ingested_total %d\n", c.BytesIn)
	fmt.Fprintf(w, "# HELP shpellcast_viewers_live Viewers currently attached to a session.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_viewers_live gauge\nshpellcast_viewers_live %d\n", c.ViewersLive)
	fmt.Fprintf(w, "# HELP shpellcast_viewers_total Viewer attachments since start.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_viewers_total counter\nshpellcast_viewers_total %d\n", c.ViewersTotal)
	// The one number that tells you consumers cannot keep up. Producers are
	// never slowed down, so this is where that pressure becomes visible.
	fmt.Fprintf(w, "# HELP shpellcast_viewers_dropped_total Viewers disconnected for falling behind.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_viewers_dropped_total counter\nshpellcast_viewers_dropped_total %d\n", c.ViewersDropped)
	fmt.Fprintf(w, "# HELP shpellcast_uptime_seconds Process uptime.\n")
	fmt.Fprintf(w, "# TYPE shpellcast_uptime_seconds gauge\nshpellcast_uptime_seconds %d\n", int64(time.Since(s.startedAt).Seconds()))

	// Runtime stats, in the names the Prometheus Go client would use, so
	// existing dashboards and alerts work without translation. Capacity
	// planning for this service is goroutines and heap, nothing else.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(w, "# HELP go_goroutines Goroutines currently running.\n")
	fmt.Fprintf(w, "# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "# HELP go_memstats_heap_inuse_bytes Heap bytes in use.\n")
	fmt.Fprintf(w, "# TYPE go_memstats_heap_inuse_bytes gauge\ngo_memstats_heap_inuse_bytes %d\n", ms.HeapInuse)
	fmt.Fprintf(w, "# HELP go_memstats_heap_alloc_bytes Heap bytes allocated and live.\n")
	fmt.Fprintf(w, "# TYPE go_memstats_heap_alloc_bytes gauge\ngo_memstats_heap_alloc_bytes %d\n", ms.HeapAlloc)
	fmt.Fprintf(w, "# HELP go_memstats_sys_bytes Memory obtained from the OS.\n")
	fmt.Fprintf(w, "# TYPE go_memstats_sys_bytes gauge\ngo_memstats_sys_bytes %d\n", ms.Sys)
	fmt.Fprintf(w, "# HELP go_gc_cycles_total Completed GC cycles.\n")
	fmt.Fprintf(w, "# TYPE go_gc_cycles_total counter\ngo_gc_cycles_total %d\n", ms.NumGC)
	if n, err := openFDs(); err == nil {
		// File descriptors are the real ceiling: one socket per producer,
		// one per viewer. Compare against LimitNOFILE in the unit file.
		fmt.Fprintf(w, "# HELP process_open_fds Open file descriptors.\n")
		fmt.Fprintf(w, "# TYPE process_open_fds gauge\nprocess_open_fds %d\n", n)
	}
}

// openFDs counts this process's descriptors on Linux. Returns an error
// anywhere /proc is unavailable, in which case the metric is omitted.
func openFDs() (int, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

func (s *Server) renderNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(mustAsset("404.html"))
}

// ---------------------------------------------------------------- helpers

// tcpTarget renders the host:port a producer should pipe into.
func (s *Server) tcpTarget(r *http.Request) string {
	if s.cfg.TCPAddr == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(s.cfg.TCPAddr)
	if err != nil {
		port = strings.TrimPrefix(s.cfg.TCPAddr, ":")
	}
	host := "localhost"
	if u, err := url.Parse(s.publicBase(r)); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return host + " " + port
}

func requestToken(r *http.Request) string {
	if v := r.Header.Get("X-Shpellcast-Token"); v != "" {
		return v
	}
	return r.URL.Query().Get("token")
}

// constantTimeEqual avoids leaking token contents through timing.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// sanitiseLabel keeps producer-supplied text harmless: printable ASCII,
// bounded length, no markup.
func sanitiseLabel(in string) string {
	if in == "" {
		return ""
	}
	if len(in) > 80 {
		in = in[:80]
	}
	var b strings.Builder
	for _, r := range in {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop control characters
		case r == '<' || r == '>' || r == '&' || r == '"' || r == '\'':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrTooMany), errors.Is(err, session.ErrTooManyClient):
		w.Header().Set("Retry-After", "30")
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	default:
		http.Error(w, "could not create session", http.StatusInternalServerError)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
