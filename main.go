package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"loadbalancer/balancer"
	"loadbalancer/config"
	"loadbalancer/dashboard"
	"loadbalancer/health"
	"loadbalancer/metrics"
	"loadbalancer/proxy"
	"loadbalancer/ratelimit"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	// ── Logging ──────────────────────────────────────────────────────────────
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// ── Config ───────────────────────────────────────────────────────────────
	loader, err := config.NewLoader(*configPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	cfg := loader.Get()
	slog.Info("config loaded", "file", *configPath)

	// ── Build backend pools ───────────────────────────────────────────────────
	type poolEntry struct {
		pool     config.BackendPool
		backends []*balancer.Backend
		bal      *balancer.Ref // hot-swappable; frontends hold this pointer
		checker  *health.Checker
		limiter  *ratelimit.Limiter
	}

	poolMap := make(map[string]*poolEntry)

	buildPools := func(cfg *config.Config) {
		for _, poolCfg := range cfg.Pools {
			backends := make([]*balancer.Backend, 0, len(poolCfg.Backends))
			for _, bc := range poolCfg.Backends {
				backends = append(backends, &balancer.Backend{
					Address: bc.Address,
					Weight:  bc.Weight,
				})
			}

			bal, err := balancer.New(poolCfg.Algorithm, backends)
			if err != nil {
				slog.Error("create balancer", "pool", poolCfg.Name, "err", err)
				continue
			}

			checker := health.New(poolCfg.Name, poolCfg.HealthCheck, backends)
			checker.Start()

			var limiter *ratelimit.Limiter
			if poolCfg.RateLimit.Enabled {
				limiter = ratelimit.New(
					poolCfg.RateLimit.RequestsPerSecond,
					poolCfg.RateLimit.Burst,
				)
			}

			if existing, ok := poolMap[poolCfg.Name]; ok {
				// Hot-swap: stop the old checker and atomically replace the
				// inner balancer. Running frontends and the dashboard hold *Ref
				// so they immediately route to the new backends.
				existing.checker.Stop()
				existing.bal.Swap(bal)
				existing.checker = checker
				existing.pool = poolCfg
				existing.backends = backends
			} else {
				poolMap[poolCfg.Name] = &poolEntry{
					pool:     poolCfg,
					backends: backends,
					bal:      balancer.NewRef(bal),
					checker:  checker,
					limiter:  limiter,
				}
			}
		}
	}

	buildPools(cfg)

	// ── Metrics store ─────────────────────────────────────────────────────────
	metricStore := metrics.NewStore()

	// ── Dashboard ─────────────────────────────────────────────────────────────
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	bindCtx := proxy.WithBind(rootCtx, cfg.Global.Bind)

	buildDashPools := func() []dashboard.Pool {
		pools := make([]dashboard.Pool, 0, len(poolMap))
		for name, entry := range poolMap {
			pools = append(pools, dashboard.Pool{
				Name:      name,
				Algorithm: entry.pool.Algorithm,
				Balancer:  entry.bal,
			})
		}
		return pools
	}

	var ds *dashboard.Server
	if cfg.Dashboard.Enabled {
		ds = dashboard.NewServer(cfg.Dashboard.Port, metricStore, buildDashPools(), *configPath)
		if err := ds.Start(bindCtx); err != nil {
			slog.Error("dashboard start failed", "err", err)
		}
		defer ds.Shutdown(context.Background())
	}

	// ── Prometheus metrics ────────────────────────────────────────────────────
	if cfg.Metrics.Enabled {
		metricsAddr := fmt.Sprintf(":%d", cfg.Metrics.Port)
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
		metricsServer := &http.Server{Addr: metricsAddr, Handler: metricsMux}
		go func() {
			slog.Info("metrics server listening", "addr", metricsAddr)
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server error", "err", err)
			}
		}()
		defer metricsServer.Shutdown(context.Background()) //nolint:errcheck
	}

	// ── Start frontends ───────────────────────────────────────────────────────
	var tcpFrontends []*proxy.TCPFrontend
	var httpFrontends []*proxy.HTTPFrontend
	var udpFrontends []*proxy.UDPFrontend

	startFrontends := func(cfg *config.Config) {
		for _, fe := range cfg.Frontends {
			entry, ok := poolMap[fe.Pool]
			if !ok {
				slog.Error("frontend references unknown pool",
					"frontend", fe.Name, "pool", fe.Pool)
				continue
			}

			switch fe.Protocol {
			case "http", "https", "grpc":
				f := proxy.NewHTTPFrontend(fe, entry.bal, entry.limiter, metricStore)
				if err := f.Start(bindCtx); err != nil {
					slog.Error("start HTTP frontend", "name", fe.Name, "err", err)
					continue
				}
				httpFrontends = append(httpFrontends, f)

			case "tcp":
				f := proxy.NewTCPFrontend(fe, entry.bal, entry.limiter, metricStore)
				if err := f.Start(bindCtx); err != nil {
					slog.Error("start TCP frontend", "name", fe.Name, "err", err)
					continue
				}
				tcpFrontends = append(tcpFrontends, f)

			case "udp":
				f := proxy.NewUDPFrontend(fe, entry.bal, entry.limiter, metricStore)
				if err := f.Start(bindCtx); err != nil {
					slog.Error("start UDP frontend", "name", fe.Name, "err", err)
					continue
				}
				udpFrontends = append(udpFrontends, f)

			default:
				slog.Error("unknown protocol", "frontend", fe.Name, "protocol", fe.Protocol)
			}
		}
	}

	startFrontends(cfg)

	// ── Config hot-reload ─────────────────────────────────────────────────────
	loader.OnChange(func(newCfg *config.Config) {
		slog.Info("applying hot-reloaded config")
		buildPools(newCfg)
		if ds != nil {
			ds.SetPools(buildDashPools())
			ds.NotifyReload()
		}
	})

	go func() {
		if err := loader.Watch(); err != nil {
			slog.Error("config watcher exited", "err", err)
		}
	}()

	// ── Startup summary ───────────────────────────────────────────────────────
	slog.Info("load balancer started",
		"frontends", len(cfg.Frontends),
		"pools", len(cfg.Pools),
	)

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutdown signal received, draining connections (max 30s)…")
	rootCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	for _, f := range httpFrontends {
		f.Shutdown(shutdownCtx)
	}
	for _, f := range tcpFrontends {
		f.Shutdown()
	}
	for _, f := range udpFrontends {
		f.Shutdown()
	}

	for _, entry := range poolMap {
		entry.checker.Stop()
		if entry.limiter != nil {
			entry.limiter.Stop()
		}
	}

	slog.Info("load balancer stopped")
}
