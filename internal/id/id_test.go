package id

import "testing"

func TestNewLengthAndAlphabet(t *testing.T) {
	for _, n := range []int{10, 16, 32} {
		s, err := New(n)
		if err != nil {
			t.Fatalf("New(%d): %v", n, err)
		}
		if len(s) != n {
			t.Errorf("len = %d, want %d", len(s), n)
		}
		if !Valid(s) {
			t.Errorf("New produced an id that Valid rejects: %q", s)
		}
		for _, c := range s {
			if c == 'i' || c == 'l' || c == 'o' || c == 'u' {
				t.Errorf("id %q contains an ambiguous character %q", s, c)
			}
		}
	}
}

func TestNewIsNotRepetitive(t *testing.T) {
	seen := make(map[string]bool, 10000)
	for i := 0; i < 10000; i++ {
		s, err := New(16)
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Fatalf("duplicate id after %d draws: %q", i, s)
		}
		seen[s] = true
	}
}

func TestValidRejectsHostileInput(t *testing.T) {
	bad := []string{
		"", "short", "../../etc/passwd", "ABCDEFGHIJ", "with space",
		"semi;colon;", "null\x00byte", "percent%2e%2e",
	}
	for _, s := range bad {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}

func TestTokenIsLongerThanID(t *testing.T) {
	tok, err := Token()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 24 {
		t.Errorf("token length = %d, want at least 24", len(tok))
	}
}
