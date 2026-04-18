package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"loadbalancer/balancer"
	"loadbalancer/config"
	"loadbalancer/metrics"
	"loadbalancer/ratelimit"
)

const udpSessionTimeout = 30 * time.Second
const udpBufferSize = 65535

// udpSession tracks the upstream connection for a client address.
type udpSession struct {
	backend    *balancer.Backend
	upstreamPC net.PacketConn
	lastSeen   time.Time
}

// UDPFrontend manages a UDP frontend.
type UDPFrontend struct {
	cfg       config.Frontend
	balancer  balancer.Balancer
	limiter   *ratelimit.Limiter
	stats     *metrics.FrontendStats
	metricsSt *metrics.Store
	conn      net.PacketConn
	sessions  map[string]*udpSession
	mu        sync.Mutex
	cancel    context.CancelFunc
}

// NewUDPFrontend creates a UDPFrontend.
func NewUDPFrontend(
	cfg config.Frontend,
	bal balancer.Balancer,
	limiter *ratelimit.Limiter,
	store *metrics.Store,
) *UDPFrontend {
	stats := store.GetOrCreate(cfg.Name, cfg.Protocol, cfg.Port)
	return &UDPFrontend{
		cfg:       cfg,
		balancer:  bal,
		limiter:   limiter,
		stats:     stats,
		metricsSt: store,
		sessions:  make(map[string]*udpSession),
	}
}

// Start begins listening for UDP packets.
func (f *UDPFrontend) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", listenBind(ctx), f.cfg.Port)
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("udp listen on %s: %w", addr, err)
	}
	f.conn = pc

	ctx, cancel := context.WithCancel(ctx)
	f.cancel = cancel

	slog.Info("UDP frontend listening", "name", f.cfg.Name, "addr", addr)

	go f.readLoop(ctx)
	go f.cleanupLoop(ctx)
	return nil
}

// Shutdown stops the UDP frontend.
func (f *UDPFrontend) Shutdown() {
	if f.cancel != nil {
		f.cancel()
	}
	if f.conn != nil {
		f.conn.Close()
	}
}

func (f *UDPFrontend) readLoop(ctx context.Context) {
	buf := make([]byte, udpBufferSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, clientAddr, err := f.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				slog.Error("UDP read error", "frontend", f.cfg.Name, "err", err)
				return
			}
		}

		clientIP := extractIP(clientAddr.String())

		// Rate limiting.
		if f.limiter != nil && !f.limiter.Allow(clientIP) {
			f.metricsSt.IncError(f.cfg.Name, f.cfg.Protocol)
			continue
		}

		// Copy packet data.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		go f.forwardPacket(ctx, clientAddr, clientIP, pkt)
	}
}

func (f *UDPFrontend) forwardPacket(ctx context.Context, clientAddr net.Addr, clientIP string, pkt []byte) {
	f.mu.Lock()
	sess, ok := f.sessions[clientAddr.String()]
	if !ok {
		backend := f.balancer.Next(clientAddr.String())
		if backend == nil {
			f.mu.Unlock()
			f.metricsSt.IncError(f.cfg.Name, f.cfg.Protocol)
			return
		}

		upstreamPC, err := net.ListenPacket("udp", "0.0.0.0:0")
		if err != nil {
			f.mu.Unlock()
			slog.Error("UDP upstream listen failed", "err", err)
			f.metricsSt.IncError(f.cfg.Name, f.cfg.Protocol)
			return
		}

		sess = &udpSession{
			backend:    backend,
			upstreamPC: upstreamPC,
			lastSeen:   time.Now(),
		}
		f.sessions[clientAddr.String()] = sess

		backend.ActiveConns.Add(1)
		f.stats.ActiveConns.Add(1)

		// Start reverse goroutine: upstream → client.
		go f.reverseLoop(ctx, clientAddr, sess)
	} else {
		sess.lastSeen = time.Now()
	}
	f.mu.Unlock()

	// Forward client → upstream.
	upstreamAddr, err := net.ResolveUDPAddr("udp", sess.backend.Address)
	if err != nil {
		slog.Error("resolve upstream addr failed", "addr", sess.backend.Address, "err", err)
		return
	}
	sess.upstreamPC.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	_, err = sess.upstreamPC.WriteTo(pkt, upstreamAddr)
	if err != nil {
		slog.Error("UDP forward to upstream failed", "err", err)
		sess.backend.Errors.Add(1)
		f.metricsSt.IncError(f.cfg.Name, f.cfg.Protocol)
		return
	}

	f.stats.TotalReqs.Add(1)
	sess.backend.TotalReqs.Add(1)
	f.metricsSt.IncRequest(f.cfg.Name, f.cfg.Protocol)
}

func (f *UDPFrontend) reverseLoop(ctx context.Context, clientAddr net.Addr, sess *udpSession) {
	buf := make([]byte, udpBufferSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		sess.upstreamPC.SetReadDeadline(time.Now().Add(udpSessionTimeout)) //nolint:errcheck
		n, _, err := sess.upstreamPC.ReadFrom(buf)
		if err != nil {
			// Session timed out or closed.
			return
		}
		f.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		f.conn.WriteTo(buf[:n], clientAddr)                       //nolint:errcheck
		f.mu.Lock()
		if s, ok := f.sessions[clientAddr.String()]; ok {
			s.lastSeen = time.Now()
		}
		f.mu.Unlock()
	}
}

func (f *UDPFrontend) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.mu.Lock()
			now := time.Now()
			for key, sess := range f.sessions {
				if now.Sub(sess.lastSeen) > udpSessionTimeout {
					sess.upstreamPC.Close()
					sess.backend.ActiveConns.Add(-1)
					f.stats.ActiveConns.Add(-1)
					delete(f.sessions, key)
				}
			}
			f.mu.Unlock()
		}
	}
}
