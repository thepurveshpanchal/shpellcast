package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/purvesh/shpellcast/internal/config"
)

// newTestServer starts a real server on ephemeral ports. These are
// end-to-end tests on purpose: the interesting behaviour of this program
// lives in the seams between a socket, a ring buffer and an SSE stream,
// not inside any one function.
func newTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()

	cfg := config.Default()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.TCPAddr = "127.0.0.1:0"
	cfg.BufferBytes = 64 << 10
	cfg.QuotaBytes = 1 << 20
	cfg.LogLevel = "error"

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Run(ctx)
	}()

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down")
		}
	})

	httpAddr, tcpAddr := srv.Addrs()
	return srv, "http://" + httpAddr, tcpAddr
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ---------------------------------------------------------------- TCP path

func TestTCPIngestEndToEnd(t *testing.T) {
	_, base, tcpAddr := newTestServer(t)

	conn, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	banner, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read banner: %v", err)
	}
	if !strings.HasPrefix(banner, "shpellcast: http") {
		t.Fatalf("banner = %q, want a URL line", banner)
	}
	url := strings.TrimSpace(strings.TrimPrefix(banner, "shpellcast: "))
	id := url[strings.LastIndex(url, "/")+1:]

	if _, err := io.WriteString(conn, "building...\nstep 1 done\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Give the server a moment to drain the socket.
	waitFor(t, func() bool {
		_, body := httpGet(t, base+"/s/"+id+"/raw?follow=0")
		return strings.Contains(body, "step 1 done")
	}, "output to appear on the raw endpoint")

	conn.Close()

	waitFor(t, func() bool {
		_, body := httpGet(t, base+"/s/"+id+"/meta")
		var st map[string]any
		_ = json.Unmarshal([]byte(body), &st)
		closed, _ := st["closed"].(bool)
		return closed
	}, "session to be marked closed after the producer disconnects")
}

// ---------------------------------------------------------------- HTTP path

func TestHTTPIngestReturnsURLAndServesOutput(t *testing.T) {
	_, base, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPut, base+"/i", strings.NewReader("hello from curl\n"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /i: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	url := strings.TrimSpace(string(body))
	if !strings.HasPrefix(url, base+"/s/") {
		t.Fatalf("response body = %q, want a session URL", body)
	}
	if h := resp.Header.Get("X-Shpellcast-Url"); h != url {
		t.Errorf("X-Shpellcast-Url = %q, want %q", h, url)
	}

	id := url[strings.LastIndex(url, "/")+1:]
	status, out := httpGet(t, base+"/s/"+id+"/raw?follow=0")
	if status != 200 || out != "hello from curl\n" {
		t.Errorf("raw = %d %q", status, out)
	}

	status, out = httpGet(t, base+"/s/"+id+"/download")
	if status != 200 || out != "hello from curl\n" {
		t.Errorf("download = %d %q", status, out)
	}
}

func TestNewThenTokenedIngest(t *testing.T) {
	_, base, _ := newTestServer(t)

	status, body := httpGet(t, base+"/new")
	if status != 200 {
		t.Fatalf("GET /new = %d", status)
	}
	var created struct {
		ID, URL, Token string
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode /new: %v (%s)", err, body)
	}
	if created.ID == "" || created.Token == "" {
		t.Fatalf("/new returned %+v", created)
	}

	// Without the token, a third party who knows the URL must not be able
	// to inject output into someone else's session.
	req, _ := http.NewRequest(http.MethodPut, base+"/i/"+created.ID, strings.NewReader("spoofed"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT without token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("PUT without token = %d, want 403", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPut, base+"/i/"+created.ID, strings.NewReader("real output\n"))
	req.Header.Set("X-Shpellcast-Token", created.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT with token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("PUT with token = %d, want 200", resp.StatusCode)
	}

	_, out := httpGet(t, base+"/s/"+created.ID+"/raw?follow=0")
	if out != "real output\n" {
		t.Errorf("raw = %q", out)
	}
}

func TestDeleteRequiresToken(t *testing.T) {
	_, base, _ := newTestServer(t)

	_, body := httpGet(t, base+"/new")
	var created struct{ ID, Token string }
	_ = json.Unmarshal([]byte(body), &created)

	req, _ := http.NewRequest(http.MethodDelete, base+"/s/"+created.ID, nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("DELETE without token = %d, want 403", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodDelete, base+"/s/"+created.ID+"?token="+created.Token, nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("DELETE with token = %d, want 200", resp.StatusCode)
	}
	if status, _ := httpGet(t, base+"/s/"+created.ID+"/meta"); status != http.StatusNotFound {
		t.Errorf("meta after delete = %d, want 404", status)
	}
}

// ---------------------------------------------------------------- SSE

func TestSSEReplaysThenStreamsLive(t *testing.T) {
	_, base, tcpAddr := newTestServer(t)

	conn, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	banner, _ := br.ReadString('\n')
	url := strings.TrimSpace(strings.TrimPrefix(banner, "shpellcast: "))
	id := url[strings.LastIndex(url, "/")+1:]

	io.WriteString(conn, "first\n")
	waitFor(t, func() bool {
		_, b := httpGet(t, base+"/s/"+id+"/raw?follow=0")
		return strings.Contains(b, "first")
	}, "first chunk to land")

	resp, err := http.Get(base + "/s/" + id + "/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	events := make(chan [2]string, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				events <- [2]string{ev, strings.TrimPrefix(line, "data: ")}
			}
		}
		close(events)
	}()

	// meta first.
	e := nextEvent(t, events)
	if e[0] != "meta" {
		t.Fatalf("first event = %q, want meta", e[0])
	}
	// then the replay of what already arrived.
	e = nextEvent(t, events)
	if e[0] != "out" {
		t.Fatalf("second event = %q, want out", e[0])
	}
	if dec, _ := base64.StdEncoding.DecodeString(e[1]); string(dec) != "first\n" {
		t.Fatalf("replay payload = %q", dec)
	}

	// and now live output.
	io.WriteString(conn, "second\n")
	e = nextEvent(t, events)
	if e[0] != "out" {
		t.Fatalf("live event = %q, want out", e[0])
	}
	if dec, _ := base64.StdEncoding.DecodeString(e[1]); string(dec) != "second\n" {
		t.Fatalf("live payload = %q", dec)
	}

	// closing the producer ends the stream cleanly.
	conn.Close()
	for {
		e = nextEvent(t, events)
		if e[0] == "end" {
			return
		}
	}
}

func TestSSECarriesBinarySafely(t *testing.T) {
	_, base, tcpAddr := newTestServer(t)

	conn, _ := net.Dial("tcp", tcpAddr)
	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	banner, _ := br.ReadString('\n')
	id := strings.TrimSpace(banner)
	id = id[strings.LastIndex(id, "/")+1:]

	// Bytes that would wreck a line-oriented protocol if sent verbatim.
	payload := []byte("\x00\x1b[31mred\x1b[0m\r\n\ndata: fake\nevent: fake\n\xff\xfe")
	conn.Write(payload)
	conn.Close()

	waitFor(t, func() bool {
		_, b := httpGet(t, base+"/s/"+id+"/raw?follow=0")
		return len(b) == len(payload)
	}, "binary payload to land")

	resp, err := http.Get(base + "/s/" + id + "/stream")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	var ev string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			ev = strings.TrimPrefix(line, "event: ")
			continue
		}
		if ev == "out" && strings.HasPrefix(line, "data: ") {
			dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "data: "))
			if err != nil {
				t.Fatalf("payload was not valid base64: %v", err)
			}
			if string(dec) != string(payload) {
				t.Fatalf("round-tripped payload differs:\n got %q\nwant %q", dec, payload)
			}
			return
		}
	}
	t.Fatal("no output event received")
}

// ---------------------------------------------------------------- misc

func TestStaticAndPages(t *testing.T) {
	_, base, _ := newTestServer(t)

	if status, body := httpGet(t, base+"/"); status != 200 || !strings.Contains(body, "shpellcast") {
		t.Errorf("index = %d", status)
	}
	if status, body := httpGet(t, base+"/static/app.js"); status != 200 || !strings.Contains(body, "EventSource") {
		t.Errorf("app.js = %d", status)
	}
	if status, body := httpGet(t, base+"/client.sh"); status != 200 || !strings.Contains(body, "shpellcast") {
		t.Errorf("client.sh = %d", status)
	}
	if status, _ := httpGet(t, base+"/healthz"); status != 200 {
		t.Error("healthz not ok")
	}
	if status, body := httpGet(t, base+"/metrics"); status != 200 || !strings.Contains(body, "shpellcast_sessions_live") {
		t.Error("metrics missing")
	}
	// Session listing is a capability leak and must stay off by default.
	if status, _ := httpGet(t, base+"/sessions"); status != http.StatusForbidden {
		t.Error("session listing should be disabled by default")
	}
}

func TestUnknownSessionsAndTraversal(t *testing.T) {
	_, base, _ := newTestServer(t)

	if status, _ := httpGet(t, base+"/s/doesnotexist1234/raw"); status != 404 {
		t.Error("unknown session should 404")
	}
	for _, p := range []string{"/static/..%2f..%2fetc%2fpasswd", "/static/nope.js", "/nonsense"} {
		if status, _ := httpGet(t, base+p); status != 404 {
			t.Errorf("GET %s = %d, want 404", p, status)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, base, _ := newTestServer(t)
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestLabelSanitisation(t *testing.T) {
	cases := map[string]string{
		"nightly build":             "nightly build",
		"<script>alert(1)</script>": "script alert(1) /script",
		"with\x00control\x07chars":  "withcontrolchars",
		strings.Repeat("x", 200):    strings.Repeat("x", 80),
	}
	for in, want := range cases {
		if got := sanitiseLabel(in); got != want {
			t.Errorf("sanitiseLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("abc123", "abc123") {
		t.Error("equal strings reported unequal")
	}
	if constantTimeEqual("abc123", "abc124") || constantTimeEqual("", "") || constantTimeEqual("a", "ab") {
		t.Error("unequal or empty strings reported equal")
	}
}

// ---------------------------------------------------------------- helpers

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func nextEvent(t *testing.T, ch chan [2]string) [2]string {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("event stream closed unexpectedly")
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an SSE event")
		return [2]string{}
	}
}
