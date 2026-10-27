// Package config parses shpellcast's runtime configuration from flags and
// environment variables. Every flag -foo-bar has an environment twin
// SHPELLCAST_FOO_BAR, so the same binary configures identically under
// systemd (EnvironmentFile), Docker (-e) and a shell.
//
// Precedence: command-line flag > environment variable > default.
package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	HTTPAddr string // listen address for the web/API server
	TCPAddr  string // listen address for raw netcat ingest ("" disables)
	BaseURL  string // public origin used to build session URLs

	BufferBytes  int
	QuotaBytes   int64
	MaxSessions  int
	MaxPerClient int
	IDLength     int

	IdleTTL   time.Duration
	ClosedTTL time.Duration
	MaxTTL    time.Duration

	SpillDir string

	TrustProxy   bool
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	TCPIdle      time.Duration

	IndexEnabled bool // expose the list of live sessions on /
	LogLevel     string
	Version      bool
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		HTTPAddr:    ":8080",
		TCPAddr:     ":1337",
		BaseURL:     "",
		BufferBytes: 1 << 18, // 256 KiB retained per session
		QuotaBytes:  64 << 20,
		MaxSessions: 2000,
		// Deliberately generous. This limit exists to stop one host
		// exhausting the server, not to ration normal use: a CI fleet
		// behind NAT, a build farm, or anything behind a reverse proxy
		// without -trust-proxy all arrive as a single source address, so
		// a low value here silently refuses legitimate sessions. 0
		// disables it entirely.
		MaxPerClient: 250,
		IDLength:     16,
		IdleTTL:      24 * time.Hour,
		ClosedTTL:    6 * time.Hour,
		MaxTTL:       7 * 24 * time.Hour,
		SpillDir:     "",
		TrustProxy:   false,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // streaming responses must not be cut off
		TCPIdle:      30 * time.Minute,
		IndexEnabled: false,
		LogLevel:     "info",
	}
}

// Parse resolves configuration from args (without the program name) and
// the process environment.
func Parse(args []string, out io.Writer) (Config, error) {
	c := Default()

	fs := flag.NewFlagSet("shpellcast", flag.ContinueOnError)
	fs.SetOutput(out)

	fs.StringVar(&c.HTTPAddr, "http", env("HTTP", c.HTTPAddr), "HTTP listen address for the viewer and API")
	fs.StringVar(&c.TCPAddr, "tcp", env("TCP", c.TCPAddr), "raw TCP listen address for `nc` ingest (empty disables)")
	fs.StringVar(&c.BaseURL, "base-url", env("BASE_URL", c.BaseURL), "public base URL used in generated links, e.g. https://shpellcast.godtech.in (empty = derive from request)")

	bufStr := fs.String("buffer", env("BUFFER", humanBytes(int64(c.BufferBytes))), "replay buffer retained per session (e.g. 256KB, 4MB)")
	quotaStr := fs.String("quota", env("QUOTA", humanBytes(c.QuotaBytes)), "maximum output accepted per session (0 = unlimited)")

	fs.IntVar(&c.MaxSessions, "max-sessions", envInt("MAX_SESSIONS", c.MaxSessions), "maximum retained sessions (0 = unlimited)")
	fs.IntVar(&c.MaxPerClient, "max-per-client", envInt("MAX_PER_CLIENT", c.MaxPerClient), "maximum live sessions per client IP (0 = unlimited)")
	fs.IntVar(&c.IDLength, "id-length", envInt("ID_LENGTH", c.IDLength), "characters in a session ID (5 bits of entropy each)")

	fs.DurationVar(&c.IdleTTL, "idle-ttl", envDur("IDLE_TTL", c.IdleTTL), "expire a session this long after its last write")
	fs.DurationVar(&c.ClosedTTL, "closed-ttl", envDur("CLOSED_TTL", c.ClosedTTL), "expire a session this long after the producer disconnects")
	fs.DurationVar(&c.MaxTTL, "max-ttl", envDur("MAX_TTL", c.MaxTTL), "hard expiry measured from session creation")
	fs.DurationVar(&c.TCPIdle, "tcp-idle", envDur("TCP_IDLE", c.TCPIdle), "drop a silent producer TCP connection after this long")
	fs.DurationVar(&c.ReadTimeout, "read-timeout", envDur("READ_TIMEOUT", c.ReadTimeout), "HTTP header read timeout")

	fs.StringVar(&c.SpillDir, "spill-dir", env("SPILL_DIR", c.SpillDir), "mirror sessions to this directory so output survives restarts (empty = memory only)")
	fs.BoolVar(&c.TrustProxy, "trust-proxy", envBool("TRUST_PROXY", c.TrustProxy), "honour X-Forwarded-For / X-Forwarded-Proto (only behind your own reverse proxy)")
	fs.BoolVar(&c.IndexEnabled, "index", envBool("INDEX", c.IndexEnabled), "list live sessions on the home page (leaks session URLs - keep off on public servers)")
	fs.StringVar(&c.LogLevel, "log-level", env("LOG_LEVEL", c.LogLevel), "debug, info, warn or error")
	fs.BoolVar(&c.Version, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return c, err
	}

	var err error
	if c.BufferBytes, err = parseBytesInt(*bufStr); err != nil {
		return c, fmt.Errorf("-buffer: %w", err)
	}
	if c.QuotaBytes, err = parseBytes(*quotaStr); err != nil {
		return c, fmt.Errorf("-quota: %w", err)
	}

	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c, c.Validate()
}

// Validate rejects configurations that would be unsafe or useless.
func (c Config) Validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("-http listen address is required")
	}
	if c.BufferBytes < 1024 {
		return fmt.Errorf("-buffer must be at least 1KB")
	}
	if c.QuotaBytes != 0 && c.QuotaBytes < int64(c.BufferBytes) {
		return fmt.Errorf("-quota (%d) must be 0 or at least -buffer (%d)", c.QuotaBytes, c.BufferBytes)
	}
	if c.IDLength < 10 {
		// Session URLs are unauthenticated capabilities; below ~50 bits
		// of entropy they become guessable by a determined scanner.
		return fmt.Errorf("-id-length must be at least 10 for unguessable URLs")
	}
	if c.BaseURL != "" && !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return fmt.Errorf("-base-url must start with http:// or https://")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("-log-level must be debug, info, warn or error")
	}
	return nil
}

// ---- environment helpers ----

func env(key, def string) string {
	if v, ok := os.LookupEnv("SHPELLCAST_" + key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv("SHPELLCAST_" + key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv("SHPELLCAST_" + key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv("SHPELLCAST_" + key); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}

// ---- size parsing ----

func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "KB"), strings.HasSuffix(s, "KIB"):
		mult, s = 1<<10, strings.TrimSuffix(strings.TrimSuffix(s, "KIB"), "KB")
	case strings.HasSuffix(s, "MB"), strings.HasSuffix(s, "MIB"):
		mult, s = 1<<20, strings.TrimSuffix(strings.TrimSuffix(s, "MIB"), "MB")
	case strings.HasSuffix(s, "GB"), strings.HasSuffix(s, "GIB"):
		mult, s = 1<<30, strings.TrimSuffix(strings.TrimSuffix(s, "GIB"), "GB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("size must not be negative")
	}
	return n * mult, nil
}

func parseBytesInt(s string) (int, error) {
	n, err := parseBytes(s)
	if err != nil {
		return 0, err
	}
	if n > int64(^uint(0)>>1) {
		return 0, fmt.Errorf("size too large")
	}
	return int(n), nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKB", n>>10)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// HumanBytes formats a byte count for logs and the UI.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
