package session

import (
	"bytes"
	"strings"
	"testing"
)

func TestRingKeepsEverythingBelowCapacity(t *testing.T) {
	r := NewRing(1024)
	r.Write([]byte("hello "))
	r.Write([]byte("world"))

	if got := string(r.Snapshot()); got != "hello world" {
		t.Fatalf("snapshot = %q, want %q", got, "hello world")
	}
	if r.Truncated() {
		t.Error("Truncated() = true for a buffer that never filled")
	}
	if r.Written() != 11 {
		t.Errorf("Written() = %d, want 11", r.Written())
	}
}

func TestRingDiscardsOldestOnWrap(t *testing.T) {
	r := NewRing(1024) // minimum enforced capacity
	payload := strings.Repeat("a", 1000) + strings.Repeat("b", 500)
	r.Write([]byte(payload))

	snap := r.Snapshot()
	if len(snap) != 1024 {
		t.Fatalf("len(snapshot) = %d, want 1024", len(snap))
	}
	if want := payload[len(payload)-1024:]; string(snap) != want {
		t.Errorf("snapshot is not the tail of the stream")
	}
	if !r.Truncated() {
		t.Error("Truncated() = false after discarding bytes")
	}
	if r.Written() != int64(len(payload)) {
		t.Errorf("Written() = %d, want %d", r.Written(), len(payload))
	}
}

func TestRingManySmallWritesWrapCorrectly(t *testing.T) {
	r := NewRing(1024)
	var all bytes.Buffer
	for i := 0; i < 5000; i++ {
		chunk := []byte{byte('0' + i%10)}
		r.Write(chunk)
		all.Write(chunk)
	}
	want := all.Bytes()[all.Len()-1024:]
	if !bytes.Equal(r.Snapshot(), want) {
		t.Error("ring content diverged from the stream tail after many wraps")
	}
}

func TestRingWriteLargerThanCapacityKeepsTail(t *testing.T) {
	r := NewRing(1024)
	r.Write([]byte("seed"))
	big := bytes.Repeat([]byte("xy"), 2000) // 4000 bytes
	r.Write(big)

	snap := r.Snapshot()
	if len(snap) != 1024 {
		t.Fatalf("len = %d, want 1024", len(snap))
	}
	if !bytes.Equal(snap, big[len(big)-1024:]) {
		t.Error("oversized write did not leave its own tail in the ring")
	}
}

func TestRingEmptySnapshot(t *testing.T) {
	r := NewRing(2048)
	if len(r.Snapshot()) != 0 {
		t.Error("fresh ring should snapshot to nothing")
	}
}

func TestRingGrowsOnDemandAndStopsAtMax(t *testing.T) {
	r := NewRing(64 << 10)

	// A fresh ring must not pay for its whole allowance up front.
	if r.Cap() > ringInitial {
		t.Fatalf("fresh ring allocated %d bytes, want at most %d", r.Cap(), ringInitial)
	}
	if r.Max() != 64<<10 {
		t.Errorf("Max() = %d", r.Max())
	}

	r.Write(bytes.Repeat([]byte("a"), 100))
	if r.Cap() != ringInitial {
		t.Errorf("small write forced a grow to %d", r.Cap())
	}

	// Cross the initial capacity: it should double, not jump to the max.
	r.Write(bytes.Repeat([]byte("b"), 5000))
	if r.Cap() <= ringInitial || r.Cap() > 16<<10 {
		t.Errorf("Cap() = %d after 5.1KB, want a modest doubling", r.Cap())
	}
	if r.Truncated() {
		t.Error("grew but also reported data loss")
	}

	// Fill past the limit: capacity stops at max and data starts rolling.
	r.Write(bytes.Repeat([]byte("c"), 200<<10))
	if r.Cap() != 64<<10 {
		t.Errorf("Cap() = %d, want it pinned at the 64KB max", r.Cap())
	}
	if r.Len() != 64<<10 {
		t.Errorf("Len() = %d, want a full 64KB", r.Len())
	}
	if !r.Truncated() {
		t.Error("Truncated() = false after exceeding the limit")
	}
}

func TestRingGrowthPreservesContentAcrossWrap(t *testing.T) {
	// Force a wrap first, then a grow, which is the case where the
	// unwrap-on-grow logic would corrupt the stream if it were wrong.
	r := NewRing(64 << 10)
	var all bytes.Buffer

	for i := 0; i < 300; i++ {
		chunk := bytes.Repeat([]byte{byte('A' + i%26)}, 37)
		r.Write(chunk)
		all.Write(chunk)
	}
	want := all.Bytes()
	if len(want) > r.Len() {
		want = want[len(want)-r.Len():]
	}
	if !bytes.Equal(r.Snapshot(), want) {
		t.Fatal("content diverged from the stream tail after growth")
	}
}

func TestRingGrowthMatchesFixedBufferBehaviour(t *testing.T) {
	// Property check: a grown ring must be byte-identical to one that was
	// allocated at full size from the start, for the same input.
	for _, max := range []int{1024, 4096, 9000, 64 << 10} {
		r := NewRing(max)
		var all bytes.Buffer
		for i := 0; i < 2000; i++ {
			chunk := []byte(strings.Repeat(string(rune('a'+i%26)), 1+i%53))
			r.Write(chunk)
			all.Write(chunk)
		}
		want := all.Bytes()
		if len(want) > max {
			want = want[len(want)-max:]
		}
		if !bytes.Equal(r.Snapshot(), want) {
			t.Errorf("max=%d: snapshot is not the tail of the stream", max)
		}
		if r.Cap() > max {
			t.Errorf("max=%d: capacity grew past the limit to %d", max, r.Cap())
		}
	}
}
