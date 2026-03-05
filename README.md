# Blacklight

A **Go** tool that discovers services and workloads from your **Kubernetes** cluster, monitors **live TCP traffic** between them, and renders an interactive **service map**. Helps you understand what talks to what, spot dead services ripe for removal, detect error-state connections, and navigate complex multi-service setups.

## What it does

- **Discovers** from the Kubernetes API:
  - Deployments, StatefulSets, DaemonSets, CronJobs, Jobs
  - Services and their workload selectors
  - Ingress resources and Traefik IngressRoute CRDs
  - Env-based references (`*_HOST`, `*_URL`, `*_SERVICE`, `*_ADDR`, `*_DB`, etc.)
- **Monitors live traffic** by reading `/proc/net/tcp` from pods to discover active TCP connections — creates graph edges automatically from observed traffic
- **Error detection** — flags connections in SYN_SENT or CLOSE_WAIT state as errors, shown as dashed red edges
- **Cloud traffic classification** — identifies connections to AWS, Azure, and GCP infrastructure via reverse DNS, shown separately from external user traffic
- **Highlights dead services** — nodes with zero observed traffic are flagged for potential removal
- **Context & namespace switching** — switch Kubernetes contexts and namespaces from the web UI without restarting
- **Persists state** — layout positions, traffic-discovered edges, and node statistics survive restarts (SQLite)
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
- **Error edges** — dashed red lines for connections in error states (SYN_SENT / CLOSE_WAIT)
- **Dead service filter** — toggle to highlight services with zero traffic
- **Search** — filter nodes by name or label in real-time
- **Context & namespace selectors** — switch cluster context or namespace without restarting
- **Draggable layout** — positions persist across page reloads and restarts
- **Auto-arrange** — toggle deterministic force-directed layout (fcose)
- **Node details** — hover to see kind, purpose, first seen, last traffic, lifetime connections, inbound/outbound peers with counts, external/cloud traffic
- **Export** — download the current graph as JSON or Mermaid diagram from the UI

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

## API

The web UI is backed by a REST + SSE API:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/graph` | GET | Full graph (nodes + edges) as JSON |
| `/api/mermaid` | GET | Mermaid flowchart diagram |
| `/api/stats` | GET | Node statistics (first seen, last traffic, lifetime connections) |
| `/api/history` | GET | Time-series connection history for an edge (`?source=...&target=...&range=24h`) |
| `/api/positions` | GET/POST | Load or save node layout positions |
| `/api/contexts` | GET | Available kubeconfig contexts |
| `/api/namespaces` | GET | Available namespaces in current context |
| `/api/config` | POST | Switch context and/or namespace (`{"context":"...","namespace":"..."}`) |
| `/api/events` | GET | SSE stream — pushes `graph` and `traffic` events in real-time |

## Requirements

### Build

- **Go 1.21+** with **CGO enabled** (required for SQLite via `go-sqlite3`)
- A C compiler (gcc/clang) — needed by CGO

### Running locally

- **kubectl configured** — Blacklight reads your kubeconfig file (`~/.kube/config` or `$KUBECONFIG`) to connect to clusters. This file is created when you configure kubectl for a cluster, e.g.:
  - AWS EKS: `aws eks update-kubeconfig --name my-cluster`
  - GCP GKE: `gcloud container clusters get-credentials my-cluster`
  - Azure AKS: `az aks get-credentials --resource-group myRG --name my-cluster`
  - minikube: `minikube start`
- Your kubeconfig user needs the following **RBAC permissions**:
  - `get`, `list` on: namespaces, pods, services, configmaps, deployments, statefulsets, daemonsets, replicasets, jobs, cronjobs, ingresses, ingressroutes
  - `create` on `pods/exec` (for traffic scanning via `/proc/net/tcp`)

### Running in-cluster

- Deploy with `kubectl apply -f deploy/` — the included manifests set up a ServiceAccount with the required ClusterRole
- No kubeconfig needed; the pod uses its service account token automatically

### Traffic scanning

- Pods must have `/proc/net/tcp` readable (most containers do)
- The scanned containers need `cat` available (standard in most base images)

## License

MIT
