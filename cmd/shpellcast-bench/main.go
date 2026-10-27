// Command shpellcast-bench drives a shpellcast server with many concurrent
// producers and viewers, and reports what the server actually did.
//
// It exists because "it should handle N sessions" is a guess until
// something measures it. Run it against a staging instance before you
// commit to a session cap or a memory limit:
//
//	shpellcast-bench -http http://127.0.0.1:8080 -tcp 127.0.0.1:1337 \
//	    -sessions 100 -viewers 1 -rate 2 -duration 30s
//
// Each producer stamps every line with a monotonic timestamp; each viewer
// reads those lines back and records the delay. The result is a real
// end-to-end latency distribution through the socket, the ring buffer,
// the fan-out and the HTTP response — not a microbenchmark of one
// function.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type opts struct {
	httpBase  string
	tcpAddr   string
	sessions  int
	viewers   int
	rate      float64
	lineSize  int
	duration  time.Duration
	rampUp    time.Duration
	transport string
}

// stats are accumulated lock-free by producers and viewers.
type stats struct {
	producersStarted atomic.Int64
	producersFailed  atomic.Int64
	viewersStarted   atomic.Int64
	viewersFailed    atomic.Int64
	linesSent        atomic.Int64
	linesReceived    atomic.Int64
	bytesSent        atomic.Int64
	bytesReceived    atomic.Int64
	gaps             atomic.Int64
	writeErrors      atomic.Int64

	mu      sync.Mutex
	latency []time.Duration
}

func (s *stats) observe(d time.Duration) {
	s.mu.Lock()
	// Reservoir-free: we simply cap the sample to keep memory flat even on
	// a long run. The first million observations are plenty for p99.
	if len(s.latency) < 1_000_000 {
		s.latency = append(s.latency, d)
	}
	s.mu.Unlock()
}

func main() {
	var o opts
	flag.StringVar(&o.httpBase, "http", "http://127.0.0.1:8080", "shpellcast HTTP base URL")
	flag.StringVar(&o.tcpAddr, "tcp", "127.0.0.1:1337", "shpellcast raw TCP ingest address")
	flag.IntVar(&o.sessions, "sessions", 100, "concurrent producer sessions")
	flag.IntVar(&o.viewers, "viewers", 1, "viewers attached per session")
	flag.Float64Var(&o.rate, "rate", 2, "log lines per second, per session")
	flag.IntVar(&o.lineSize, "line-size", 120, "approximate bytes per log line")
	flag.DurationVar(&o.duration, "duration", 30*time.Second, "how long to stream")
	flag.DurationVar(&o.rampUp, "ramp-up", 2*time.Second, "spread session starts over this window")
	flag.StringVar(&o.transport, "transport", "tcp", "producer transport: tcp or http")
	flag.Parse()

	if o.transport != "tcp" && o.transport != "http" {
		fmt.Fprintln(os.Stderr, "-transport must be tcp or http")
		os.Exit(2)
	}

	fmt.Printf("shpellcast-bench: %d sessions x %d viewers, %.1f lines/s each, %s via %s\n",
		o.sessions, o.viewers, o.rate, o.duration, o.transport)

	before, err := scrape(o.httpBase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot reach %s/metrics: %v\n", o.httpBase, err)
		os.Exit(1)
	}

	st := &stats{}
	ctx, cancel := context.WithTimeout(context.Background(), o.duration)
	defer cancel()

	// Sample the server while the load runs, so we see the peak rather
	// than whatever it has settled back to afterwards.
	peak := map[string]float64{}
	var peakMu sync.Mutex
	sampler := time.NewTicker(500 * time.Millisecond)
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sampler.C:
				if m, err := scrape(o.httpBase); err == nil {
					peakMu.Lock()
					for k, v := range m {
						if v > peak[k] {
							peak[k] = v
						}
					}
					peakMu.Unlock()
				}
			}
		}
	}()

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < o.sessions; i++ {
		delay := time.Duration(0)
		if o.rampUp > 0 && o.sessions > 1 {
			delay = time.Duration(int64(o.rampUp) * int64(i) / int64(o.sessions))
		}
		wg.Add(1)
		go func(n int, delay time.Duration) {
			defer wg.Done()
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
			runSession(ctx, o, st, n)
		}(i, delay)
	}
	wg.Wait()
	sampler.Stop()
	<-samplerDone
	elapsed := time.Since(start)

	// Give the last writes a moment to settle before reading counters.
	time.Sleep(500 * time.Millisecond)
	after, _ := scrape(o.httpBase)

	report(o, st, before, after, peak, elapsed)
}

// runSession opens one producer and its viewers, streams for the duration
// of ctx, then tears everything down.
func runSession(ctx context.Context, o opts, st *stats, n int) {
	var (
		id     string
		writer io.WriteCloser
	)

	switch o.transport {
	case "tcp":
		conn, err := net.Dial("tcp", o.tcpAddr)
		if err != nil {
			st.producersFailed.Add(1)
			return
		}
		br := bufio.NewReader(conn)
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		banner, err := br.ReadString('\n')
		if err != nil {
			st.producersFailed.Add(1)
			conn.Close()
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		if strings.Contains(banner, "refused") {
			st.producersFailed.Add(1)
			conn.Close()
			return
		}
		url := strings.TrimSpace(strings.TrimPrefix(banner, "shpellcast: "))
		id = url[strings.LastIndex(url, "/")+1:]
		writer = conn

	case "http":
		sid, token, err := createSession(o.httpBase)
		if err != nil {
			st.producersFailed.Add(1)
			return
		}
		id = sid
		pr, pw := io.Pipe()
		req, _ := http.NewRequest(http.MethodPut, o.httpBase+"/i/"+sid, pr)
		req.Header.Set("X-Shpellcast-Token", token)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
		writer = pw
	}

	st.producersStarted.Add(1)
	defer writer.Close()

	var vwg sync.WaitGroup
	for v := 0; v < o.viewers; v++ {
		vwg.Add(1)
		go func() {
			defer vwg.Done()
			runViewer(ctx, o, st, id)
		}()
	}

	// Let viewers attach before the first line, so latency samples cover
	// the live path rather than the replay path.
	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
	}

	pad := strings.Repeat("=", max(0, o.lineSize-40))
	interval := time.Duration(float64(time.Second) / o.rate)
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			vwg.Wait()
			return
		case <-tick.C:
			line := fmt.Sprintf("T%d %s\n", time.Now().UnixNano(), pad)
			if _, err := io.WriteString(writer, line); err != nil {
				st.writeErrors.Add(1)
				vwg.Wait()
				return
			}
			st.linesSent.Add(1)
			st.bytesSent.Add(int64(len(line)))
		}
	}
}

// runViewer tails a session over the raw endpoint and times each line
// against the timestamp the producer stamped into it.
func runViewer(ctx context.Context, o opts, st *stats, id string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.httpBase+"/s/"+id+"/raw", nil)
	if err != nil {
		st.viewersFailed.Add(1)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		st.viewersFailed.Add(1)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		st.viewersFailed.Add(1)
		return
	}
	st.viewersStarted.Add(1)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		st.bytesReceived.Add(int64(len(line) + 1))
		if strings.Contains(line, "fell behind") {
			st.gaps.Add(1)
			continue
		}
		if !strings.HasPrefix(line, "T") {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		sent, err := strconv.ParseInt(line[1:sp], 10, 64)
		if err != nil {
			continue
		}
		st.linesReceived.Add(1)
		st.observe(time.Duration(time.Now().UnixNano() - sent))
	}
}

func createSession(base string) (id, token string, err error) {
	resp, err := http.Get(base + "/new?format=text")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	b, _ := io.ReadAll(resp.Body)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 3 {
		return "", "", fmt.Errorf("unexpected /new response %q", b)
	}
	return lines[0], lines[2], nil
}

// scrape reads the server's own metrics so the report can show what the
// server observed, not only what the client did.
func scrape(base string) (map[string]float64, error) {
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		if v, err := strconv.ParseFloat(line[i+1:], 64); err == nil {
			out[line[:i]] = v
		}
	}
	return out, nil
}

func report(o opts, st *stats, before, after, peak map[string]float64, elapsed time.Duration) {
	st.mu.Lock()
	lat := append([]time.Duration(nil), st.latency...)
	st.mu.Unlock()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })

	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		i := int(float64(len(lat)-1) * p)
		return lat[i]
	}

	sent := st.linesSent.Load()
	recv := st.linesReceived.Load()
	expected := sent * int64(o.viewers)

	fmt.Printf("\n=== client ===\n")
	fmt.Printf("  duration            %s\n", elapsed.Round(10*time.Millisecond))
	fmt.Printf("  producers           %d started, %d failed\n", st.producersStarted.Load(), st.producersFailed.Load())
	fmt.Printf("  viewers             %d started, %d failed\n", st.viewersStarted.Load(), st.viewersFailed.Load())
	fmt.Printf("  lines sent          %d (%.0f/s aggregate)\n", sent, float64(sent)/elapsed.Seconds())
	fmt.Printf("  lines received      %d of %d expected (%.2f%%)\n", recv, expected, 100*float64(recv)/float64(max64(expected, 1)))
	fmt.Printf("  bytes in / out      %s / %s\n", human(st.bytesSent.Load()), human(st.bytesReceived.Load()))
	fmt.Printf("  write errors        %d\n", st.writeErrors.Load())
	fmt.Printf("  viewer gap notices  %d\n", st.gaps.Load())

	fmt.Printf("\n=== end-to-end latency (producer write -> viewer read) ===\n")
	if len(lat) == 0 {
		fmt.Printf("  no samples\n")
	} else {
		fmt.Printf("  samples             %d\n", len(lat))
		fmt.Printf("  p50                 %s\n", pct(0.50).Round(time.Microsecond))
		fmt.Printf("  p90                 %s\n", pct(0.90).Round(time.Microsecond))
		fmt.Printf("  p99                 %s\n", pct(0.99).Round(time.Microsecond))
		fmt.Printf("  p99.9               %s\n", pct(0.999).Round(time.Microsecond))
		fmt.Printf("  max                 %s\n", lat[len(lat)-1].Round(time.Microsecond))
	}

	d := func(k string) float64 { return after[k] - before[k] }
	fmt.Printf("\n=== server ===\n")
	fmt.Printf("  sessions created    %.0f\n", d("shpellcast_sessions_created_total"))
	fmt.Printf("  sessions rejected   %.0f\n", d("shpellcast_sessions_rejected_total"))
	fmt.Printf("  bytes ingested      %s\n", human(int64(d("shpellcast_bytes_ingested_total"))))
	fmt.Printf("  viewers dropped     %.0f  (slow-consumer evictions)\n", d("shpellcast_viewers_dropped_total"))
	fmt.Printf("  peak live sessions  %.0f\n", peak["shpellcast_sessions_live"])
	fmt.Printf("  peak live viewers   %.0f\n", peak["shpellcast_viewers_live"])
	fmt.Printf("  peak goroutines     %.0f\n", peak["go_goroutines"])
	fmt.Printf("  peak heap in use    %s\n", human(int64(peak["go_memstats_heap_inuse_bytes"])))
	fmt.Printf("  peak rss-ish (sys)  %s\n", human(int64(peak["go_memstats_sys_bytes"])))
	fmt.Printf("  peak open fds       %.0f\n", peak["process_open_fds"])
	fmt.Printf("  gc cycles           %.0f\n", d("go_gc_cycles_total"))

	fmt.Println()
	switch {
	case st.producersFailed.Load() > 0:
		fmt.Println("RESULT: FAILED — some producers could not start (check -max-per-client and -max-sessions)")
		os.Exit(1)
	case expected > 0 && float64(recv)/float64(expected) < 0.99:
		fmt.Println("RESULT: FAILED — viewers missed more than 1% of lines")
		os.Exit(1)
	case pct(0.99) > 2*time.Second:
		fmt.Println("RESULT: FAILED — p99 latency above 2s")
		os.Exit(1)
	default:
		fmt.Println("RESULT: PASS")
	}
}

func human(n int64) string {
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
