# Blacklight

A **Go** tool that discovers services and workloads from your **Kubernetes** cluster, monitors **live TCP traffic** between them, and renders an interactive **service map**. Helps you understand what talks to what, spot dead services ripe for removal, and navigate complex multi-service setups.

## What it does

- **Discovers** from the Kubernetes API:
  - Deployments, StatefulSets, DaemonSets, CronJobs, Jobs
  - Services and their workload selectors
  - Ingress resources and Traefik IngressRoute CRDs
  - Env-based references (`*_HOST`, `*_URL`, `*_SERVICE`, `*_ADDR`, `*_DB`, etc.)
- **Monitors live traffic** by reading `/proc/net/tcp` from pods to discover active TCP connections — creates graph edges automatically from observed traffic
- **Highlights dead services** — nodes with zero observed traffic are flagged for potential removal
- **Persists state** — layout positions and traffic-discovered edges survive restarts (SQLite)
- **Optional source scanning** — walks local source trees to find URL-based service references

## Output formats

| Format   | Description |
|----------|-------------|
| `web`    | Interactive web UI with live-updating graph, traffic overlays, and dead-service filter (default) |
| `mermaid`| Mermaid `flowchart LR` diagram — paste into docs, GitHub, or [Mermaid Live](https://mermaid.live) |
| `json`   | Full graph as JSON (`nodes` + `edges`) for scripting |

## Quick start

### Run locally (uses your kubeconfig)

```bash
make build
./blacklight -output web
# open http://localhost:8080
```

### Run inside Kubernetes

```bash
# Deploy with included manifests
kubectl apply -f deploy/

# Or build and push your own image
make docker
docker tag blacklight:dev ghcr.io/yourname/blacklight:latest
docker push ghcr.io/yourname/blacklight:latest
```

The included manifests create a `blacklight` namespace with appropriate RBAC (read-only cluster access + pod exec for traffic scanning).

## Usage

```bash
# All namespaces — web UI (default)
./blacklight

# Single namespace
./blacklight -namespace my-app

# Mermaid diagram to stdout
./blacklight -output mermaid

# JSON graph
./blacklight -output json

# Custom port
./blacklight -port 9090

# With source code scanning
./blacklight -source /path/to/repo

# Custom data directory
./blacklight -data-dir /tmp/blacklight-data

# Print version
./blacklight -version
```

## Configuration

Blacklight reads optional config from `.blacklight.yaml` in the current directory or `~/.config/blacklight/config.yaml`:

```yaml
namespace: my-app          # default namespace filter
source: ../my-monorepo     # source code path for static scanning
data_dir: /data            # persistence directory
```

CLI flags override config file values.

## Web UI features

- **Live graph** — nodes and edges update in real-time via SSE
- **Traffic density** — edge thickness scales with connection count (log-scale)
- **Dead service filter** — toggle to highlight services with zero traffic
- **Draggable layout** — positions persist across page reloads and restarts
- **Auto-arrange** — toggle deterministic force-directed layout (fcose)
- **Node details** — hover to see inbound/outbound traffic with peer names and connection counts

## Edge kinds

| Kind | Description |
|------|-------------|
| `selector` | Service selects a workload via label matching |
| `ingress_backend` | Ingress/IngressRoute routes to a Service |
| `env_ref` | Workload env var references another Service |
| `traffic` | Observed TCP connection (auto-discovered) |
| `code_ref` | URL reference found in source code |

## Annotations

Add purpose descriptions to your resources for a more readable map:

```yaml
metadata:
  annotations:
    blacklight/purpose: "User-facing API"
    # Also reads: servicemap/purpose, purpose, description, app.kubernetes.io/part-of
```

## Requirements

- Go 1.21+ with CGO enabled (for SQLite)
- Access to a Kubernetes cluster (kubeconfig or in-cluster)
- Pods must have `/proc/net/tcp` readable for traffic scanning (most containers do)

## License

MIT
