package balancer

import "sync"

// Ref is a thread-safe, hot-swappable Balancer wrapper.
// Components hold *Ref; call Swap to replace the inner Balancer without
// restarting those components (the pointer itself stays the same).
type Ref struct {
	mu  sync.RWMutex
	bal Balancer
}

// NewRef wraps b in a Ref.
func NewRef(b Balancer) *Ref { return &Ref{bal: b} }

func (r *Ref) Next(key string) *Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bal.Next(key)
}

func (r *Ref) All() []*Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bal.All()
}

// Swap atomically replaces the inner Balancer.
func (r *Ref) Swap(b Balancer) {
	r.mu.Lock()
	r.bal = b
	r.mu.Unlock()
}
