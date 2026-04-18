package balancer

import (
	"fmt"
	"sync/atomic"
)

// Backend represents a single upstream server with atomic metrics.
type Backend struct {
	Address     string
	Weight      int
	healthy     atomic.Bool
	ActiveConns atomic.Int64
	TotalReqs   atomic.Int64
	Errors      atomic.Int64
}

// IsHealthy returns whether the backend is currently considered healthy.
func (b *Backend) IsHealthy() bool {
	return b.healthy.Load()
}

// SetHealthy sets the health state of the backend.
func (b *Backend) SetHealthy(v bool) {
	b.healthy.Store(v)
}

// Balancer selects a backend for a given request key.
type Balancer interface {
	// Next returns the next backend to use. key is used by consistent hashing.
	Next(key string) *Backend
	// All returns every backend (healthy or not).
	All() []*Backend
}

// New creates a Balancer for the given algorithm.
func New(algorithm string, backends []*Backend) (Balancer, error) {
	switch algorithm {
	case "round_robin", "":
		return newRoundRobin(backends), nil
	case "least_connections":
		return newLeastConn(backends), nil
	case "weighted":
		return newWeighted(backends), nil
	case "consistent_hash":
		return newConsistentHash(backends), nil
	default:
		return nil, fmt.Errorf("unknown balancer algorithm: %s", algorithm)
	}
}

// healthyBackends returns only the healthy subset of backends.
func healthyBackends(all []*Backend) []*Backend {
	out := make([]*Backend, 0, len(all))
	for _, b := range all {
		if b.IsHealthy() {
			out = append(out, b)
		}
	}
	// Fall back to all backends if every backend is unhealthy to avoid a dead pool.
	if len(out) == 0 {
		return all
	}
	return out
}
