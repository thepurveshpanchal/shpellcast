// Package id generates unguessable, URL-safe session identifiers.
//
// Security model: shpellcast has no authentication. A session URL is a
// bearer capability, so the identifier must be impossible to guess or
// enumerate. We draw from crypto/rand and encode with a lowercase
// base32 alphabet (no padding, no ambiguous characters) so IDs survive
// copy/paste out of a terminal, double-click selection and shell history.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

// alphabet is Crockford-ish base32: digits + lowercase letters minus
// i, l, o, u (visually ambiguous or prone to forming words).
const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// New returns a random identifier of n characters.
// n=16 yields 16*5 = 80 bits of entropy, which makes online enumeration
// of live sessions infeasible even with no rate limiting at all.
func New(n int) (string, error) {
	if n <= 0 {
		n = 16
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("id: read entropy: %w", err)
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// MustNew is New but panics on entropy failure. A machine that cannot
// produce randomness cannot safely serve unauthenticated capability URLs,
// so failing loudly at startup is correct. Only call during init paths.
func MustNew(n int) string {
	s, err := New(n)
	if err != nil {
		panic(err)
	}
	return s
}

// Valid reports whether s looks like an identifier this package produced.
// Used to reject path traversal and junk before touching the store.
func Valid(s string) bool {
	if len(s) < 6 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z')
		if !ok {
			return false
		}
	}
	return true
}

// Token returns a secondary secret used for privileged operations on a
// session (closing or deleting it) without any user accounts.
func Token() (string, error) { return New(24) }

// Fingerprint returns a short, stable, non-reversible label for a value
// such as a client IP, for logs that should not carry raw addresses.
func Fingerprint(seed uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], seed*0x9E3779B97F4A7C15)
	out := make([]byte, 6)
	for i := range out {
		out[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(out)
}
