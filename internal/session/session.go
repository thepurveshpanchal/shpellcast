package session

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Errors returned by Write.
var (
	// ErrClosed means the producer already finished this session.
	ErrClosed = errors.New("session closed")
	// ErrQuotaExceeded means the session hit its byte cap. The producer
	// connection should be terminated; retained output stays readable.
	ErrQuotaExceeded = errors.New("session byte quota exceeded")
)

// Metrics are process-wide counters shared by every session. They are
// read by /metrics and are the first thing to look at when a server under
// load is behaving oddly: a climbing ViewersDropped means consumers
// cannot keep up, which is survivable, while a climbing goroutine count
// with flat session numbers means something is leaking.
type Metrics struct {
	BytesIn        atomic.Int64
	ViewersLive    atomic.Int64
	ViewersTotal   atomic.Int64
	ViewersDropped atomic.Int64
}

// Source describes how a producer delivered its output.
type Source string

const (
	SourceTCP  Source = "tcp"
	SourceHTTP Source = "http"
)

// subscriber is one live viewer attached to a session.
//
// Backpressure policy: a session must never be slowed down by a slow
// viewer — the whole point is that the producing command keeps running at
// full speed. So each subscriber gets a bounded queue; if it fills, the
// subscriber is dropped rather than blocking the writer. Dropped viewers
// reconnect and replay from the ring, so the user-visible effect is a
// brief gap marker, not a stalled process.
type subscriber struct {
	ch     chan []byte
	once   sync.Once
	closed bool
}

func (s *subscriber) close() {
	s.once.Do(func() { close(s.ch) })
}

// Session is a single stream of output addressed by its ID.
type Session struct {
	ID        string
	Token     string // secret for privileged ops (close/delete)
	Source    Source
	Label     string // optional, producer-supplied, sanitised
	Client    string // coarse client descriptor for logs (IP only)
	CreatedAt time.Time

	mu         sync.RWMutex
	ring       *Ring
	subs       map[*subscriber]struct{}
	lastWrite  time.Time
	closed     bool
	closedAt   time.Time
	quota      int64
	quotaHit   bool
	spill      *os.File
	spillPath  string
	spillBytes int64
	viewers    int64 // cumulative viewer count, for the index page
	metrics    *Metrics
}

// Config for a single session, supplied by the store.
type sessionConfig struct {
	bufferBytes int
	quotaBytes  int64
	spillDir    string
	metrics     *Metrics
}

func newSession(sid, token string, src Source, label, client string, cfg sessionConfig, now time.Time) (*Session, error) {
	s := &Session{
		ID:        sid,
		Token:     token,
		Source:    src,
		Label:     label,
		Client:    client,
		CreatedAt: now,
		ring:      NewRing(cfg.bufferBytes),
		subs:      make(map[*subscriber]struct{}),
		lastWrite: now,
		quota:     cfg.quotaBytes,
		metrics:   cfg.metrics,
	}
	if s.metrics == nil {
		s.metrics = &Metrics{}
	}
	if cfg.spillDir != "" {
		p := filepath.Join(cfg.spillDir, sid+".log")
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return nil, err
		}
		s.spill = f
		s.spillPath = p
	}
	return s, nil
}

// Write appends producer output and fans it out to live viewers.
// It is safe for concurrent use but a session normally has one producer.
func (s *Session) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	if s.quota > 0 && s.ring.Written() >= s.quota {
		s.quotaHit = true
		s.mu.Unlock()
		return 0, ErrQuotaExceeded
	}

	// Copy once; the slice is then immutable and shared with all viewers.
	chunk := make([]byte, len(p))
	copy(chunk, p)

	_, _ = s.ring.Write(chunk)
	s.lastWrite = time.Now()
	s.metrics.BytesIn.Add(int64(len(chunk)))

	if s.spill != nil {
		if n, err := s.spill.Write(chunk); err == nil {
			s.spillBytes += int64(n)
		}
		// A failing spill file must not kill a live stream; the ring is
		// still authoritative for the viewer.
	}

	for sub := range s.subs {
		select {
		case sub.ch <- chunk:
		default:
			// Slow viewer: drop it instead of blocking the producer.
			sub.close()
			delete(s.subs, sub)
			s.metrics.ViewersLive.Add(-1)
			s.metrics.ViewersDropped.Add(1)
		}
	}
	quotaHit := s.quota > 0 && s.ring.Written() >= s.quota
	s.quotaHit = quotaHit
	s.mu.Unlock()

	if quotaHit {
		return len(p), ErrQuotaExceeded
	}
	return len(p), nil
}

// Close marks the producer side finished and releases live viewers.
// Retained output stays readable until the session is reaped.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.closedAt = time.Now()
	for sub := range s.subs {
		sub.close()
		delete(s.subs, sub)
		s.metrics.ViewersLive.Add(-1)
	}
	if s.spill != nil {
		_ = s.spill.Sync()
		_ = s.spill.Close()
		s.spill = nil
	}
	s.mu.Unlock()
}

// Subscribe atomically captures the current replay buffer and registers a
// live subscriber, so no bytes can be lost between the two.
//
// The returned channel is closed when the session ends or when this
// subscriber falls too far behind; use Closed() to tell the two apart.
// Callers must call Unsubscribe when finished.
func (s *Session) Subscribe(queue int) (replay []byte, ch <-chan []byte, cancel func()) {
	if queue < 8 {
		queue = 8
	}
	sub := &subscriber{ch: make(chan []byte, queue)}

	s.mu.Lock()
	replay = s.ring.Snapshot()
	s.viewers++
	s.metrics.ViewersTotal.Add(1)
	if s.closed {
		s.mu.Unlock()
		sub.close()
		return replay, sub.ch, func() {}
	}
	s.subs[sub] = struct{}{}
	s.metrics.ViewersLive.Add(1)
	s.mu.Unlock()

	return replay, sub.ch, func() {
		s.mu.Lock()
		if _, ok := s.subs[sub]; ok {
			delete(s.subs, sub)
			s.metrics.ViewersLive.Add(-1)
		}
		s.mu.Unlock()
		sub.close()
	}
}

// Snapshot returns the retained output.
func (s *Session) Snapshot() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ring.Snapshot()
}

// SpillPath returns the on-disk log path, or "" when spilling is off.
func (s *Session) SpillPath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.spillPath
}

// Stat is an immutable view of a session for APIs and the index page.
type Stat struct {
	ID        string    `json:"id"`
	Source    Source    `json:"source"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	LastWrite time.Time `json:"last_write"`
	Bytes     int64     `json:"bytes"`
	Retained  int       `json:"retained_bytes"`
	Truncated bool      `json:"truncated"`
	QuotaHit  bool      `json:"quota_exceeded"`
	Closed    bool      `json:"closed"`
	ClosedAt  time.Time `json:"closed_at,omitempty"`
	Viewers   int       `json:"live_viewers"`
}

// Stat returns a consistent snapshot of session metadata.
func (s *Session) Stat() Stat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Stat{
		ID:        s.ID,
		Source:    s.Source,
		Label:     s.Label,
		CreatedAt: s.CreatedAt,
		LastWrite: s.lastWrite,
		Bytes:     s.ring.Written(),
		Retained:  s.ring.Len(),
		Truncated: s.ring.Truncated(),
		QuotaHit:  s.quotaHit,
		Closed:    s.closed,
		ClosedAt:  s.closedAt,
		Viewers:   len(s.subs),
	}
}

// IsClosed reports whether the producer has finished.
func (s *Session) IsClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}
