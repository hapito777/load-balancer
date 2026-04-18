package balancer

import "sync/atomic"

// weighted implements weighted round-robin by expanding backends
// proportionally to their weight, then performing standard round-robin.
type weighted struct {
	all      []*Backend
	expanded []*Backend
	counter  atomic.Uint64
}

func newWeighted(backends []*Backend) *weighted {
	w := &weighted{all: backends}
	w.expanded = buildExpanded(backends)
	return w
}

func buildExpanded(backends []*Backend) []*Backend {
	var out []*Backend
	for _, b := range backends {
		w := b.Weight
		if w <= 0 {
			w = 1
		}
		for i := 0; i < w; i++ {
			out = append(out, b)
		}
	}
	return out
}

func (w *weighted) Next(_ string) *Backend {
	// Filter expanded list to healthy entries only.
	var healthy []*Backend
	seen := make(map[*Backend]bool)
	for _, b := range w.expanded {
		if b.IsHealthy() {
			healthy = append(healthy, b)
		} else if !seen[b] {
			seen[b] = true
		}
	}
	// Rebuild healthy expanded list.
	var healthyExpanded []*Backend
	for _, b := range w.expanded {
		if b.IsHealthy() {
			healthyExpanded = append(healthyExpanded, b)
		}
	}
	if len(healthyExpanded) == 0 {
		// Fall back to all expanded.
		healthyExpanded = w.expanded
	}
	if len(healthyExpanded) == 0 {
		return nil
	}
	idx := w.counter.Add(1) - 1
	return healthyExpanded[idx%uint64(len(healthyExpanded))]
}

func (w *weighted) All() []*Backend {
	return w.all
}
