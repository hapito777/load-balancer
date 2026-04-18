package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"

	"loadbalancer/balancer"
	"loadbalancer/config"
	"loadbalancer/metrics"
	"loadbalancer/ratelimit"
)

// HTTPFrontend manages an HTTP or HTTPS reverse-proxy frontend.
type HTTPFrontend struct {
	cfg       config.Frontend
	balancer  balancer.Balancer
	limiter   *ratelimit.Limiter
	stats     *metrics.FrontendStats
	metricsSt *metrics.Store
	server    *http.Server
	wg        sync.WaitGroup
}

// NewHTTPFrontend creates an HTTPFrontend.
func NewHTTPFrontend(
	cfg config.Frontend,
	bal balancer.Balancer,
	limiter *ratelimit.Limiter,
	store *metrics.Store,
) *HTTPFrontend {
	stats := store.GetOrCreate(cfg.Name, cfg.Protocol, cfg.Port)
	return &HTTPFrontend{
		cfg:       cfg,
		balancer:  bal,
		limiter:   limiter,
		stats:     stats,
		metricsSt: store,
	}
}

// Start begins listening and serving HTTP(S) requests.
func (f *HTTPFrontend) Start(ctx context.Context) error {
	bind := listenBind(ctx)
	addr := fmt.Sprintf("%s:%d", bind, f.cfg.Port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handler)

	f.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	var ln net.Listener
	var err error

	switch f.cfg.Protocol {
	case "https":
		if f.cfg.TLS == nil {
			return fmt.Errorf("frontend %s: TLS config required for https", f.cfg.Name)
		}
		cert, err := tls.LoadX509KeyPair(f.cfg.TLS.CertFile, f.cfg.TLS.KeyFile)
		if err != nil {
			return fmt.Errorf("load TLS cert/key for frontend %s: %w", f.cfg.Name, err)
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"h2", "http/1.1"},
		}
		ln, err = tls.Listen("tcp", addr, tlsCfg)
		if err != nil {
			return fmt.Errorf("tls listen on %s: %w", addr, err)
		}

	case "grpc":
		// gRPC over HTTP/2; TLS is optional.
		if f.cfg.TLS != nil {
			cert, err := tls.LoadX509KeyPair(f.cfg.TLS.CertFile, f.cfg.TLS.KeyFile)
			if err != nil {
				return fmt.Errorf("load TLS cert/key for gRPC frontend %s: %w", f.cfg.Name, err)
			}
			tlsCfg := &tls.Config{
				Certificates: []tls.Certificate{cert},
				NextProtos:   []string{"h2"},
			}
			ln, err = tls.Listen("tcp", addr, tlsCfg)
			if err != nil {
				return fmt.Errorf("grpc tls listen on %s: %w", addr, err)
			}
		} else {
			ln, err = net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("grpc listen on %s: %w", addr, err)
			}
		}

	default: // http
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("http listen on %s: %w", addr, err)
		}
	}

	slog.Info("HTTP frontend listening",
		"name", f.cfg.Name,
		"addr", addr,
		"protocol", f.cfg.Protocol)

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		if err := f.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "frontend", f.cfg.Name, "err", err)
		}
	}()
	return nil
}

// Shutdown gracefully stops the HTTP server.
func (f *HTTPFrontend) Shutdown(ctx context.Context) {
	if f.server != nil {
		f.server.Shutdown(ctx) //nolint:errcheck
	}
	f.wg.Wait()
}

func (f *HTTPFrontend) handler(w http.ResponseWriter, r *http.Request) {
	clientIP := extractIP(r.RemoteAddr)

	// Rate limiting.
	if f.limiter != nil && !f.limiter.Allow(clientIP) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		f.metricsSt.IncError(f.cfg.Name, f.cfg.Protocol)
		return
	}

	// Pick backend.
	backend := f.balancer.Next(clientIP)
	if backend == nil {
		http.Error(w, "No backends available", http.StatusBadGateway)
		f.metricsSt.IncError(f.cfg.Name, f.cfg.Protocol)
		return
	}

	// Track metrics.
	f.stats.ActiveConns.Add(1)
	f.stats.TotalReqs.Add(1)
	backend.ActiveConns.Add(1)
	backend.TotalReqs.Add(1)
	f.metricsSt.IncRequest(f.cfg.Name, f.cfg.Protocol)
	f.metricsSt.SetActiveConns(f.cfg.Name, f.cfg.Protocol, f.stats.ActiveConns.Load())

	defer func() {
		f.stats.ActiveConns.Add(-1)
		backend.ActiveConns.Add(-1)
		f.metricsSt.SetActiveConns(f.cfg.Name, f.cfg.Protocol, f.stats.ActiveConns.Load())
	}()

	// Build proxy.
	targetURL := &url.URL{
		Scheme: backendScheme(f.cfg.Protocol),
		Host:   backend.Address,
	}

	transport := f.buildTransport()
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("proxy error",
			"frontend", f.cfg.Name,
			"backend", backend.Address,
			"err", err)
		backend.Errors.Add(1)
		f.metricsSt.IncError(f.cfg.Name, f.cfg.Protocol)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	// Set X-Forwarded-For.
	r.Header.Set("X-Forwarded-For", clientIP)

	proxy.ServeHTTP(w, r)
}

func (f *HTTPFrontend) buildTransport() http.RoundTripper {
	if f.cfg.Protocol == "grpc" {
		return &http2Transport{}
	}
	return http.DefaultTransport
}

func backendScheme(protocol string) string {
	switch protocol {
	case "https":
		return "https"
	default:
		return "http"
	}
}

// http2Transport wraps http.Transport with HTTP/2 enabled for gRPC.
type http2Transport struct{}

func (t *http2Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec
			NextProtos:         []string{"h2"},
		},
		ForceAttemptHTTP2: true,
	}
	return transport.RoundTrip(req)
}
