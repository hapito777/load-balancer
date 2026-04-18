package balancer

import "sync/atomic"

type roundRobin struct {
	backends []*Backend
	counter  atomic.Uint64
}

func newRoundRobin(backends []*Backend) *roundRobin {
	return &roundRobin{backends: backends}
}

func (r *roundRobin) Next(_ string) *Backend {
	healthy := healthyBackends(r.backends)
	if len(healthy) == 0 {
		return nil
	}
	idx := r.counter.Add(1) - 1
	return healthy[idx%uint64(len(healthy))]
}

func (r *roundRobin) All() []*Backend {
	return r.backends
}
