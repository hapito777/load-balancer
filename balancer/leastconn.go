package balancer

type leastConn struct {
	backends []*Backend
}

func newLeastConn(backends []*Backend) *leastConn {
	return &leastConn{backends: backends}
}

func (l *leastConn) Next(_ string) *Backend {
	healthy := healthyBackends(l.backends)
	if len(healthy) == 0 {
		return nil
	}

	best := healthy[0]
	bestConns := best.ActiveConns.Load()

	for _, b := range healthy[1:] {
		conns := b.ActiveConns.Load()
		if conns < bestConns {
			best = b
			bestConns = conns
		}
	}
	return best
}

func (l *leastConn) All() []*Backend {
	return l.backends
}
