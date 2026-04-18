package config

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// GlobalConfig holds global settings.
type GlobalConfig struct {
	Bind     string `yaml:"bind"`
	LogLevel string `yaml:"log_level"`
}

// TLSConfig holds TLS certificate configuration.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Frontend describes a listening port and its protocol.
type Frontend struct {
	Name     string     `yaml:"name"`
	Protocol string     `yaml:"protocol"` // http, https, tcp, udp, grpc
	Port     int        `yaml:"port"`
	TLS      *TLSConfig `yaml:"tls,omitempty"`
	Pool     string     `yaml:"backend_pool"`
}

// HealthCheckConfig controls backend health checking.
type HealthCheckConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	Path     string        `yaml:"path"` // empty = TCP check
}

// RateLimitConfig configures per-IP rate limiting.
type RateLimitConfig struct {
	Enabled           bool    `yaml:"enabled"`
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst             int     `yaml:"burst"`
}

// BackendConfig describes a single backend server.
type BackendConfig struct {
	Address string `yaml:"address"`
	Weight  int    `yaml:"weight"`
}

// BackendPool groups backends under a load balancing algorithm.
type BackendPool struct {
	Name        string            `yaml:"name"`
	Algorithm   string            `yaml:"algorithm"` // round_robin, least_connections, weighted, consistent_hash
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	RateLimit   RateLimitConfig   `yaml:"rate_limit"`
	Backends    []BackendConfig   `yaml:"backends"`
}

// DashboardConfig controls the web dashboard.
type DashboardConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

// MetricsConfig controls Prometheus metrics exposition.
type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

// Config is the root configuration structure.
type Config struct {
	Global    GlobalConfig    `yaml:"global"`
	Frontends []Frontend      `yaml:"frontends"`
	Pools     []BackendPool   `yaml:"backend_pools"`
	Dashboard DashboardConfig `yaml:"dashboard"`
	Metrics   MetricsConfig   `yaml:"metrics"`
}

// Loader loads and watches a YAML config file.
type Loader struct {
	mu        sync.RWMutex
	path      string
	current   *Config
	callbacks []func(*Config)
}

// NewLoader reads the config file at path and returns a Loader.
func NewLoader(path string) (*Loader, error) {
	l := &Loader{path: path}
	cfg, err := l.load()
	if err != nil {
		return nil, err
	}
	l.current = cfg
	return l, nil
}

// Get returns the current config (safe for concurrent use).
func (l *Loader) Get() *Config {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.current
}

// OnChange registers a callback that is called whenever the config is reloaded.
func (l *Loader) OnChange(fn func(*Config)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.callbacks = append(l.callbacks, fn)
}

// Watch starts an fsnotify watcher and reloads config on file changes.
// It blocks until the watcher encounters an unrecoverable error; call in a goroutine.
func (l *Loader) Watch() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(l.path); err != nil {
		return fmt.Errorf("watch %s: %w", l.path, err)
	}

	slog.Info("config watcher started", "file", l.path)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				slog.Info("config file changed, reloading", "file", l.path)
				cfg, err := l.load()
				if err != nil {
					slog.Error("config reload failed", "err", err)
					continue
				}
				l.mu.Lock()
				l.current = cfg
				cbs := make([]func(*Config), len(l.callbacks))
				copy(cbs, l.callbacks)
				l.mu.Unlock()

				for _, cb := range cbs {
					cb(cfg)
				}
				slog.Info("config reloaded successfully")
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Error("watcher error", "err", err)
		}
	}
}

func (l *Loader) load() (*Config, error) {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults.
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		if pool.HealthCheck.Interval == 0 {
			pool.HealthCheck.Interval = 10 * time.Second
		}
		if pool.HealthCheck.Timeout == 0 {
			pool.HealthCheck.Timeout = 2 * time.Second
		}
		for j := range pool.Backends {
			if pool.Backends[j].Weight == 0 {
				pool.Backends[j].Weight = 1
			}
		}
		if pool.RateLimit.Burst == 0 && pool.RateLimit.RequestsPerSecond > 0 {
			pool.RateLimit.Burst = int(pool.RateLimit.RequestsPerSecond * 2)
		}
	}
	if cfg.Dashboard.Port == 0 {
		cfg.Dashboard.Port = 9000
	}
	if cfg.Metrics.Port == 0 {
		cfg.Metrics.Port = 9001
	}
	if cfg.Global.Bind == "" {
		cfg.Global.Bind = "0.0.0.0"
	}

	return &cfg, nil
}
