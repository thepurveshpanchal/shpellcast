package session

import (
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/purvesh/shpellcast/internal/id"
)

// Errors returned by the store.
var (
	ErrNotFound      = errors.New("session not found")
	ErrTooMany       = errors.New("server session limit reached")
	ErrTooManyClient = errors.New("per-client session limit reached")
)

// Options configures the store. Zero values get sane defaults in New.
type Options struct {
	// BufferBytes is the per-session replay buffer (memory cap per session).
	BufferBytes int
	// QuotaBytes caps total output accepted per session; 0 disables.
	QuotaBytes int64
	// MaxSessions caps concurrently retained sessions; 0 disables.
	MaxSessions int
	// MaxPerClient caps live sessions per client IP; 0 disables.
	MaxPerClient int
	// IdleTTL reaps a session this long after its last write.
	IdleTTL time.Duration
	// ClosedTTL reaps a session this long after the producer disconnects.
	ClosedTTL time.Duration
	// MaxTTL reaps a session this long after creation, no matter what.
	MaxTTL time.Duration
	// SpillDir, when set, mirrors every session to a file there so output
	// survives a restart and can be downloaded without holding it in RAM.
	SpillDir string
	// IDLength is the number of characters in a session identifier.
	IDLength int
}

func (o *Options) withDefaults() {
	if o.BufferBytes <= 0 {
		o.BufferBytes = 1 << 18 // 256 KiB
	}
	if o.IdleTTL <= 0 {
		o.IdleTTL = 24 * time.Hour
	}
	if o.ClosedTTL <= 0 {
		o.ClosedTTL = time.Hour
	}
	if o.MaxTTL <= 0 {
		o.MaxTTL = 7 * 24 * time.Hour
	}
	if o.IDLength <= 0 {
		o.IDLength = 16
	}
}

// Store holds all live sessions and reaps expired ones.
type Store struct {
	opts Options

	mu       sync.RWMutex
	sessions map[string]*Session
	perIP    map[string]int

	metrics *Metrics

	stopOnce sync.Once
	stop     chan struct{}

	// counters, read by /metrics
	createdTotal   uint64
	reapedTotal    uint64
	rejectedinc    uint64
	rejectedGlobal uint64
	rejectedClient uint64
}

// NewStore builds a store and starts its reaper goroutine.
func NewStore(opts Options) (*Store, error) {
	opts.withDefaults()
	if opts.SpillDir != "" {
		if err := os.MkdirAll(opts.SpillDir, 0o750); err != nil {
			return nil, err
		}
	}
	s := &Store{
		opts:     opts,
		sessions: make(map[string]*Session),
		perIP:    make(map[string]int),
		metrics:  &Metrics{},
		stop:     make(chan struct{}),
	}
	go s.reapLoop()
	return s, nil
}

// Create allocates a new session for a producer.
func (s *Store) Create(src Source, label, client string) (*Session, error) {
	sid, err := id.New(s.opts.IDLength)
	if err != nil {
		return nil, err
	}
	tok, err := id.Token()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.opts.MaxSessions > 0 && len(s.sessions) >= s.opts.MaxSessions {
		s.rejectedinc++
		s.rejectedGlobal++
		s.mu.Unlock()
		return nil, ErrTooMany
	}
	if s.opts.MaxPerClient > 0 && client != "" && s.perIP[client] >= s.opts.MaxPerClient {
		s.rejectedinc++
		s.rejectedClient++
		s.mu.Unlock()
		return nil, ErrTooManyClient
	}
	// Collision is cryptographically implausible, but handle it rather
	// than silently handing two producers the same URL.
	if _, exists := s.sessions[sid]; exists {
		s.mu.Unlock()
		return nil, errors.New("identifier collision, retry")
	}
	s.mu.Unlock()

	sess, err := newSession(sid, tok, src, label, client, sessionConfig{
		bufferBytes: s.opts.BufferBytes,
		quotaBytes:  s.opts.QuotaBytes,
		spillDir:    s.opts.SpillDir,
		metrics:     s.metrics,
	}, time.Now())
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.sessions[sid] = sess
	if client != "" {
		s.perIP[client]++
	}
	s.createdTotal++
	s.mu.Unlock()
	return sess, nil
}

// Get returns a session by ID.
func (s *Store) Get(sid string) (*Session, error) {
	if !id.Valid(sid) {
		return nil, ErrNotFound
	}
	s.mu.RLock()
	sess, ok := s.sessions[sid]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	return sess, nil
}

// Finish marks a producer done and releases its per-client slot.
func (s *Store) Finish(sess *Session) {
	if sess == nil {
		return
	}
	sess.Close()
	s.mu.Lock()
	if sess.Client != "" {
		if n := s.perIP[sess.Client]; n > 1 {
			s.perIP[sess.Client] = n - 1
		} else {
			delete(s.perIP, sess.Client)
		}
	}
	s.mu.Unlock()
}

// Delete removes a session and its spill file immediately.
func (s *Store) Delete(sid string) error {
	s.mu.Lock()
	sess, ok := s.sessions[sid]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	delete(s.sessions, sid)
	if sess.Client != "" {
		if n := s.perIP[sess.Client]; n > 1 {
			s.perIP[sess.Client] = n - 1
		} else {
			delete(s.perIP, sess.Client)
		}
	}
	s.mu.Unlock()

	sess.Close()
	if p := sess.SpillPath(); p != "" {
		_ = os.Remove(p)
	}
	return nil
}

// List returns stats for all retained sessions, newest first.
func (s *Store) List() []Stat {
	s.mu.RLock()
	out := make([]Stat, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess.Stat())
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Counters is a snapshot of store-level metrics.
type Counters struct {
	Live           int
	Created        uint64
	Reaped         uint64
	Rejected       uint64
	RejectedGlobal uint64
	RejectedClient uint64
	BytesIn        int64
	ViewersLive    int64
	ViewersTotal   int64
	ViewersDropped int64
}

// Counters returns store metrics.
func (s *Store) Counters() Counters {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Counters{
		Live:           len(s.sessions),
		Created:        s.createdTotal,
		Reaped:         s.reapedTotal,
		Rejected:       s.rejectedinc,
		RejectedGlobal: s.rejectedGlobal,
		RejectedClient: s.rejectedClient,
		BytesIn:        s.metrics.BytesIn.Load(),
		ViewersLive:    s.metrics.ViewersLive.Load(),
		ViewersTotal:   s.metrics.ViewersTotal.Load(),
		ViewersDropped: s.metrics.ViewersDropped.Load(),
	}
}

// Close stops the reaper and releases every session.
func (s *Store) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.mu.Lock()
	for _, sess := range s.sessions {
		sess.Close()
	}
	s.mu.Unlock()
}

func (s *Store) reapLoop() {
	interval := s.opts.ClosedTTL / 4
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.reap(time.Now())
		}
	}
}

// reap removes expired sessions. Split out for testing with a fixed clock.
func (s *Store) reap(now time.Time) int {
	var expired []*Session

	s.mu.Lock()
	for sid, sess := range s.sessions {
		st := sess.Stat()
		dead := now.Sub(st.CreatedAt) > s.opts.MaxTTL ||
			now.Sub(st.LastWrite) > s.opts.IdleTTL ||
			(st.Closed && now.Sub(st.ClosedAt) > s.opts.ClosedTTL)
		if !dead {
			continue
		}
		delete(s.sessions, sid)
		if sess.Client != "" {
			if n := s.perIP[sess.Client]; n > 1 {
				s.perIP[sess.Client] = n - 1
			} else {
				delete(s.perIP, sess.Client)
			}
		}
		s.reapedTotal++
		expired = append(expired, sess)
	}
	s.mu.Unlock()

	for _, sess := range expired {
		sess.Close()
		if p := sess.SpillPath(); p != "" {
			_ = os.Remove(p)
		}
	}
	return len(expired)
}
