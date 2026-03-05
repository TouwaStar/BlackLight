package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/TouwaStar/BlackLight/pkg/discovery"
	"github.com/TouwaStar/BlackLight/pkg/model"
	"github.com/TouwaStar/BlackLight/pkg/store"
	"github.com/TouwaStar/BlackLight/pkg/traffic"
)

// Event is sent to SSE subscribers.
type Event struct {
	Type string      // "graph" or "traffic"
	Data any
}

const (
	trafficWindow     = 5 * time.Minute
	discoveryInterval = 30 * time.Second
	trafficInterval   = 12 * time.Second
)

// Manager runs periodic discovery and traffic scanning in the background.
type Manager struct {
	discoverer  *discovery.Discoverer
	scanner     *traffic.Scanner
	store       *store.Store
	sourcePath  string
	kubeContext string // current kubeconfig context (empty = default)
	namespace   string // current namespace filter (empty = all)

	mu           sync.RWMutex
	graph        *model.Graph
	traffic      *model.TrafficSnapshot
	idMap        map[string]string // collapsed Service ID → Workload ID
	recentScans  []timedSnapshot   // rolling window of recent scans
	trafficEdges map[string]model.Edge // edges discovered from traffic (key: "from\tto")

	subsMu sync.Mutex
	subs   map[chan Event]struct{}
}

type timedSnapshot struct {
	time time.Time
	snap *model.TrafficSnapshot
}

func NewManager(d *discovery.Discoverer, st *store.Store, sourcePath string) *Manager {
	return &Manager{
		discoverer:   d,
		scanner:      traffic.NewScanner(d.Clientset(), d.RestConfig(), d.Namespace()),
		store:        st,
		sourcePath:   sourcePath,
		namespace:    d.Namespace(),
		trafficEdges: make(map[string]model.Edge),
		subs:         make(map[chan Event]struct{}),
	}
}

func (m *Manager) Store() *store.Store   { return m.store }
func (m *Manager) KubeContext() string   { m.mu.RLock(); defer m.mu.RUnlock(); return m.kubeContext }
func (m *Manager) Namespace() string     { m.mu.RLock(); defer m.mu.RUnlock(); return m.namespace }
func (m *Manager) Discoverer() *discovery.Discoverer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.discoverer
}

// Reconfigure switches the cluster context and/or namespace, re-initializes
// discovery and traffic scanning, and broadcasts the new graph.
func (m *Manager) Reconfigure(ctx context.Context, kubeContext, namespace string) error {
	d, err := discovery.NewDiscovererWithContext(kubeContext, namespace)
	if err != nil {
		return err
	}
	g, err := d.Discover(ctx)
	if err != nil {
		return err
	}
	if m.sourcePath != "" {
		sourceEdges, err := discovery.ScanSource(m.sourcePath)
		if err != nil {
			log.Printf("source scan on reconfigure: %v", err)
		} else {
			discovery.MergeSourceEdges(g, sourceEdges)
		}
	}
	collapsed, idMap := model.Collapse(g)
	scanner := traffic.NewScanner(d.Clientset(), d.RestConfig(), d.Namespace())

	m.mu.Lock()
	m.discoverer = d
	m.scanner = scanner
	m.kubeContext = kubeContext
	m.namespace = namespace
	m.graph = collapsed
	m.idMap = idMap
	m.traffic = nil
	m.recentScans = nil
	m.trafficEdges = make(map[string]model.Edge)
	m.restoreTrafficEdgesLocked()
	m.mu.Unlock()

	if m.store != nil {
		if err := m.store.RecordNodes(collapsed.Nodes); err != nil {
			log.Printf("store record nodes on reconfigure: %v", err)
		}
	}
	m.broadcast(Event{Type: "graph", Data: collapsed})
	return nil
}

func (m *Manager) SetInitialGraph(g *model.Graph) {
	collapsed, idMap := model.Collapse(g)
	m.mu.Lock()
	m.graph = collapsed
	m.idMap = idMap
	m.restoreTrafficEdgesLocked()
	m.mu.Unlock()
	if m.store != nil {
		if err := m.store.RecordNodes(collapsed.Nodes); err != nil {
			log.Printf("store record initial nodes: %v", err)
		}
	}
}

// restoreTrafficEdgesLocked loads persisted traffic edges from the store and
// merges them into the current graph. Must be called while holding m.mu.
func (m *Manager) restoreTrafficEdgesLocked() {
	if m.store == nil {
		return
	}
	edges, err := m.store.LoadTrafficEdges()
	if err != nil {
		log.Printf("store load traffic edges: %v", err)
		return
	}
	for _, e := range edges {
		m.trafficEdges[e.From+"\t"+e.To] = e
	}
	m.mergeTrafficEdgesLocked()
}

func (m *Manager) Graph() *model.Graph {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.graph
}

func (m *Manager) Traffic() *model.TrafficSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.traffic
}

func (m *Manager) Subscribe() chan Event {
	ch := make(chan Event, 16)
	m.subsMu.Lock()
	m.subs[ch] = struct{}{}
	m.subsMu.Unlock()
	return ch
}

func (m *Manager) Unsubscribe(ch chan Event) {
	m.subsMu.Lock()
	delete(m.subs, ch)
	close(ch)
	m.subsMu.Unlock()
}

func (m *Manager) broadcast(evt Event) {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- evt:
		default: // drop for slow subscribers
		}
	}
}

// Run starts background loops. Blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	go m.discoveryLoop(ctx)
	go m.trafficLoop(ctx)
	<-ctx.Done()
}

func (m *Manager) discoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(discoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newGraph, err := m.discoverer.Discover(ctx)
			if err != nil {
				log.Printf("discovery refresh: %v", err)
				continue
			}
			if m.sourcePath != "" {
				edges, err := discovery.ScanSource(m.sourcePath)
				if err != nil {
					log.Printf("source scan refresh: %v", err)
				} else {
					discovery.MergeSourceEdges(newGraph, edges)
				}
			}
			collapsed, idMap := model.Collapse(newGraph)
			m.mu.Lock()
			old := m.graph
			m.graph = collapsed
			m.idMap = idMap
			// Re-apply traffic-discovered edges to the new graph.
			m.mergeTrafficEdgesLocked()
			m.mu.Unlock()
			if m.store != nil {
				if err := m.store.RecordNodes(collapsed.Nodes); err != nil {
					log.Printf("store record nodes: %v", err)
				}
			}
			if !model.GraphEqual(old, collapsed) {
				m.broadcast(Event{Type: "graph", Data: collapsed})
			}
		}
	}
}

func (m *Manager) trafficLoop(ctx context.Context) {
	ticker := time.NewTicker(trafficInterval)
	defer ticker.Stop()

	scan := func() {
		m.mu.RLock()
		idMap := m.idMap
		m.mu.RUnlock()

		// Broadcast partial results as pods respond so traffic appears quickly.
		onProgress := func(partial *model.TrafficSnapshot) {
			translated := translateTraffic(partial, idMap)
			m.broadcast(Event{Type: "traffic", Data: translated})
		}

		snap, err := m.scanner.ScanWithProgress(ctx, onProgress)
		if err != nil {
			log.Printf("traffic scan: %v", err)
			return
		}
		translated := translateTraffic(snap, idMap)

		now := time.Now()
		m.mu.Lock()
		// Append new scan, evict old ones outside the window.
		m.recentScans = append(m.recentScans, timedSnapshot{time: now, snap: translated})
		cutoff := now.Add(-trafficWindow)
		kept := 0
		for _, ts := range m.recentScans {
			if ts.time.After(cutoff) {
				m.recentScans[kept] = ts
				kept++
			}
		}
		m.recentScans = m.recentScans[:kept]

		merged := mergeSnapshots(m.recentScans)
		m.traffic = merged
		m.mu.Unlock()
		if m.store != nil {
			if err := m.store.RecordTraffic(translated); err != nil {
				log.Printf("store record traffic: %v", err)
			}
		}
		m.broadcast(Event{Type: "traffic", Data: merged})

		// Add edges for traffic connections that have no graph edge.
		if m.addTrafficEdges(merged) {
			m.mu.RLock()
			g := m.graph
			m.mu.RUnlock()
			m.broadcast(Event{Type: "graph", Data: g})
			m.persistTrafficEdges()
		}
	}

	// Run first scan immediately.
	scan()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan()
		}
	}
}

// addTrafficEdges checks for traffic between nodes that have no edge in the graph.
// Returns true if the graph was updated.
func (m *Manager) addTrafficEdges(snap *model.TrafficSnapshot) bool {
	if snap == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	nodeSet := make(map[string]bool, len(m.graph.Nodes))
	for _, n := range m.graph.Nodes {
		nodeSet[n.ID] = true
	}

	edgeSet := make(map[string]bool, len(m.graph.Edges))
	for _, e := range m.graph.Edges {
		edgeSet[e.From+"\t"+e.To] = true
		edgeSet[e.To+"\t"+e.From] = true // bidirectional check
	}

	changed := false
	for _, c := range snap.Connections {
		src := c.SourceWorkload
		tgt := c.TargetService
		if !nodeSet[src] || !nodeSet[tgt] || src == tgt {
			continue
		}
		if edgeSet[src+"\t"+tgt] || edgeSet[tgt+"\t"+src] {
			continue
		}
		edge := model.Edge{From: src, To: tgt, Kind: "traffic", Detail: "observed"}
		m.trafficEdges[src+"\t"+tgt] = edge
		m.graph.Edges = append(m.graph.Edges, edge)
		edgeSet[src+"\t"+tgt] = true
		changed = true
	}
	return changed
}

// mergeTrafficEdgesLocked re-applies traffic-discovered edges to the current graph.
// Must be called while holding m.mu.
func (m *Manager) mergeTrafficEdgesLocked() {
	if len(m.trafficEdges) == 0 {
		return
	}
	nodeSet := make(map[string]bool, len(m.graph.Nodes))
	for _, n := range m.graph.Nodes {
		nodeSet[n.ID] = true
	}
	edgeSet := make(map[string]bool, len(m.graph.Edges))
	for _, e := range m.graph.Edges {
		edgeSet[e.From+"\t"+e.To] = true
		edgeSet[e.To+"\t"+e.From] = true
	}
	for key, edge := range m.trafficEdges {
		if !nodeSet[edge.From] || !nodeSet[edge.To] {
			delete(m.trafficEdges, key) // node gone, drop edge
			continue
		}
		if edgeSet[edge.From+"\t"+edge.To] || edgeSet[edge.To+"\t"+edge.From] {
			continue // discovery already found this edge
		}
		m.graph.Edges = append(m.graph.Edges, edge)
		edgeSet[edge.From+"\t"+edge.To] = true
	}
}

func (m *Manager) persistTrafficEdges() {
	if m.store == nil {
		return
	}
	m.mu.RLock()
	edges := make([]model.Edge, 0, len(m.trafficEdges))
	for _, e := range m.trafficEdges {
		edges = append(edges, e)
	}
	m.mu.RUnlock()
	if err := m.store.SaveTrafficEdges(edges); err != nil {
		log.Printf("store save traffic edges: %v", err)
	}
}

// mergeSnapshots combines all snapshots in the window into one, taking max counts per pair.
func mergeSnapshots(scans []timedSnapshot) *model.TrafficSnapshot {
	aggr := make(map[string]*model.TrafficConnection)
	var latest int64
	for _, ts := range scans {
		if ts.snap == nil {
			continue
		}
		if ts.snap.Timestamp > latest {
			latest = ts.snap.Timestamp
		}
		for _, c := range ts.snap.Connections {
			key := c.SourceWorkload + "\t" + c.TargetService
			if existing, ok := aggr[key]; ok {
				if c.ConnCount > existing.ConnCount {
					existing.ConnCount = c.ConnCount
				}
				if c.ErrorCount > existing.ErrorCount {
					existing.ErrorCount = c.ErrorCount
				}
			} else {
				aggr[key] = &model.TrafficConnection{
					SourceWorkload: c.SourceWorkload,
					TargetService:  c.TargetService,
					ConnCount:      c.ConnCount,
					ErrorCount:     c.ErrorCount,
				}
			}
		}
	}
	// Merge external and cloud traffic: take max per workload, preserving TopIPs.
	extAggr := make(map[string]model.ExternalTraffic)
	cloudAggr := make(map[string]model.ExternalTraffic)
	for _, ts := range scans {
		if ts.snap == nil {
			continue
		}
		for _, ext := range ts.snap.External {
			if existing, ok := extAggr[ext.NodeID]; !ok || ext.ConnCount > existing.ConnCount {
				extAggr[ext.NodeID] = ext
			}
		}
		for _, cl := range ts.snap.Cloud {
			if existing, ok := cloudAggr[cl.NodeID]; !ok || cl.ConnCount > existing.ConnCount {
				cloudAggr[cl.NodeID] = cl
			}
		}
	}

	result := &model.TrafficSnapshot{Timestamp: latest}
	for _, c := range aggr {
		result.Connections = append(result.Connections, *c)
	}
	for _, ext := range extAggr {
		result.External = append(result.External, ext)
	}
	for _, cl := range cloudAggr {
		result.Cloud = append(result.Cloud, cl)
	}
	return result
}

// translateTraffic maps Service IDs in traffic data to their collapsed Workload IDs.
func translateTraffic(snap *model.TrafficSnapshot, idMap map[string]string) *model.TrafficSnapshot {
	if snap == nil {
		return nil
	}
	if len(idMap) == 0 {
		return snap
	}
	aggr := make(map[string]*model.TrafficConnection)
	for _, c := range snap.Connections {
		src := c.SourceWorkload
		tgt := c.TargetService
		if mapped, ok := idMap[src]; ok {
			src = mapped
		}
		if mapped, ok := idMap[tgt]; ok {
			tgt = mapped
		}
		if src == tgt {
			continue // skip self-connections after collapse
		}
		key := src + "\t" + tgt
		if existing, ok := aggr[key]; ok {
			existing.ConnCount += c.ConnCount
			existing.ErrorCount += c.ErrorCount
		} else {
			aggr[key] = &model.TrafficConnection{
				SourceWorkload: src,
				TargetService:  tgt,
				ConnCount:      c.ConnCount,
				ErrorCount:     c.ErrorCount,
			}
		}
	}
	result := &model.TrafficSnapshot{Timestamp: snap.Timestamp}
	for _, c := range aggr {
		result.Connections = append(result.Connections, *c)
	}
	// Translate external and cloud traffic node IDs.
	for _, ext := range snap.External {
		id := ext.NodeID
		if mapped, ok := idMap[id]; ok {
			id = mapped
		}
		result.External = append(result.External, model.ExternalTraffic{
			NodeID:    id,
			ConnCount: ext.ConnCount,
			TopIPs:    ext.TopIPs,
		})
	}
	for _, cl := range snap.Cloud {
		id := cl.NodeID
		if mapped, ok := idMap[id]; ok {
			id = mapped
		}
		result.Cloud = append(result.Cloud, model.ExternalTraffic{
			NodeID:    id,
			ConnCount: cl.ConnCount,
			TopIPs:    cl.TopIPs,
		})
	}
	return result
}
