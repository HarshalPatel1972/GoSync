// Command gosync-loadtest simulates many users, each with several devices,
// writing concurrently, and measures end-to-end propagation latency: the time
// from a write on one device until it is readable on the user's other
// devices.
//
// The target server must run with GOSYNC_INSECURE_DEV_AUTH=true (tokens are
// user ids) or accept the tokens produced by -token-prefix.
//
//	go run ./cmd/gosync-loadtest -url ws://localhost:8080/sync -users 250 -devices 4 -duration 60s
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarshalPatel1972/GoSync/client"
)

type stats struct {
	mu        sync.Mutex
	latencies []time.Duration
	connectMS []time.Duration
	written   atomic.Int64
	writeErrs atomic.Int64
	syncErrs  atomic.Int64
}

func (s *stats) addLatency(d time.Duration) {
	s.mu.Lock()
	s.latencies = append(s.latencies, d)
	s.mu.Unlock()
}

func main() {
	url := flag.String("url", "ws://127.0.0.1:8080/sync", "sync endpoint; a comma-separated list spreads each user's devices across servers")
	users := flag.Int("users", 100, "number of users (namespaces)")
	devices := flag.Int("devices", 3, "devices per user")
	duration := flag.Duration("duration", 30*time.Second, "how long to write")
	interval := flag.Duration("interval", 2*time.Second, "mean time between writes per device")
	payload := flag.Int("payload", 200, "bytes per written value")
	ramp := flag.Duration("ramp", 5*time.Second, "spread connection start over this long")
	prefix := flag.String("token-prefix", "load-", "token = prefix + user index")
	metricsURL := flag.String("metrics", "", "optional server metrics URL to sample (e.g. http://127.0.0.1:9090/metrics)")
	maxP99 := flag.Duration("max-p99", 0, "exit non-zero if p99 latency exceeds this")
	minDelivery := flag.Float64("min-delivery", 0, "exit non-zero if fewer than this fraction of expected deliveries arrive (0-1)")
	flag.Parse()

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &stats{}
	run := fmt.Sprintf("%x", time.Now().UnixNano())
	value := strings.Repeat("x", *payload)
	total := *users * *devices
	urls := strings.Split(*url, ",")

	fmt.Printf("load test: %d users × %d devices = %d connections across %d server(s), write every ~%v for %v\n", *users, *devices, total, len(urls), *interval, *duration)

	var clients []*client.Client
	var online sync.WaitGroup
	// Writers start only once every device is connected, so latency measures
	// propagation rather than devices still joining.
	startWrites := make(chan struct{})
	for u := 0; u < *users; u++ {
		token := fmt.Sprintf("%s%s-%d", *prefix, run, u)
		for d := 0; d < *devices; d++ {
			me := fmt.Sprintf("u%d-d%d", u, d)
			c, err := client.New(ctx, client.Options{
				URL:    urls[d%len(urls)], // a user's devices land on different servers
				Token:  func(context.Context) (string, error) { return token, nil },
				Store:  client.NewMemoryStore(),
				Dialer: client.WebSocketDialer{},
				Logger: quiet,
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			seen := sync.Map{}
			var start time.Time // set when the client starts connecting
			var once sync.Once
			online.Add(1)
			c.Subscribe(func(ev client.Event) {
				switch ev.Kind {
				case "status":
					if ev.Status == client.StatusOnline {
						once.Do(func() {
							st.mu.Lock()
							st.connectMS = append(st.connectMS, time.Since(start))
							st.mu.Unlock()
							online.Done()
						})
					}
				case "error":
					st.syncErrs.Add(1)
				case "change":
					now := time.Now()
					for _, id := range ev.IDs {
						if strings.HasPrefix(id, me+"-") {
							continue // own write
						}
						if _, dup := seen.LoadOrStore(id, true); dup {
							continue
						}
						doc, ok, err := c.Get(ctx, "load", id)
						if err != nil || !ok {
							continue
						}
						var sent int64
						if json.Unmarshal(doc.Fields["sent"], &sent) == nil && sent > 0 {
							st.addLatency(now.Sub(time.Unix(0, sent)))
						}
					}
				}
			})
			clients = append(clients, c)
			delay := time.Duration(rand.Int64N(int64(*ramp) + 1))
			go func() {
				time.Sleep(delay)
				start = time.Now()
				c.Run(ctx)
			}()
			go func() {
				<-startWrites
				writer(ctx, c, me, value, *interval, *duration, st)
			}()
		}
	}

	waitCh := make(chan struct{})
	go func() { online.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(*ramp + 60*time.Second):
		fmt.Println("warning: not every client came online")
	}
	close(startWrites)
	connected := len(st.connectMS)
	fmt.Printf("connected: %d/%d  (connect time p50 %v, p99 %v)\n", connected, total, pct(st.connectMS, 50), pct(st.connectMS, 99))

	if *metricsURL != "" {
		go sampleMetrics(ctx, *metricsURL)
	}
	time.Sleep(*duration)
	fmt.Println("writes finished; waiting 10s for in-flight deliveries")
	time.Sleep(10 * time.Second)

	st.mu.Lock()
	lat := slices.Clone(st.latencies)
	st.mu.Unlock()
	written := st.written.Load()
	expected := written * int64(*devices-1)
	delivery := 1.0
	if expected > 0 {
		delivery = float64(len(lat)) / float64(expected)
	}
	fmt.Printf("writes: %d (%.0f/s), write errors: %d, sync errors: %d\n", written, float64(written)/duration.Seconds(), st.writeErrs.Load(), st.syncErrs.Load())
	fmt.Printf("deliveries: %d/%d (%.2f%%)\n", len(lat), expected, delivery*100)
	fmt.Printf("propagation latency: p50 %v  p95 %v  p99 %v  max %v\n", pct(lat, 50), pct(lat, 95), pct(lat, 99), pct(lat, 100))
	if *metricsURL != "" {
		printMetrics(*metricsURL)
	}

	failed := false
	if *maxP99 > 0 && pct(lat, 99) > *maxP99 {
		fmt.Printf("FAIL: p99 %v exceeds %v\n", pct(lat, 99), *maxP99)
		failed = true
	}
	if *minDelivery > 0 && delivery < *minDelivery {
		fmt.Printf("FAIL: delivery %.4f below %.4f\n", delivery, *minDelivery)
		failed = true
	}
	if connected < total || st.writeErrs.Load() > 0 || st.syncErrs.Load() > 0 {
		fmt.Println("FAIL: connection, write or sync errors")
		failed = true
	}
	cancel()
	if failed {
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func writer(ctx context.Context, c *client.Client, me, value string, interval, duration time.Duration, st *stats) {
	end := time.Now().Add(duration)
	for seq := 0; time.Now().Before(end); seq++ {
		// Exponentially distributed gaps: a Poisson write process.
		time.Sleep(time.Duration(rand.ExpFloat64() * float64(interval)))
		if ctx.Err() != nil {
			return
		}
		sent, _ := json.Marshal(time.Now().UnixNano())
		v, _ := json.Marshal(value)
		err := c.Set(ctx, "load", fmt.Sprintf("%s-%d", me, seq), map[string]json.RawMessage{"sent": sent, "v": v})
		if err != nil {
			st.writeErrs.Add(1)
			continue
		}
		st.written.Add(1)
	}
}

func pct(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	i := (len(s) - 1) * p / 100
	return s[i].Round(time.Millisecond / 10)
}

var peak struct {
	sync.Mutex
	rss, goroutines float64
}

func sampleMetrics(ctx context.Context, url string) {
	for ctx.Err() == nil {
		m := scrape(url)
		peak.Lock()
		peak.rss = max(peak.rss, m["process_resident_memory_bytes"])
		peak.goroutines = max(peak.goroutines, m["go_goroutines"])
		peak.Unlock()
		time.Sleep(time.Second)
	}
}

func printMetrics(url string) {
	m := scrape(url)
	peak.Lock()
	defer peak.Unlock()
	fmt.Printf("server: peak RSS %.0f MiB, peak goroutines %.0f, sessions %.0f, CPU %.1fs\n",
		peak.rss/(1<<20), peak.goroutines, m["gosync_sessions"], m["process_cpu_seconds_total"])
	for _, op := range []string{"push", "pull"} {
		sum := m[`gosync_request_duration_seconds_sum{op="`+op+`"}`]
		n := m[`gosync_request_duration_seconds_count{op="`+op+`"}`]
		if n > 0 {
			fmt.Printf("server %s: %.0f requests, mean %v\n", op, n, time.Duration(sum/n*float64(time.Second)).Round(time.Microsecond*10))
		}
	}
}

func scrape(url string) map[string]float64 {
	out := map[string]float64{}
	resp, err := http.Get(url)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 2 {
			var v float64
			fmt.Sscan(f[1], &v)
			out[f[0]] = v
		}
	}
	return out
}
