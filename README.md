# Load Balancer

A multi-protocol load balancer written in Go with real-time dashboard, Prometheus metrics, health checks, and rate limiting.

## What it can do

- **Protocols** — HTTP, HTTPS (TLS termination), TCP, UDP, gRPC (HTTP/2)
- **Algorithms** — Round Robin, Least Connections, Weighted, Consistent Hash
- **Health checks** — HTTP (2xx) or TCP dial, configurable interval and timeout per pool
- **Rate limiting** — per-client-IP token bucket with burst capacity
- **Prometheus metrics** — counters and gauges at `:9001/metrics`
- **Live dashboard** — browser UI at `:9000`, updates every second via WebSocket
- **Config hot-reload** — edit `config.yaml`, changes apply instantly (no restart)
- **Graceful shutdown** — 30-second drain window on SIGINT/SIGTERM

---

## Load Balancing Algorithms

| Algorithm | Config value | Best for |
|---|---|---|
| Round Robin | `round_robin` | Equal-capacity backends, stateless services |
| Least Connections | `least_connections` | Long-lived connections (databases, WebSockets) |
| Weighted | `weighted` | Mixed-capacity backends (set `weight` per backend) |
| Consistent Hash | `consistent_hash` | Session stickiness by client IP |

---

## Configuration

```yaml
global:
  bind: "0.0.0.0"
  log_level: "info"       # debug | info | warn | error

frontends:
  - name: http
    protocol: http         # http | https | tcp | udp | grpc
    port: 8080
    backend_pool: web-backends
    tls:                   # required for https and grpc with TLS
      cert_file: cert.pem
      key_file:  key.pem

backend_pools:
  - name: web-backends
    algorithm: round_robin
    health_check:
      enabled: true
      interval: 10s
      timeout: 2s
      path: /health        # omit for TCP dial check
    rate_limit:
      enabled: true
      requests_per_second: 1000
      burst: 2000
    backends:
      - address: "127.0.0.1:8001"
        weight: 1
      - address: "127.0.0.1:8002"
        weight: 2          # gets twice the traffic in weighted mode

dashboard:
  enabled: true
  port: 9000

metrics:
  enabled: true
  port: 9001
```

### Hot-reload

Edit `config.yaml` while the process is running — backend pool changes (addresses, weights, algorithms, health check settings, rate limits) apply within a second. Adding or removing frontends requires a restart.

---

## Running

### 1. Start fake backends

```bash
go run ./testapp
```

Starts three HTTP servers on `:8001`, `:8002`, `:8003` with different simulated latencies:

| Port | Profile | Latency |
|---|---|---|
| `:8001` | fast | 1–10 ms |
| `:8002` | normal | 20–80 ms |
| `:8003` | slow | 100–300 ms |

Custom ports:

```bash
go run ./testapp -ports 8001,8002,8003,8004
```

### 2. Start the load balancer

```bash
go run . -config config.yaml
```

Or build first:

```bash
go build -o loadbalancer .
./loadbalancer -config config.yaml
```

### 3. Open the dashboard

```
http://localhost:9000
```

Shows live active connections, requests/sec, errors, and per-backend health status.

---

## Load Testing

The `testapp` has a built-in load generator:

```bash
go run ./testapp -mode load -rps 500
```

| Flag | Default | Description |
|---|---|---|
| `-rps` | `50` | Target requests per second |
| `-target` | `http://localhost:8080` | Load balancer URL |
| `-duration` | forever | How long to run, e.g. `30s`, `5m` |

The generator prints a summary every 5 seconds:

```
[load] last 5s — ok: 2500  err: 0  avg_latency: 23ms
```

### Full local workflow

```bash
# Terminal 1 — fake backends
go run ./testapp

# Terminal 2 — load balancer
go run . -config config.yaml

# Terminal 3 — generate load
go run ./testapp -mode load -rps 500

# Browser — live dashboard
open http://localhost:9000

# Prometheus metrics
curl http://localhost:9001/metrics
```

---

## Running Tests

```bash
go test ./...
```

Specific packages:

```bash
go test ./balancer/
go test ./ratelimit/
go test ./health/
```

Verbose output:

```bash
go test -v ./...
```

---

## Project Structure

```
.
├── main.go           # Entry point, wires everything together
├── config/           # Config parsing and hot-reload (fsnotify)
├── balancer/         # Load balancing algorithms
├── proxy/            # HTTP, TCP, UDP frontend handlers
├── health/           # Health checker
├── ratelimit/        # Per-IP token bucket limiter
├── metrics/          # Prometheus + in-process stats store
├── dashboard/        # WebSocket dashboard server
└── testapp/          # Fake backends + load generator
```
