package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"

	"loadbalancer/balancer"
	"loadbalancer/config"
	"loadbalancer/metrics"
	"loadbalancer/ratelimit"
)

// TCPFrontend manages a TCP (or gRPC) frontend listener.
type TCPFrontend struct {
	cfg       config.Frontend
	balancer  balancer.Balancer
	limiter   *ratelimit.Limiter
	stats     *metrics.FrontendStats
	metricsSt *metrics.Store
	listener  net.Listener
	wg        sync.WaitGroup
	cancel    context.CancelFunc
}

// NewTCPFrontend creates a TCPFrontend.
func NewTCPFrontend(
	cfg config.Frontend,
	bal balancer.Balancer,
	limiter *ratelimit.Limiter,
	store *metrics.Store,
) *TCPFrontend {
	stats := store.GetOrCreate(cfg.Name, cfg.Protocol, cfg.Port)
	return &TCPFrontend{
		cfg:       cfg,
		balancer:  bal,
		limiter:   limiter,
		stats:     stats,
		metricsSt: store,
	}
}

// Start begins listening and accepting connections.
func (f *TCPFrontend) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", listenBind(ctx), f.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp listen on %s: %w", addr, err)
	}
	f.listener = ln

	ctx, cancel := context.WithCancel(ctx)
	f.cancel = cancel

	slog.Info("TCP frontend listening", "name", f.cfg.Name, "addr", addr, "protocol", f.cfg.Protocol)

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					slog.Error("accept error", "frontend", f.cfg.Name, "err", err)
					return
				}
			}
			f.wg.Add(1)
			go func(c net.Conn) {
				defer f.wg.Done()
				f.handleConn(c)
			}(conn)
		}
	}()
	return nil
}

// Shutdown stops accepting new connections and waits for existing ones to finish.
func (f *TCPFrontend) Shutdown() {
	if f.cancel != nil {
		f.cancel()
	}
	if f.listener != nil {
		f.listener.Close()
	}
	f.wg.Wait()
}

func (f *TCPFrontend) handleConn(clientConn net.Conn) {
	defer clientConn.Close()

	clientIP := extractIP(clientConn.RemoteAddr().String())

	// Rate limiting.
	if f.limiter != nil && !f.limiter.Allow(clientIP) {
		slog.Debug("rate limited", "frontend", f.cfg.Name, "ip", clientIP)
		f.metricsSt.IncError(f.cfg.Name, f.cfg.Protocol)
		return
	}

	// Pick backend.
	backend := f.balancer.Next(clientIP)
	if backend == nil {
		slog.Error("no backend available", "frontend", f.cfg.Name)
		f.metricsSt.IncError(f.cfg.Name, f.cfg.Protocol)
		return
	}

	// Connect to backend.
	backendConn, err := net.Dial("tcp", backend.Address)
	if err != nil {
		slog.Error("dial backend failed", "frontend", f.cfg.Name, "backend", backend.Address, "err", err)
		backend.Errors.Add(1)
		f.metricsSt.IncError(f.cfg.Name, f.cfg.Protocol)
		return
	}
	defer backendConn.Close()

	// Track connections.
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

	// Bidirectional copy.
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(backendConn, clientConn) //nolint:errcheck
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, backendConn) //nolint:errcheck
		done <- struct{}{}
	}()

	<-done
}

// listenBind extracts the bind address from the context value (set by main).
// Falls back to 0.0.0.0.
func listenBind(ctx context.Context) string {
	if v := ctx.Value(bindKey{}); v != nil {
		return v.(string)
	}
	return "0.0.0.0"
}

type bindKey struct{}

// WithBind stores the bind address into a context.
func WithBind(ctx context.Context, bind string) context.Context {
	return context.WithValue(ctx, bindKey{}, bind)
}

func extractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Could be just an IP without port.
		return strings.TrimSpace(addr)
	}
	return host
}
