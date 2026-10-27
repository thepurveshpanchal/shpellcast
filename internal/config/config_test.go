package config

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	c, err := Parse(nil, io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.HTTPAddr != ":8080" || c.TCPAddr != ":1337" {
		t.Errorf("default addrs = %q %q", c.HTTPAddr, c.TCPAddr)
	}
	if c.BufferBytes != 1<<18 {
		t.Errorf("default buffer = %d", c.BufferBytes)
	}
	if c.IndexEnabled {
		t.Error("session listing must default to off")
	}
	if c.TrustProxy {
		t.Error("proxy headers must not be trusted by default")
	}
}

func TestParseFlags(t *testing.T) {
	c, err := Parse([]string{
		"-http", "127.0.0.1:9000",
		"-tcp", "",
		"-buffer", "4MB",
		"-quota", "0",
		"-base-url", "https://cast.example.com/",
		"-idle-ttl", "90m",
		"-trust-proxy",
	}, io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.HTTPAddr != "127.0.0.1:9000" || c.TCPAddr != "" {
		t.Errorf("addrs = %q %q", c.HTTPAddr, c.TCPAddr)
	}
	if c.BufferBytes != 4<<20 {
		t.Errorf("buffer = %d, want %d", c.BufferBytes, 4<<20)
	}
	if c.QuotaBytes != 0 {
		t.Errorf("quota = %d, want 0 (unlimited)", c.QuotaBytes)
	}
	if c.BaseURL != "https://cast.example.com" {
		t.Errorf("base URL = %q (trailing slash should be trimmed)", c.BaseURL)
	}
	if c.IdleTTL != 90*time.Minute {
		t.Errorf("idle TTL = %v", c.IdleTTL)
	}
	if !c.TrustProxy {
		t.Error("-trust-proxy not applied")
	}
}

func TestEnvironmentIsHonoured(t *testing.T) {
	t.Setenv("SHPELLCAST_HTTP", "0.0.0.0:7000")
	t.Setenv("SHPELLCAST_BUFFER", "1MB")
	t.Setenv("SHPELLCAST_INDEX", "true")

	c, err := Parse(nil, io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.HTTPAddr != "0.0.0.0:7000" || c.BufferBytes != 1<<20 || !c.IndexEnabled {
		t.Errorf("env not applied: %+v", c)
	}

	// A flag must still win over the environment.
	c, err = Parse([]string{"-http", "127.0.0.1:1"}, io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.HTTPAddr != "127.0.0.1:1" {
		t.Errorf("flag did not override env: %q", c.HTTPAddr)
	}
}

func TestParseBytes(t *testing.T) {
	cases := map[string]int64{
		"512": 512, "1KB": 1 << 10, "4MB": 4 << 20,
		"2GB": 2 << 30, "256kb": 256 << 10, " 8MiB ": 8 << 20, "0": 0,
	}
	for in, want := range cases {
		got, err := parseBytes(in)
		if err != nil {
			t.Errorf("parseBytes(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseBytes(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "lots", "-5", "12x"} {
		if _, err := parseBytes(bad); err == nil {
			t.Errorf("parseBytes(%q) accepted an invalid size", bad)
		}
	}
}

func TestValidateRejectsUnsafeConfig(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"short ids", []string{"-id-length", "4"}, "id-length"},
		{"quota below buffer", []string{"-buffer", "4MB", "-quota", "1MB"}, "quota"},
		{"tiny buffer", []string{"-buffer", "16"}, "buffer"},
		{"bad base url", []string{"-base-url", "cast.example.com"}, "base-url"},
		{"bad log level", []string{"-log-level", "verbose"}, "log-level"},
		{"no http addr", []string{"-http", ""}, "http"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.args, io.Discard)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512: "512 B", 1 << 10: "1.0 KB", 1536: "1.5 KB",
		1 << 20: "1.0 MB", 1 << 30: "1.0 GB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// Regression: the first release shipped -max-per-client=50, which refused
// half of a 100-session load test because every producer behind one NAT
// or reverse proxy shares a source address. The default must comfortably
// clear the "a few hundred concurrent builds from one CI host" case.
func TestPerClientDefaultDoesNotCapRealisticFleets(t *testing.T) {
	c, err := Parse(nil, io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.MaxPerClient != 0 && c.MaxPerClient < 100 {
		t.Errorf("-max-per-client default = %d; a single CI host or proxy "+
			"would be refused past that many concurrent sessions", c.MaxPerClient)
	}
	if c.MaxSessions != 0 && c.MaxSessions < c.MaxPerClient {
		t.Errorf("per-client cap (%d) exceeds the server cap (%d), so it can never bind",
			c.MaxPerClient, c.MaxSessions)
	}
}
