package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"

	"loadbalancer/balancer"
	"loadbalancer/metrics"
)

//go:embed static/index.html
var staticFiles embed.FS

// PoolInfo summarises a backend pool for the dashboard.
type PoolInfo struct {
	Name      string        `json:"name"`
	Algorithm string        `json:"algorithm"`
	Backends  []BackendInfo `json:"backends"`
}

// BackendInfo summarises a single backend server.
type BackendInfo struct {
	Address     string `json:"address"`
	Weight      int    `json:"weight"`
	Healthy     bool   `json:"healthy"`
	ActiveConns int64  `json:"active_conns"`
	TotalReqs   int64  `json:"total_reqs"`
	Errors      int64  `json:"errors"`
}

// FrontendInfo summarises a frontend for the dashboard.
type FrontendInfo struct {
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	Port        int    `json:"port"`
	ActiveConns int64  `json:"active_conns"`
	ReqsPerSec  int64  `json:"reqs_per_sec"`
	TotalReqs   int64  `json:"total_reqs"`
	Errors      int64  `json:"errors"`
}

// DashboardPayload is the JSON pushed to WebSocket clients each second.
type DashboardPayload struct {
	Uptime        string         `json:"uptime"`
	ConfigVersion int64          `json:"config_version"`
	Frontends     []FrontendInfo `json:"frontends"`
	Pools         []PoolInfo     `json:"pools"`
}

// Pool groups pool metadata with its balancer for the dashboard.
type Pool struct {
	Name      string
	Algorithm string
	Balancer  balancer.Balancer
}

// Server serves the web dashboard and pushes live updates via WebSocket.
type Server struct {
	port          int
	configPath    string
	store         *metrics.Store
	start         time.Time
	upgrader      websocket.Upgrader
	server        *http.Server
	clients       map[*websocket.Conn]struct{}
	clientsMu     sync.Mutex
	pools         []Pool
	poolsMu       sync.RWMutex
	configVersion atomic.Int64
}

// NewServer creates a dashboard Server.
func NewServer(port int, store *metrics.Store, pools []Pool, configPath string) *Server {
	s := &Server{
		port:       port,
		configPath: configPath,
		store:      store,
		pools:      pools,
		start:      time.Now(),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		clients: make(map[*websocket.Conn]struct{}),
	}
	return s
}

// SetPools replaces the pool list (called on config hot-reload).
func (s *Server) SetPools(pools []Pool) {
	s.poolsMu.Lock()
	s.pools = pools
	s.poolsMu.Unlock()
}

// NotifyReload increments the config version counter so connected browsers
// can detect and announce a successful hot-reload.
func (s *Server) NotifyReload() {
	s.configVersion.Add(1)
}

// Start begins serving the dashboard.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.wsHandler)
	mux.HandleFunc("/api/config", s.configHandler)
	mux.HandleFunc("/", s.indexHandler)

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	go s.broadcastLoop(ctx)

	slog.Info("dashboard listening", "port", s.port)
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("dashboard server error", "err", err)
		}
	}()
	return nil
}

// Shutdown gracefully stops the dashboard server.
func (s *Server) Shutdown(ctx context.Context) {
	s.server.Shutdown(ctx) //nolint:errcheck
}

func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "dashboard not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data) //nolint:errcheck
}

func (s *Server) configHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(s.configPath)
		if err != nil {
			http.Error(w, "cannot read config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(data) //nolint:errcheck

	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read error: "+err.Error(), http.StatusBadRequest)
			return
		}
		var tmp interface{}
		if err := yaml.Unmarshal(body, &tmp); err != nil {
			http.Error(w, "invalid YAML: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := os.WriteFile(s.configPath, body, 0o644); err != nil {
			http.Error(w, "write error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, "saved")

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "err", err)
		return
	}

	s.clientsMu.Lock()
	s.clients[conn] = struct{}{}
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, conn)
		s.clientsMu.Unlock()
		conn.Close()
	}()

	s.sendPayload(conn)

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			return
		}
	}
}

func (s *Server) broadcastLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.store.Tick()
			s.broadcast()
		}
	}
}

func (s *Server) broadcast() {
	payload := s.buildPayload()
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Error("marshal dashboard payload", "err", err)
		return
	}

	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()

	for conn := range s.clients {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			conn.Close()
			delete(s.clients, conn)
		}
	}
}

func (s *Server) sendPayload(conn *websocket.Conn) {
	payload := s.buildPayload()
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	conn.WriteMessage(websocket.TextMessage, data) //nolint:errcheck
}

func (s *Server) buildPayload() DashboardPayload {
	uptime := time.Since(s.start).Round(time.Second).String()

	frontendSnaps := s.store.Snapshot()
	frontends := make([]FrontendInfo, 0, len(frontendSnaps))
	for _, fs := range frontendSnaps {
		frontends = append(frontends, FrontendInfo{
			Name:        fs.Name,
			Protocol:    fs.Protocol,
			Port:        fs.Port,
			ActiveConns: fs.ActiveConns,
			ReqsPerSec:  fs.ReqsLastSec,
			TotalReqs:   fs.TotalReqs,
			Errors:      fs.Errors,
		})
	}

	s.poolsMu.RLock()
	currentPools := s.pools
	s.poolsMu.RUnlock()

	pools := make([]PoolInfo, 0, len(currentPools))
	for _, p := range currentPools {
		var backends []BackendInfo
		for _, b := range p.Balancer.All() {
			backends = append(backends, BackendInfo{
				Address:     b.Address,
				Weight:      b.Weight,
				Healthy:     b.IsHealthy(),
				ActiveConns: b.ActiveConns.Load(),
				TotalReqs:   b.TotalReqs.Load(),
				Errors:      b.Errors.Load(),
			})
		}
		pools = append(pools, PoolInfo{
			Name:      p.Name,
			Algorithm: p.Algorithm,
			Backends:  backends,
		})
	}

	return DashboardPayload{
		Uptime:        uptime,
		ConfigVersion: s.configVersion.Load(),
		Frontends:     frontends,
		Pools:         pools,
	}
}
