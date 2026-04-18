package health

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"loadbalancer/balancer"
	"loadbalancer/config"
)

// Checker runs background health checks for a set of backends.
type Checker struct {
	pool     string
	cfg      config.HealthCheckConfig
	backends []*balancer.Backend
	stop     chan struct{}
}

// New creates a Checker but does not start it yet.
func New(poolName string, cfg config.HealthCheckConfig, backends []*balancer.Backend) *Checker {
	return &Checker{
		pool:     poolName,
		cfg:      cfg,
		backends: backends,
		stop:     make(chan struct{}),
	}
}

// Start launches a goroutine per backend that periodically checks its health.
func (c *Checker) Start() {
	if !c.cfg.Enabled {
		// Mark all backends healthy if health checks are disabled.
		for _, b := range c.backends {
			b.SetHealthy(true)
		}
		return
	}

	for _, b := range c.backends {
		b.SetHealthy(true) // Optimistic initial state.
		go c.runLoop(b)
	}
}

// Stop signals all health-check goroutines to exit.
func (c *Checker) Stop() {
	close(c.stop)
}

func (c *Checker) runLoop(b *balancer.Backend) {
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			healthy := c.check(b)
			prev := b.IsHealthy()
			b.SetHealthy(healthy)
			if prev != healthy {
				state := "unhealthy"
				if healthy {
					state = "healthy"
				}
				slog.Info("backend health changed",
					"pool", c.pool,
					"backend", b.Address,
					"state", state)
			}
		}
	}
}

func (c *Checker) check(b *balancer.Backend) bool {
	if c.cfg.Path != "" {
		return c.httpCheck(b)
	}
	return c.tcpCheck(b)
}

func (c *Checker) httpCheck(b *balancer.Backend) bool {
	url := fmt.Sprintf("http://%s%s", b.Address, c.cfg.Path)
	client := &http.Client{Timeout: c.cfg.Timeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (c *Checker) tcpCheck(b *balancer.Backend) bool {
	conn, err := net.DialTimeout("tcp", b.Address, c.cfg.Timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
