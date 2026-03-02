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
	SourceWorkload string `json:"source_workload"` // node ID
	TargetService  string `json:"target_service"`  // node ID
	ConnCount      int    `json:"conn_count"`
	ErrorCount     int    `json:"error_count"` // SYN_SENT + CLOSE_WAIT connections
}

// TrafficSnapshot holds a point-in-time view of all observed TCP connections.
type TrafficSnapshot struct {
	Timestamp   int64               `json:"timestamp"` // unix millis
	Connections []TrafficConnection `json:"connections"`
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
