package metrics

import (
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// FrontendStats holds live statistics for a frontend listener.
type FrontendStats struct {
	Name        string
	Protocol    string
	Port        int
	ActiveConns atomic.Int64
	TotalReqs   atomic.Int64
	ReqsLastSec atomic.Int64
	Errors      atomic.Int64

	// prevTotal is used to compute ReqsLastSec.
	prevTotal int64
}

// FrontendSnapshot is a plain-data snapshot of FrontendStats (safe to copy).
type FrontendSnapshot struct {
	Name        string
	Protocol    string
	Port        int
	ActiveConns int64
	TotalReqs   int64
	ReqsLastSec int64
	Errors      int64
}

// Store holds stats for all frontends and exposes Prometheus metrics.
type Store struct {
	mu        sync.RWMutex
	frontends map[string]*FrontendStats

	// Prometheus metrics.
	promTotalReqs   *prometheus.CounterVec
	promActiveConns *prometheus.GaugeVec
	promErrors      *prometheus.CounterVec
}

// NewStore creates an initialised Store and registers Prometheus collectors.
func NewStore() *Store {
	s := &Store{
		frontends: make(map[string]*FrontendStats),
	}

	s.promTotalReqs = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "loadbalancer_total_requests",
		Help: "Total number of requests processed per frontend.",
	}, []string{"frontend", "protocol"})

	s.promActiveConns = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "loadbalancer_active_connections",
		Help: "Number of currently active connections per frontend.",
	}, []string{"frontend", "protocol"})

	s.promErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "loadbalancer_errors_total",
		Help: "Total number of errors per frontend.",
	}, []string{"frontend", "protocol"})

	return s
}

// GetOrCreate returns existing stats for name, or creates them.
func (s *Store) GetOrCreate(name, protocol string, port int) *FrontendStats {
	s.mu.RLock()
	fs, ok := s.frontends[name]
	s.mu.RUnlock()
	if ok {
		return fs
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Double-check after acquiring write lock.
	if fs, ok = s.frontends[name]; ok {
		return fs
	}
	fs = &FrontendStats{Name: name, Protocol: protocol, Port: port}
	s.frontends[name] = fs
	return fs
}

// IncRequest increments the request counter for a frontend and updates Prometheus.
func (s *Store) IncRequest(name, protocol string) {
	s.mu.RLock()
	fs := s.frontends[name]
	s.mu.RUnlock()
	if fs != nil {
		fs.TotalReqs.Add(1)
	}
	s.promTotalReqs.WithLabelValues(name, protocol).Inc()
}

// IncError increments the error counter for a frontend and updates Prometheus.
func (s *Store) IncError(name, protocol string) {
	s.mu.RLock()
	fs := s.frontends[name]
	s.mu.RUnlock()
	if fs != nil {
		fs.Errors.Add(1)
	}
	s.promErrors.WithLabelValues(name, protocol).Inc()
}

// SetActiveConns sets the active connection gauge for Prometheus.
func (s *Store) SetActiveConns(name, protocol string, count int64) {
	s.promActiveConns.WithLabelValues(name, protocol).Set(float64(count))
}

// Tick should be called once per second; it computes ReqsLastSec for each frontend.
func (s *Store) Tick() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, fs := range s.frontends {
		current := fs.TotalReqs.Load()
		rps := current - fs.prevTotal
		fs.prevTotal = current
		fs.ReqsLastSec.Store(rps)
	}
}

// Snapshot returns a point-in-time copy of all frontend stats as plain data.
func (s *Store) Snapshot() []FrontendSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]FrontendSnapshot, 0, len(s.frontends))
	for _, fs := range s.frontends {
		out = append(out, FrontendSnapshot{
			Name:        fs.Name,
			Protocol:    fs.Protocol,
			Port:        fs.Port,
			ActiveConns: fs.ActiveConns.Load(),
			TotalReqs:   fs.TotalReqs.Load(),
			ReqsLastSec: fs.ReqsLastSec.Load(),
			Errors:      fs.Errors.Load(),
		})
	}
	return out
}

// SnapshotMap returns stats as a map keyed by name. Callers hold no lock.
func (s *Store) SnapshotMap() map[string]FrontendSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]FrontendSnapshot, len(s.frontends))
	for k, fs := range s.frontends {
		out[k] = FrontendSnapshot{
			Name:        fs.Name,
			Protocol:    fs.Protocol,
			Port:        fs.Port,
			ActiveConns: fs.ActiveConns.Load(),
			TotalReqs:   fs.TotalReqs.Load(),
			ReqsLastSec: fs.ReqsLastSec.Load(),
			Errors:      fs.Errors.Load(),
		}
	}
	return out
}

