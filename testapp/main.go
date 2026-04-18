// Test app: fake HTTP backends + load generator.
//
// Serve mode (default):
//
//	go run ./testapp
//	go run ./testapp -ports 8001,8002,8003
//
// Load mode:
//
//	go run ./testapp -mode load -target http://localhost:8080 -rps 100
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// ─── backend server ───────────────────────────────────────────────────────────

type backendProfile struct {
	name  string
	minMS int
	maxMS int
}

var profiles = []backendProfile{
	{"fast", 1, 10},
	{"normal", 20, 80},
	{"slow", 100, 300},
}

func startBackend(port int, profile backendProfile) {
	var totalReqs atomic.Int64

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"status": "ok",
			"server": fmt.Sprintf("backend-%d (%s)", port, profile.name),
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		delayMS := profile.minMS
		if profile.maxMS > profile.minMS {
			delayMS += rand.Intn(profile.maxMS - profile.minMS)
		}
		time.Sleep(time.Duration(delayMS) * time.Millisecond)

		n := totalReqs.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"server":      fmt.Sprintf("backend-%d (%s)", port, profile.name),
			"port":        port,
			"request_num": n,
			"latency_ms":  delayMS,
			"path":        r.URL.Path,
			"method":      r.Method,
		})
	})

	addr := fmt.Sprintf(":%d", port)
	log.Printf("[backend-%d] listening on %s  latency=%d-%dms", port, addr, profile.minMS, profile.maxMS)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[backend-%d] error: %v", port, err)
	}
}

// ─── load generator ──────────────────────────────────────────────────────────

func runLoad(target string, rps int, duration time.Duration) {
	// workers scales with rps so each worker handles ~500 rps max.
	workers := rps / 500
	if workers < 1 {
		workers = 1
	}

	limiter := rate.NewLimiter(rate.Limit(rps), rps)

	transport := &http.Transport{
		MaxIdleConnsPerHost: workers * 2,
		MaxConnsPerHost:     workers * 4,
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}

	var (
		success atomic.Int64
		errors  atomic.Int64
		latSum  atomic.Int64
	)

	ctx := context.Background()
	if duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}

	paths := []string{"/", "/api", "/data", "/users", "/status", "/healthz"}

	log.Printf("[load] target=%s  rps=%d  workers=%d  duration=%s", target, rps, workers, duration)

	for i := 0; i < workers; i++ {
		go func() {
			for {
				if err := limiter.Wait(ctx); err != nil {
					return
				}
				path := paths[rand.Intn(len(paths))]
				start := time.Now()
				resp, err := client.Get(target + path)
				elapsed := time.Since(start).Milliseconds()
				if err != nil {
					errors.Add(1)
					continue
				}
				resp.Body.Close()
				success.Add(1)
				latSum.Add(elapsed)
			}
		}()
	}

	// Stats printer every 5 seconds.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s, e, l := success.Swap(0), errors.Swap(0), latSum.Swap(0)
			avgMS := int64(0)
			if s > 0 {
				avgMS = l / s / 5
			}
			log.Printf("[load] last 5s — ok: %d  err: %d  avg_latency: %dms", s, e, avgMS)
		case <-ctx.Done():
			log.Println("[load] duration elapsed, stopping")
			return
		}
	}
}

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	mode := flag.String("mode", "serve", "mode: serve | load")
	portsStr := flag.String("ports", "8001,8002,8003", "comma-separated backend ports (serve mode)")
	target := flag.String("target", "http://localhost:8080", "load balancer URL (load mode)")
	rps := flag.Int("rps", 50, "requests per second (load mode)")
	duration := flag.Duration("duration", 0, "run duration, 0 = forever (load mode)")
	flag.Parse()

	switch *mode {
	case "serve":
		parts := strings.Split(*portsStr, ",")
		ports := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				log.Fatalf("invalid port %q: %v", p, err)
			}
			ports = append(ports, n)
		}

		for i, port := range ports {
			profile := profiles[i%len(profiles)]
			if i < len(ports)-1 {
				go startBackend(port, profile)
			} else {
				startBackend(port, profile) // blocks
			}
		}

	case "load":
		runLoad(*target, *rps, *duration)

	default:
		log.Fatalf("unknown mode %q — use 'serve' or 'load'", *mode)
	}
}
