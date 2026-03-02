package render

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/TouwaStar/BlackLight/pkg/model"
)

// Mermaid outputs a Mermaid flowchart (flowchart LR or TB) for the graph.
func Mermaid(g *model.Graph, direction string) string {
	if direction == "" {
		direction = "LR"
	}
	var b strings.Builder
	b.WriteString("flowchart " + direction + "\n")
	nodeIDs := make(map[string]bool)
	for _, e := range g.Edges {
		nodeIDs[e.From] = true
		nodeIDs[e.To] = true
	}
	for _, n := range g.Nodes {
		if !nodeIDs[n.ID] {
			continue
		}
		safeID := mermaidSafeID(n.ID)
		label := n.DisplayName
		if n.Purpose != "" {
			label = label + "\\n(" + n.Purpose + ")"
		}
		label = strings.ReplaceAll(label, "\"", "#quot;")
		var shape, closeShape string
		switch n.Kind {
		case "Service":
			shape, closeShape = "[[", "]]"
		case "Deployment", "StatefulSet", "DaemonSet":
			shape, closeShape = "[", "]"
		case "CronJob":
			shape, closeShape = "{{", "}}"
		case "Job":
			shape, closeShape = "([", "])"
		default:
			shape, closeShape = "([", "])"
		}
		b.WriteString(fmt.Sprintf("    %s%s\"%s\"%s\n", safeID, shape, label, closeShape))
	}
	seenEdges := make(map[string]bool)
	for _, e := range g.Edges {
		key := e.From + "\t" + e.To + "\t" + e.Kind + "\t" + e.Detail
		if seenEdges[key] {
			continue
		}
		seenEdges[key] = true
		from := mermaidSafeID(e.From)
		to := mermaidSafeID(e.To)
		label := e.Kind
		if e.Detail != "" {
			label = label + ": " + e.Detail
		}
		label = strings.ReplaceAll(label, "\"", "#quot;")
		b.WriteString(fmt.Sprintf("    %s -->|%s| %s\n", from, label, to))
	}
	return b.String()
}

func mermaidSafeID(id string) string {
	return strings.ReplaceAll(strings.ReplaceAll(id, "/", "_"), " ", "_")
}

// JSON returns the graph as indented JSON.
func JSON(g *model.Graph) ([]byte, error) {
	return json.MarshalIndent(g, "", "  ")
}
