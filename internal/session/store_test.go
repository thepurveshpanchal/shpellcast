package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T, mutate func(*Options)) *Store {
	t.Helper()
	opts := Options{BufferBytes: 4096}
	if mutate != nil {
		mutate(&opts)
	}
	st, err := NewStore(opts)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestCreateGetDelete(t *testing.T) {
	st := newTestStore(t, nil)

	sess, err := st.Create(SourceTCP, "build", "10.0.0.1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(sess.ID) < 10 {
		t.Errorf("session ID %q is too short to be unguessable", sess.ID)
	}
	if sess.Token == "" {
		t.Error("session has no owner token")
	}

	got, err := st.Get(sess.ID)
	if err != nil || got != sess {
		t.Fatalf("Get returned %v, %v", got, err)
	}
	if err := st.Delete(sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(sess.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestGetRejectsMalformedIDs(t *testing.T) {
	st := newTestStore(t, nil)
	for _, bad := range []string{"", "../../etc/passwd", "ABC", "a", "with space", "semi;colon"} {
		if _, err := st.Get(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", bad, err)
		}
	}
}

func TestIDsAreUnique(t *testing.T) {
	st := newTestStore(t, nil)
	seen := make(map[string]bool, 500)
	for i := 0; i < 500; i++ {
		s, err := st.Create(SourceTCP, "", "")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if seen[s.ID] {
			t.Fatalf("duplicate session ID %q", s.ID)
		}
		seen[s.ID] = true
	}
}

func TestGlobalSessionLimit(t *testing.T) {
	st := newTestStore(t, func(o *Options) { o.MaxSessions = 2 })
	for i := 0; i < 2; i++ {
		if _, err := st.Create(SourceTCP, "", "1.1.1.1"); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if _, err := st.Create(SourceTCP, "", "2.2.2.2"); !errors.Is(err, ErrTooMany) {
		t.Errorf("third Create = %v, want ErrTooMany", err)
	}
}

func TestPerClientLimitAndRelease(t *testing.T) {
	st := newTestStore(t, func(o *Options) { o.MaxPerClient = 1 })

	first, err := st.Create(SourceTCP, "", "9.9.9.9")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.Create(SourceTCP, "", "9.9.9.9"); !errors.Is(err, ErrTooManyClient) {
		t.Fatalf("second Create = %v, want ErrTooManyClient", err)
	}
	// A different client is unaffected.
	if _, err := st.Create(SourceTCP, "", "8.8.8.8"); err != nil {
		t.Fatalf("other client Create: %v", err)
	}
	// Finishing frees the slot even though the session is still readable.
	st.Finish(first)
	if _, err := st.Create(SourceTCP, "", "9.9.9.9"); err != nil {
		t.Fatalf("Create after Finish: %v", err)
	}
}

func TestQuotaStopsWritesButKeepsOutput(t *testing.T) {
	st := newTestStore(t, func(o *Options) {
		o.BufferBytes = 2048
		o.QuotaBytes = 2048
	})
	sess, _ := st.Create(SourceHTTP, "", "")

	payload := bytes.Repeat([]byte("x"), 1024)
	if _, err := sess.Write(payload); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := sess.Write(payload); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("write that reaches the quota = %v, want ErrQuotaExceeded", err)
	}
	if _, err := sess.Write(payload); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("write past the quota = %v, want ErrQuotaExceeded", err)
	}
	if n := len(sess.Snapshot()); n != 2048 {
		t.Errorf("retained %d bytes after quota, want 2048 still readable", n)
	}
	if !sess.Stat().QuotaHit {
		t.Error("Stat().QuotaHit = false after exceeding the quota")
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	st := newTestStore(t, nil)
	sess, _ := st.Create(SourceHTTP, "", "")
	st.Finish(sess)
	if _, err := sess.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Errorf("write after close = %v, want ErrClosed", err)
	}
}

func TestSubscribeReplaysThenStreams(t *testing.T) {
	st := newTestStore(t, nil)
	sess, _ := st.Create(SourceHTTP, "", "")
	sess.Write([]byte("already here\n"))

	replay, ch, cancel := sess.Subscribe(16)
	defer cancel()

	if string(replay) != "already here\n" {
		t.Fatalf("replay = %q", replay)
	}
	sess.Write([]byte("live\n"))

	select {
	case chunk, ok := <-ch:
		if !ok {
			t.Fatal("channel closed instead of delivering live output")
		}
		if string(chunk) != "live\n" {
			t.Errorf("live chunk = %q, want %q", chunk, "live\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for live output")
	}
}

func TestSubscribersAreDroppedNotBlocking(t *testing.T) {
	st := newTestStore(t, func(o *Options) { o.BufferBytes = 1 << 16 })
	sess, _ := st.Create(SourceHTTP, "", "")

	// A viewer that never reads must not slow the producer down.
	_, ch, cancel := sess.Subscribe(8)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			sess.Write([]byte("chunk"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked on a slow viewer")
	}

	// The lagging viewer should have been closed out.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // dropped, as designed
			}
		case <-deadline:
			t.Fatal("slow viewer was never dropped")
		}
	}
}

func TestSubscribeOnClosedSessionReturnsClosedChannel(t *testing.T) {
	st := newTestStore(t, nil)
	sess, _ := st.Create(SourceHTTP, "", "")
	sess.Write([]byte("done"))
	st.Finish(sess)

	replay, ch, cancel := sess.Subscribe(4)
	defer cancel()
	if string(replay) != "done" {
		t.Errorf("replay = %q", replay)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected a closed channel for a finished session")
		}
	case <-time.After(time.Second):
		t.Error("channel for a finished session never closed")
	}
}

func TestConcurrentWritersAndViewers(t *testing.T) {
	st := newTestStore(t, func(o *Options) { o.BufferBytes = 1 << 20 })
	sess, _ := st.Create(SourceHTTP, "", "")

	var wg sync.WaitGroup
	for v := 0; v < 8; v++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ch, cancel := sess.Subscribe(64)
			defer cancel()
			for range ch {
			}
		}()
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				sess.Write([]byte("line of output\n"))
			}
		}(w)
	}
	time.Sleep(100 * time.Millisecond)
	st.Finish(sess)
	wg.Wait()
}

func TestReapExpiresIdleClosedAndAgedSessions(t *testing.T) {
	st := newTestStore(t, func(o *Options) {
		o.IdleTTL = time.Hour
		o.ClosedTTL = 10 * time.Minute
		o.MaxTTL = 24 * time.Hour
	})

	idle, _ := st.Create(SourceTCP, "", "")
	closed, _ := st.Create(SourceTCP, "", "")
	live, _ := st.Create(SourceTCP, "", "")
	st.Finish(closed)

	// Nothing is due yet.
	if n := st.reap(time.Now()); n != 0 {
		t.Fatalf("reaped %d sessions immediately, want 0", n)
	}

	// 20 minutes on: only the finished one is due.
	if n := st.reap(time.Now().Add(20 * time.Minute)); n != 1 {
		t.Fatalf("reaped %d at +20m, want 1", n)
	}
	if _, err := st.Get(closed.ID); !errors.Is(err, ErrNotFound) {
		t.Error("finished session outlived its closed TTL")
	}

	// Two hours on: the idle ones go too.
	st.reap(time.Now().Add(2 * time.Hour))
	if _, err := st.Get(idle.ID); !errors.Is(err, ErrNotFound) {
		t.Error("idle session outlived its idle TTL")
	}
	if _, err := st.Get(live.ID); !errors.Is(err, ErrNotFound) {
		t.Error("second idle session outlived its idle TTL")
	}
}

func TestSpillWritesAndIsRemovedOnDelete(t *testing.T) {
	dir := t.TempDir()
	st := newTestStore(t, func(o *Options) { o.SpillDir = dir })

	sess, err := st.Create(SourceHTTP, "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess.Write([]byte("persisted output\n"))
	st.Finish(sess)

	path := filepath.Join(dir, sess.ID+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file: %v", err)
	}
	if string(data) != "persisted output\n" {
		t.Errorf("spill content = %q", data)
	}

	if err := st.Delete(sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("spill file survived session deletion")
	}
}

func TestCountersTrackLifecycle(t *testing.T) {
	st := newTestStore(t, func(o *Options) { o.MaxSessions = 1 })
	s1, _ := st.Create(SourceTCP, "", "")
	_, _ = st.Create(SourceTCP, "", "") // rejected

	c := st.Counters()
	if c.Created != 1 || c.Rejected != 1 || c.Live != 1 {
		t.Fatalf("counters = %+v", c)
	}
	_ = st.Delete(s1.ID)
	if c := st.Counters(); c.Live != 0 {
		t.Errorf("live = %d after delete, want 0", c.Live)
	}
}

func TestRejectionCountersDistinguishReasons(t *testing.T) {
	// Operators need to tell "the server is full" from "this one client is
	// capped" without reading logs, because the fixes are different.
	global := newTestStore(t, func(o *Options) { o.MaxSessions = 1 })
	_, _ = global.Create(SourceTCP, "", "1.1.1.1")
	if _, err := global.Create(SourceTCP, "", "2.2.2.2"); !errors.Is(err, ErrTooMany) {
		t.Fatalf("want ErrTooMany, got %v", err)
	}
	if c := global.Counters(); c.RejectedGlobal != 1 || c.RejectedClient != 0 {
		t.Errorf("capacity rejection counted as %+v", c)
	}

	perClient := newTestStore(t, func(o *Options) { o.MaxPerClient = 1 })
	_, _ = perClient.Create(SourceTCP, "", "3.3.3.3")
	if _, err := perClient.Create(SourceTCP, "", "3.3.3.3"); !errors.Is(err, ErrTooManyClient) {
		t.Fatalf("want ErrTooManyClient, got %v", err)
	}
	if c := perClient.Counters(); c.RejectedClient != 1 || c.RejectedGlobal != 0 {
		t.Errorf("per-client rejection counted as %+v", c)
	}
}

func TestMetricsTrackViewerLifecycle(t *testing.T) {
	st := newTestStore(t, nil)
	sess, _ := st.Create(SourceHTTP, "", "")

	_, _, cancel1 := sess.Subscribe(16)
	_, _, cancel2 := sess.Subscribe(16)
	if c := st.Counters(); c.ViewersLive != 2 || c.ViewersTotal != 2 {
		t.Fatalf("after two subscribes: %+v", c)
	}
	cancel1()
	if c := st.Counters(); c.ViewersLive != 1 {
		t.Errorf("after one cancel: live = %d, want 1", c.ViewersLive)
	}
	sess.Write([]byte("hello"))
	if c := st.Counters(); c.BytesIn != 5 {
		t.Errorf("BytesIn = %d, want 5", c.BytesIn)
	}
	st.Finish(sess)
	if c := st.Counters(); c.ViewersLive != 0 {
		t.Errorf("after close: live = %d, want 0", c.ViewersLive)
	}
	cancel2()
}
