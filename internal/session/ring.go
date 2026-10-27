package session

// Ring is a byte ring buffer that retains the most recent Max() bytes and
// discards the oldest. This bounds memory per session to a known constant
// regardless of how much a producer streams, which is the property that
// lets the server accept untrusted, unauthenticated, potentially infinite
// output safely.
//
// The backing array starts small and doubles on demand up to the limit,
// because most sessions never come close to filling it: a job that prints
// 3 KB should cost 4 KB of heap, not the 256 KB it was permitted. With a
// few hundred concurrent sessions that is the difference between tens of
// megabytes and hundreds. Growth is amortised O(1) per byte and happens
// at most log2(max/initial) times per session — eight reallocations to
// reach 1 MB, and never again.
//
// Ring is not safe for concurrent use; the owning Session holds the lock.
type Ring struct {
	buf      []byte
	max      int   // retention limit; buf never grows past this
	start    int   // index of oldest byte when wrapped
	size     int   // bytes currently held (<= len(buf))
	written  int64 // total bytes ever written
	overflow bool  // true once we have discarded at least one byte
}

// ringInitial is the first allocation. Small enough that an idle or
// short-lived session is cheap, large enough that a typical line of log
// output does not trigger a reallocation on arrival.
const ringInitial = 4 << 10

// NewRing returns a ring that retains at most the last max bytes.
func NewRing(max int) *Ring {
	if max < 1024 {
		max = 1024
	}
	initial := ringInitial
	if initial > max {
		initial = max
	}
	return &Ring{buf: make([]byte, initial), max: max}
}

// Max returns the retention limit in bytes.
func (r *Ring) Max() int { return r.max }

// Cap returns the currently allocated capacity, which grows towards Max
// as output arrives.
func (r *Ring) Cap() int { return len(r.buf) }

// Len returns the number of bytes currently retained.
func (r *Ring) Len() int { return r.size }

// Written returns the total number of bytes ever written, including
// bytes that have since been discarded.
func (r *Ring) Written() int64 { return r.written }

// Truncated reports whether any data has been discarded, i.e. whether a
// viewer loading the replay is seeing a partial stream.
func (r *Ring) Truncated() bool { return r.overflow }

// grow reallocates so the buffer holds at least need bytes, capped at
// max, and re-lays the retained bytes out linearly from index 0.
func (r *Ring) grow(need int) {
	if need > r.max {
		need = r.max
	}
	newCap := len(r.buf)
	for newCap < need {
		newCap *= 2
	}
	if newCap > r.max {
		newCap = r.max
	}
	if newCap == len(r.buf) {
		return
	}
	next := make([]byte, newCap)
	// Snapshot order, not storage order: unwrap while copying so the
	// buffer is linear again and start can reset to zero.
	c := len(r.buf)
	n := copy(next, r.buf[r.start:min(r.start+r.size, c)])
	if n < r.size {
		copy(next[n:], r.buf[:r.size-n])
	}
	r.buf = next
	r.start = 0
}

// Write appends p, discarding the oldest bytes once the retention limit
// is reached. It never fails.
func (r *Ring) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	r.written += int64(n)

	if r.size+n > len(r.buf) && len(r.buf) < r.max {
		r.grow(r.size + n)
	}

	c := len(r.buf)

	// A write at least as large as the whole buffer: keep only its tail.
	if n >= c {
		copy(r.buf, p[n-c:])
		r.start = 0
		r.size = c
		r.overflow = true
		return n, nil
	}

	// Write position is start+size modulo capacity.
	pos := (r.start + r.size) % c
	first := copy(r.buf[pos:], p)
	if first < n {
		copy(r.buf, p[first:])
	}

	if r.size+n <= c {
		r.size += n
	} else {
		// We wrapped over retained data; advance start past what we clobbered.
		r.overflow = true
		r.start = (r.start + (r.size + n - c)) % c
		r.size = c
	}
	return n, nil
}

// Snapshot returns a copy of the retained bytes in write order.
// The caller owns the returned slice.
func (r *Ring) Snapshot() []byte {
	out := make([]byte, r.size)
	if r.size == 0 {
		return out
	}
	c := len(r.buf)
	n := copy(out, r.buf[r.start:min(r.start+r.size, c)])
	if n < r.size {
		copy(out[n:], r.buf[:r.size-n])
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
