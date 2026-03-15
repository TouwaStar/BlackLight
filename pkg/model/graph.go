package model

// Node represents a service, workload, or entry point in the map.
type Node struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"` // "Service", "Deployment", "StatefulSet", "Ingress", "Pod"
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	DisplayName string            `json:"display_name"` // e.g. "namespace/name"
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Purpose     string            `json:"purpose,omitempty"` // inferred or from annotation
	Meta        map[string]any    `json:"meta,omitempty"`    // extra details: replicas, images, ports, etc.
}

// Edge represents a directed relationship (who talks to whom / who uses whom).
type Edge struct {
	From   string `json:"from"`   // node ID
	To     string `json:"to"`     // node ID
	Kind   string `json:"kind"`   // "routes_to", "selector", "env_ref", "ingress_backend"
	Detail string `json:"detail"` // optional: port, path, env var name
}

// Graph is the full service map.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// TrafficConnection represents an observed TCP connection between a workload and a target.
type TrafficConnection struct {
	SourceWorkload string            `json:"source_workload"` // node ID
	TargetService  string            `json:"target_service"`  // node ID
	ConnCount      int               `json:"conn_count"`
	ErrorCount     int               `json:"error_count"`            // SYN_SENT + CLOSE_WAIT connections
	Ports          []PortCount       `json:"ports,omitempty"`        // connections grouped by remote port
	Pods           []PodTraffic      `json:"pods,omitempty"`         // per-pod breakdown
	States         *StateBreakdown   `json:"states,omitempty"`       // TCP state counts
}

// PortCount groups connections by remote port.
type PortCount struct {
	Port     uint16 `json:"port"`
	Conns    int    `json:"conns"`
	Errors   int    `json:"errors"`
	Protocol string `json:"protocol,omitempty"` // inferred protocol name (e.g. "HTTP", "gRPC", "PostgreSQL")
}

// PortProtocol returns the likely protocol name for a well-known port.
// Returns empty string for unknown ports.
func PortProtocol(port uint16) string {
	switch port {
	case 80, 8080, 8000, 8888, 3000, 5000, 9090:
		return "HTTP"
	case 443, 8443:
		return "HTTPS"
	case 50051:
		return "gRPC"
	case 5432:
		return "PostgreSQL"
	case 3306:
		return "MySQL"
	case 6379:
		return "Redis"
	case 27017:
		return "MongoDB"
	case 9092:
		return "Kafka"
	case 4222:
		return "NATS"
	case 2379, 2380:
		return "etcd"
	case 53:
		return "DNS"
	case 5672, 5671:
		return "AMQP"
	case 9200, 9300:
		return "Elasticsearch"
	case 11211:
		return "Memcached"
	case 6443:
		return "Kubernetes API"
	case 4317, 4318:
		return "OpenTelemetry"
	default:
		return ""
	}
}

// PodTraffic shows per-pod connection details.
type PodTraffic struct {
	Pod    string `json:"pod"`
	Conns  int    `json:"conns"`
	Errors int    `json:"errors"`
}

// StateBreakdown shows connections by TCP state.
type StateBreakdown struct {
	Established int `json:"established"`
	TimeWait    int `json:"time_wait"`
	SynSent     int `json:"syn_sent"`
	CloseWait   int `json:"close_wait"`
}

// ExternalTraffic records inbound connections from outside the cluster to a workload.
type ExternalTraffic struct {
	NodeID    string   `json:"node_id"`    // workload node ID
	ConnCount int      `json:"conn_count"` // number of external inbound connections
	TopIPs    []IPCount `json:"top_ips,omitempty"` // top source IPs (for debugging)
}

// IPCount is an IP address with a connection count.
type IPCount struct {
	IP    string `json:"ip"`
	Count int    `json:"count"`
}

// TrafficSnapshot holds a point-in-time view of all observed TCP connections.
type TrafficSnapshot struct {
	Timestamp   int64               `json:"timestamp"` // unix millis
	Connections []TrafficConnection `json:"connections"`
	External    []ExternalTraffic   `json:"external,omitempty"` // workloads receiving external traffic
	Cloud       []ExternalTraffic   `json:"cloud,omitempty"`    // connections to/from cloud infra (AWS, etc.)
}

// GraphEqual returns true if two graphs have the same node IDs and edge keys.
func GraphEqual(a, b *Graph) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Nodes) != len(b.Nodes) || len(a.Edges) != len(b.Edges) {
		return false
	}
	nodes := make(map[string]bool, len(a.Nodes))
	for _, n := range a.Nodes {
		nodes[n.ID] = true
	}
	for _, n := range b.Nodes {
		if !nodes[n.ID] {
			return false
		}
	}
	edges := make(map[string]bool, len(a.Edges))
	for _, e := range a.Edges {
		edges[e.From+"\t"+e.To+"\t"+e.Kind] = true
	}
	for _, e := range b.Edges {
		if !edges[e.From+"\t"+e.To+"\t"+e.Kind] {
			return false
		}
	}
	return true
}

// Collapse merges Service+Workload pairs connected by 1:1 selector edges into single nodes.
// This removes the redundant Service→Deployment duplication and keeps only the workload node.
// Returns the collapsed graph and a mapping of removed Service IDs to their Workload IDs.
func Collapse(g *Graph) (*Graph, map[string]string) {
	// Find selector edges: Service → Workload
	svcToWorkloads := make(map[string][]string)
	workloadToSvcs := make(map[string][]string)
	for _, e := range g.Edges {
		if e.Kind == "selector" {
			svcToWorkloads[e.From] = append(svcToWorkloads[e.From], e.To)
			workloadToSvcs[e.To] = append(workloadToSvcs[e.To], e.From)
		}
	}

	// Only collapse 1:1 pairs (one service selects exactly one workload and vice versa).
	idMap := make(map[string]string)
	removeSvc := make(map[string]bool)
	for svcID, workloads := range svcToWorkloads {
		if len(workloads) != 1 {
			continue
		}
		wID := workloads[0]
		if len(workloadToSvcs[wID]) != 1 {
			continue
		}
		idMap[svcID] = wID
		removeSvc[svcID] = true
	}

	collapsed := &Graph{}

	// Copy nodes, skipping collapsed Services.
	for _, n := range g.Nodes {
		if removeSvc[n.ID] {
			continue
		}
		collapsed.Nodes = append(collapsed.Nodes, n)
	}

	// Copy edges, rerouting Service IDs to Workload IDs and deduplicating.
	edgeSeen := make(map[string]bool)
	for _, e := range g.Edges {
		if e.Kind == "selector" && removeSvc[e.From] {
			continue
		}
		from := e.From
		to := e.To
		if mapped, ok := idMap[from]; ok {
			from = mapped
		}
		if mapped, ok := idMap[to]; ok {
			to = mapped
		}
		if from == to {
			continue
		}
		key := from + "\t" + to + "\t" + e.Kind
		if edgeSeen[key] {
			continue
		}
		edgeSeen[key] = true
		collapsed.Edges = append(collapsed.Edges, Edge{
			From: from, To: to, Kind: e.Kind, Detail: e.Detail,
		})
	}

	return collapsed, idMap
}

// NodeID returns a unique ID for a node (namespace/kind/name).
func NodeID(kind, namespace, name string) string {
	return namespace + "/" + kind + "/" + name
}
