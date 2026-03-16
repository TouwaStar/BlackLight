package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TouwaStar/BlackLight/pkg/discovery"
	"github.com/TouwaStar/BlackLight/pkg/model"
	"github.com/TouwaStar/BlackLight/pkg/render"
	"github.com/TouwaStar/BlackLight/pkg/store"
	"github.com/TouwaStar/BlackLight/pkg/traffic"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	defaultTailLines  = 100
	searchTailLines   = 200
	searchTimeout     = 30 * time.Second
	searchConcurrency = 5
)

//go:embed static
var staticFS embed.FS

// checkOrigin rejects cross-origin mutating requests (CSRF protection).
// Allows requests with no Origin (e.g. curl), or where the Origin matches the Host.
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser clients (curl, etc.)
	}
	// Strip scheme to compare host:port.
	originHost := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	host := r.Host
	if host == "" {
		host = r.Header.Get("Host")
	}
	return originHost == host
}

func serveWeb(mgr *Manager, port int) error {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	http.HandleFunc("/api/graph", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mgr.Graph())
	})

	http.HandleFunc("/api/mermaid", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, render.Mermaid(mgr.Graph(), "LR"))
	})

	http.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		st := mgr.Store()
		if st == nil {
			http.Error(w, "store not available", http.StatusServiceUnavailable)
			return
		}
		stats, err := st.NodeStats()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	})

	http.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		st := mgr.Store()
		if st == nil {
			http.Error(w, "store not available", http.StatusServiceUnavailable)
			return
		}
		source := r.URL.Query().Get("source")
		target := r.URL.Query().Get("target")
		if source == "" || target == "" {
			http.Error(w, "source and target required", http.StatusBadRequest)
			return
		}
		rangeStr := r.URL.Query().Get("range")
		if rangeStr == "" {
			rangeStr = "24h"
		}
		// Go's time.ParseDuration doesn't support "d"; convert to hours.
		if strings.HasSuffix(rangeStr, "d") {
			days, err := strconv.Atoi(strings.TrimSuffix(rangeStr, "d"))
			if err == nil && days > 0 {
				rangeStr = strconv.Itoa(days*24) + "h"
			}
		}
		dur, err := time.ParseDuration(rangeStr)
		if err != nil {
			http.Error(w, "invalid range: "+err.Error(), http.StatusBadRequest)
			return
		}
		buckets, err := st.EdgeHistory(source, target, time.Now().Add(-dur))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buckets)
	})

	http.HandleFunc("/api/positions", func(w http.ResponseWriter, r *http.Request) {
		st := mgr.Store()
		if st == nil {
			http.Error(w, "store not available", http.StatusServiceUnavailable)
			return
		}
		switch r.Method {
		case http.MethodGet:
			positions, err := st.LoadPositions()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(positions)
		case http.MethodPost:
			var positions []store.Position
			if err := json.NewDecoder(r.Body).Decode(&positions); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := st.SavePositions(positions); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	http.HandleFunc("/api/contexts", func(w http.ResponseWriter, r *http.Request) {
		contexts, current, err := discovery.ListKubeContexts()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		active := mgr.KubeContext()
		if active == "" {
			active = current
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"contexts": contexts,
			"current":  active,
		})
	})

	http.HandleFunc("/api/namespaces", func(w http.ResponseWriter, r *http.Request) {
		d := mgr.Discoverer()
		nsList, err := discovery.ListNamespacesFor(r.Context(), d.Clientset())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"namespaces": nsList,
			"current":    mgr.Namespace(),
		})
	})

	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !checkOrigin(r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}
		var req struct {
			Context   string `json:"context"`
			Namespace string `json:"namespace"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Run reconfigure in background so the POST returns immediately.
		go func() {
			if err := mgr.Reconfigure(context.Background(), req.Context, req.Namespace); err != nil {
				log.Printf("reconfigure: %v", err)
			}
		}()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch := mgr.Subscribe()
		defer mgr.Unsubscribe(ch)

		// Send current state immediately.
		if g := mgr.Graph(); g != nil {
			data, _ := json.Marshal(g)
			fmt.Fprintf(w, "event: graph\ndata: %s\n\n", data)
			flusher.Flush()
		}
		if t := mgr.Traffic(); t != nil {
			data, _ := json.Marshal(t)
			fmt.Fprintf(w, "event: traffic\ndata: %s\n\n", data)
			flusher.Flush()
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case evt, ok := <-ch:
				if !ok {
					return
				}
				data, err := json.Marshal(evt.Data)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, data)
				flusher.Flush()
			}
		}
	})

	http.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		kind, namespace, name, err := workloadParams(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tailLines := int64(defaultTailLines)
		if t := r.URL.Query().Get("tail"); t != "" {
			if v, err := strconv.ParseInt(t, 10, 64); err == nil && v > 0 {
				tailLines = v
			}
		}

		d := mgr.Discoverer()
		podName, container, err := findPodForWorkload(r.Context(), d.Clientset(), kind, namespace, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		stream, err := d.Clientset().CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
			Follow:     true,
			TailLines:  &tailLines,
			Timestamps: true,
			Container:  container,
		}).Stream(r.Context())
		if err != nil {
			http.Error(w, "log stream: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer stream.Close()

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Pod-Name", podName)
		w.Header().Set("X-Container", container)
		flusher.Flush()

		scanner := bufio.NewScanner(stream)
		for scanner.Scan() {
			fmt.Fprintln(w, scanner.Text())
			flusher.Flush()
		}
	})

	wsUpgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	http.HandleFunc("/api/exec", func(w http.ResponseWriter, r *http.Request) {
		kind, namespace, name, err := workloadParams(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		d := mgr.Discoverer()
		podName, container, err := findPodForWorkload(r.Context(), d.Clientset(), kind, namespace, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		ws, err := wsUpgrader.Upgrade(w, r, http.Header{
			"X-Pod-Name":  {podName},
			"X-Container": {container},
		})
		if err != nil {
			log.Printf("websocket upgrade: %v", err)
			return
		}
		defer ws.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		req := d.Clientset().CoreV1().RESTClient().Post().
			Resource("pods").
			Name(podName).
			Namespace(namespace).
			SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: container,
				Command:   []string{"/bin/sh"},
				Stdin:     true,
				Stdout:    true,
				Stderr:    true,
				TTY:       true,
			}, scheme.ParameterCodec)

		executor, err := remotecommand.NewSPDYExecutor(d.RestConfig(), "POST", req.URL())
		if err != nil {
			ws.WriteMessage(websocket.TextMessage, []byte("exec error: "+err.Error()))
			return
		}

		bridge := &wsBridge{
			ws:     ws,
			sizeCh: make(chan remotecommand.TerminalSize, 1),
		}
		bridge.stdinR, bridge.stdinW = io.Pipe()

		// Read from WebSocket, write to exec stdin.
		go bridge.readLoop(cancel)

		// Stream exec stdout to WebSocket.
		err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin:             bridge.stdinR,
			Stdout:            bridge,
			Stderr:            bridge,
			Tty:               true,
			TerminalSizeQueue: bridge,
		})
		if err != nil {
			ws.WriteMessage(websocket.TextMessage, []byte("\r\n[exec ended: "+err.Error()+"]\r\n"))
		}
	})

	http.HandleFunc("/api/describe", func(w http.ResponseWriter, r *http.Request) {
		kind, namespace, name, err := workloadParams(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := mgr.Discoverer()
		text, err := describeResource(r.Context(), d.Clientset(), kind, namespace, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(text))
	})

	http.HandleFunc("/api/env", func(w http.ResponseWriter, r *http.Request) {
		kind, namespace, name, err := workloadParams(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := mgr.Discoverer()
		cs := d.Clientset()
		podName, _, err := findPodForWorkload(r.Context(), cs, kind, namespace, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := getEnvVars(r.Context(), cs, namespace, podName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if !checkOrigin(r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}
		kind, namespace, name, err := workloadParams(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := mgr.Discoverer()
		cs := d.Clientset()
		ctx := r.Context()
		restartAnnotation := map[string]string{
			"kubectl.kubernetes.io/restartedAt": time.Now().Format(time.RFC3339),
		}
		switch kind {
		case "Deployment":
			dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if dep.Spec.Template.Annotations == nil {
				dep.Spec.Template.Annotations = make(map[string]string)
			}
			for k, v := range restartAnnotation {
				dep.Spec.Template.Annotations[k] = v
			}
			if _, err := cs.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "StatefulSet":
			sts, err := cs.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if sts.Spec.Template.Annotations == nil {
				sts.Spec.Template.Annotations = make(map[string]string)
			}
			for k, v := range restartAnnotation {
				sts.Spec.Template.Annotations[k] = v
			}
			if _, err := cs.AppsV1().StatefulSets(namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "DaemonSet":
			ds, err := cs.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if ds.Spec.Template.Annotations == nil {
				ds.Spec.Template.Annotations = make(map[string]string)
			}
			for k, v := range restartAnnotation {
				ds.Spec.Template.Annotations[k] = v
			}
			if _, err := cs.AppsV1().DaemonSets(namespace).Update(ctx, ds, metav1.UpdateOptions{}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		default:
			http.Error(w, "restart not supported for "+kind, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	http.HandleFunc("/api/pods", func(w http.ResponseWriter, r *http.Request) {
		kind, namespace, name, err := workloadParams(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := mgr.Discoverer()
		pods, err := listPodsForWorkload(r.Context(), d.Clientset(), kind, namespace, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pods)
	})

	http.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		kind, namespace, name, err := workloadParams(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := mgr.Discoverer()
		metrics, err := getPodMetrics(r.Context(), d.Clientset(), kind, namespace, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(metrics)
	})

	http.HandleFunc("/api/kubectl", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !checkOrigin(r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}
		var req struct {
			Command string `json:"command"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		raw := strings.TrimSpace(req.Command)
		if raw == "" {
			http.Error(w, "empty command", http.StatusBadRequest)
			return
		}

		// Strip leading "kubectl" if present so users can paste full commands.
		raw = strings.TrimPrefix(raw, "kubectl ")

		args := strings.Fields(raw)

		// Block interactive / destructive-to-blacklight flags.
		for _, a := range args {
			if a == "-it" || a == "--stdin" || a == "-i" || a == "--tty" || a == "edit" {
				http.Error(w, "interactive commands are not supported", http.StatusBadRequest)
				return
			}
		}

		// Inject current context if one is set.
		if ctx := mgr.KubeContext(); ctx != "" {
			args = append(args, "--context", ctx)
		}

		cmd := exec.CommandContext(r.Context(), "kubectl", args...)
		out, err := cmd.CombinedOutput()
		w.Header().Set("Content-Type", "application/json")
		resp := struct {
			Output string `json:"output"`
			Error  string `json:"error,omitempty"`
		}{Output: string(out)}
		if err != nil {
			resp.Error = err.Error()
			w.WriteHeader(http.StatusOK) // still 200 so frontend gets the output
		}
		json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/api/search-logs", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("q")
		if query == "" {
			http.Error(w, "q parameter required", http.StatusBadRequest)
			return
		}
		tailLines := int64(searchTailLines)
		if t := r.URL.Query().Get("tail"); t != "" {
			if v, err := strconv.ParseInt(t, 10, 64); err == nil && v > 0 {
				tailLines = v
			}
		}

		g := mgr.Graph()
		if g == nil {
			http.Error(w, "graph not available", http.StatusServiceUnavailable)
			return
		}

		// 30-second timeout so slow pods don't hang the request.
		ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
		defer cancel()

		d := mgr.Discoverer()

		// Batch-resolve workload → pod: single cluster-wide pod list
		// instead of 2 API calls per workload (Get workload + List pods).
		workloadNodes := make(map[string]struct{}) // node IDs we care about
		for _, node := range g.Nodes {
			switch node.Kind {
			case "Deployment", "StatefulSet", "DaemonSet", "Job":
				workloadNodes[node.ID] = struct{}{}
			}
		}
		if len(workloadNodes) == 0 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]struct{}{})
			return
		}

		type podTarget struct {
			nodeID    string
			namespace string
			podName   string
			container string
		}

		allPods, err := d.Clientset().CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: "status.phase=Running",
		})
		if err != nil {
			http.Error(w, "list pods: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Map each workload to one running pod by matching owner references.
		targets := make(map[string]podTarget) // nodeID → podTarget
		for i := range allPods.Items {
			pod := &allPods.Items[i]
			ownerKind, ownerName := traffic.WorkloadOwnerFromPod(pod)
			if ownerKind == "" {
				continue
			}
			nodeID := pod.Namespace + "/" + ownerKind + "/" + ownerName
			if _, want := workloadNodes[nodeID]; !want {
				continue
			}
			if _, have := targets[nodeID]; have {
				continue // already picked a pod for this workload
			}
			container := traffic.PickRunningContainer(pod)
			targets[nodeID] = podTarget{
				nodeID:    nodeID,
				namespace: pod.Namespace,
				podName:   pod.Name,
				container: container,
			}
		}

		type result struct {
			NodeID  string `json:"node_id"`
			Matches int    `json:"matches"`
		}
		var (
			mu      sync.Mutex
			results []result
			wg      sync.WaitGroup
		)

		// Limit concurrency to avoid overwhelming the API server.
		sem := make(chan struct{}, searchConcurrency)
		queryLower := strings.ToLower(query)

		for _, pt := range targets {
			wg.Add(1)
			go func(pt podTarget) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				stream, err := d.Clientset().CoreV1().Pods(pt.namespace).GetLogs(pt.podName, &corev1.PodLogOptions{
					TailLines: &tailLines,
					Container: pt.container,
				}).Stream(ctx)
				if err != nil {
					return
				}
				defer stream.Close()

				count := 0
				scanner := bufio.NewScanner(stream)
				for scanner.Scan() {
					if strings.Contains(strings.ToLower(scanner.Text()), queryLower) {
						count++
					}
				}
				if count > 0 {
					mu.Lock()
					results = append(results, result{NodeID: pt.nodeID, Matches: count})
					mu.Unlock()
				}
			}(pt)
		}
		wg.Wait()

		if results == nil {
			results = []result{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	})

	http.HandleFunc("/api/traffic-history", func(w http.ResponseWriter, r *http.Request) {
		st := mgr.Store()
		if st == nil {
			http.Error(w, "store not available", http.StatusServiceUnavailable)
			return
		}
		rangeStr := r.URL.Query().Get("range")
		if rangeStr == "" {
			rangeStr = "1h"
		}
		if strings.HasSuffix(rangeStr, "d") {
			days, err := strconv.Atoi(strings.TrimSuffix(rangeStr, "d"))
			if err == nil && days > 0 {
				rangeStr = strconv.Itoa(days*24) + "h"
			}
		}
		dur, err := time.ParseDuration(rangeStr)
		if err != nil {
			http.Error(w, "invalid range: "+err.Error(), http.StatusBadRequest)
			return
		}
		conns, err := st.TrafficInRange(time.Now().Add(-dur))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if conns == nil {
			conns = []model.TrafficConnection{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"connections": conns,
		})
	})

	http.HandleFunc("/api/topology", func(w http.ResponseWriter, r *http.Request) {
		d := mgr.Discoverer()
		ctx := r.Context()

		k8sNodes, err := d.Clientset().CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			http.Error(w, "list nodes: "+err.Error(), http.StatusInternalServerError)
			return
		}

		ns := mgr.Namespace()
		allPods, err := d.Clientset().CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			FieldSelector: "status.phase=Running",
		})
		if err != nil {
			http.Error(w, "list pods: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Map workload IDs to host nodes.
		workloadToNode := make(map[string]string) // workload node ID → k8s node name
		for i := range allPods.Items {
			pod := &allPods.Items[i]
			if pod.Spec.NodeName == "" {
				continue
			}
			ownerKind, ownerName := traffic.WorkloadOwnerFromPod(pod)
			if ownerKind == "" {
				continue
			}
			wid := pod.Namespace + "/" + ownerKind + "/" + ownerName
			// A workload may have pods on multiple nodes; pick first seen.
			if _, exists := workloadToNode[wid]; !exists {
				workloadToNode[wid] = pod.Spec.NodeName
			}
		}

		// Also track workloads with pods on MULTIPLE nodes.
		workloadNodes := make(map[string]map[string]bool) // wid → set of node names
		for i := range allPods.Items {
			pod := &allPods.Items[i]
			if pod.Spec.NodeName == "" {
				continue
			}
			ownerKind, ownerName := traffic.WorkloadOwnerFromPod(pod)
			if ownerKind == "" {
				continue
			}
			wid := pod.Namespace + "/" + ownerKind + "/" + ownerName
			if workloadNodes[wid] == nil {
				workloadNodes[wid] = make(map[string]bool)
			}
			workloadNodes[wid][pod.Spec.NodeName] = true
		}

		type topoNode struct {
			Name       string            `json:"name"`
			Labels     map[string]string `json:"labels,omitempty"`
			Capacity   map[string]string `json:"capacity,omitempty"`
			Conditions []string          `json:"conditions,omitempty"`
			Workloads  []string          `json:"workloads"`
		}

		result := make([]topoNode, 0, len(k8sNodes.Items))
		for i := range k8sNodes.Items {
			node := &k8sNodes.Items[i]
			tn := topoNode{
				Name:      node.Name,
				Labels:    node.Labels,
				Capacity:  make(map[string]string),
				Workloads: []string{},
			}
			if cpu := node.Status.Capacity.Cpu(); cpu != nil {
				tn.Capacity["cpu"] = cpu.String()
			}
			if mem := node.Status.Capacity.Memory(); mem != nil {
				memMiB := mem.Value() / (1024 * 1024)
				tn.Capacity["memory"] = fmt.Sprintf("%d MiB", memMiB)
			}
			for _, cond := range node.Status.Conditions {
				if cond.Status == "True" {
					tn.Conditions = append(tn.Conditions, string(cond.Type))
				}
			}
			// Find workloads placed on this node.
			for wid, nodeName := range workloadToNode {
				if nodeName == node.Name {
					tn.Workloads = append(tn.Workloads, wid)
				}
			}
			result = append(result, tn)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"nodes":          result,
			"workload_nodes": workloadNodes,
		})
	})

	addr := ":" + strconv.Itoa(port)
	fmt.Printf("Serving at http://localhost%s\n", addr)
	return http.ListenAndServe(addr, nil)
}

// wsBridge bridges a WebSocket connection to a k8s remotecommand exec session.
type wsBridge struct {
	ws     *websocket.Conn
	mu     sync.Mutex
	sizeCh chan remotecommand.TerminalSize
	stdinR *io.PipeReader
	stdinW *io.PipeWriter
}

// Write sends exec stdout/stderr to the WebSocket client.
func (b *wsBridge) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ws.WriteMessage(websocket.TextMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Next implements remotecommand.TerminalSizeQueue.
func (b *wsBridge) Next() *remotecommand.TerminalSize {
	size, ok := <-b.sizeCh
	if !ok {
		return nil
	}
	return &size
}

// readLoop reads messages from the WebSocket and routes them to stdin or resize.
func (b *wsBridge) readLoop(cancel context.CancelFunc) {
	defer cancel()
	defer b.stdinW.Close()
	for {
		_, msg, err := b.ws.ReadMessage()
		if err != nil {
			return
		}
		// Check for resize message: {"type":"resize","cols":80,"rows":24}
		if len(msg) > 0 && msg[0] == '{' {
			var resize struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(msg, &resize) == nil && resize.Type == "resize" {
				select {
				case b.sizeCh <- remotecommand.TerminalSize{Width: resize.Cols, Height: resize.Rows}:
				default:
				}
				continue
			}
		}
		if _, err := b.stdinW.Write(msg); err != nil {
			return
		}
	}
}

// workloadParams extracts and validates kind/namespace/name query parameters.
func workloadParams(r *http.Request) (kind, namespace, name string, err error) {
	kind = r.URL.Query().Get("kind")
	namespace = r.URL.Query().Get("namespace")
	name = r.URL.Query().Get("name")
	if kind == "" || namespace == "" || name == "" {
		return "", "", "", fmt.Errorf("kind, namespace, and name required")
	}
	return kind, namespace, name, nil
}

// findPodForWorkload resolves a workload (Deployment, StatefulSet, etc.) to a running pod.
func findPodForWorkload(ctx context.Context, clientset *kubernetes.Clientset, kind, namespace, name string) (string, string, error) {
	sel, err := selectorForWorkload(ctx, clientset, kind, namespace, name)
	if err != nil {
		return "", "", err
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sel,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return "", "", fmt.Errorf("list pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Ready && cs.State.Running != nil {
				return pod.Name, cs.Name, nil
			}
		}
	}
	if len(pods.Items) > 0 && len(pods.Items[0].Spec.Containers) > 0 {
		return pods.Items[0].Name, pods.Items[0].Spec.Containers[0].Name, nil
	}
	return "", "", fmt.Errorf("no running pods found for %s/%s", kind, name)
}

// describeResource returns a kubectl-describe-like text summary for a k8s resource.
func describeResource(ctx context.Context, clientset *kubernetes.Clientset, kind, namespace, name string) (string, error) {
	var b strings.Builder
	w := func(label, value string) { fmt.Fprintf(&b, "%-22s %s\n", label+":", value) }
	section := func(title string) { fmt.Fprintf(&b, "\n%s\n", title) }

	writeLabels := func(labels map[string]string) {
		if len(labels) == 0 {
			w("Labels", "<none>")
			return
		}
		first := true
		for k, v := range labels {
			if first {
				w("Labels", k+"="+v)
				first = false
			} else {
				fmt.Fprintf(&b, "%-22s %s\n", "", k+"="+v)
			}
		}
	}

	writeAnnotations := func(annotations map[string]string) {
		if len(annotations) == 0 {
			w("Annotations", "<none>")
			return
		}
		first := true
		for k, v := range annotations {
			val := k + "=" + v
			if len(val) > 80 {
				val = val[:80] + "..."
			}
			if first {
				w("Annotations", val)
				first = false
			} else {
				fmt.Fprintf(&b, "%-22s %s\n", "", val)
			}
		}
	}

	writeContainers := func(containers []corev1.Container) {
		section("Containers:")
		for _, c := range containers {
			fmt.Fprintf(&b, "  %s:\n", c.Name)
			fmt.Fprintf(&b, "    Image:    %s\n", c.Image)
			if len(c.Ports) > 0 {
				ports := make([]string, 0, len(c.Ports))
				for _, p := range c.Ports {
					entry := fmt.Sprintf("%d/%s", p.ContainerPort, p.Protocol)
					if p.Name != "" {
						entry = p.Name + " " + entry
					}
					ports = append(ports, entry)
				}
				fmt.Fprintf(&b, "    Ports:    %s\n", strings.Join(ports, ", "))
			}
			if len(c.Env) > 0 {
				envCount := len(c.Env)
				envFrom := len(c.EnvFrom)
				detail := fmt.Sprintf("%d vars", envCount)
				if envFrom > 0 {
					detail += fmt.Sprintf(" + %d from refs", envFrom)
				}
				fmt.Fprintf(&b, "    Env:      %s\n", detail)
			}
			res := c.Resources
			if len(res.Requests) > 0 || len(res.Limits) > 0 {
				fmt.Fprintf(&b, "    Resources:\n")
				if cpu := res.Requests.Cpu(); cpu != nil && !cpu.IsZero() {
					fmt.Fprintf(&b, "      cpu request:     %s\n", cpu.String())
				}
				if mem := res.Requests.Memory(); mem != nil && !mem.IsZero() {
					fmt.Fprintf(&b, "      memory request:  %s\n", mem.String())
				}
				if cpu := res.Limits.Cpu(); cpu != nil && !cpu.IsZero() {
					fmt.Fprintf(&b, "      cpu limit:       %s\n", cpu.String())
				}
				if mem := res.Limits.Memory(); mem != nil && !mem.IsZero() {
					fmt.Fprintf(&b, "      memory limit:    %s\n", mem.String())
				}
			}
			if c.ReadinessProbe != nil {
				fmt.Fprintf(&b, "    Readiness:  configured\n")
			}
			if c.LivenessProbe != nil {
				fmt.Fprintf(&b, "    Liveness:   configured\n")
			}
		}
	}

	writeConditions := func(conditions []string) {
		if len(conditions) == 0 {
			return
		}
		section("Conditions:")
		for _, c := range conditions {
			fmt.Fprintf(&b, "  %s\n", c)
		}
	}

	switch kind {
	case "Deployment":
		obj, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get deployment: %w", err)
		}
		w("Name", obj.Name)
		w("Namespace", obj.Namespace)
		w("Created", obj.CreationTimestamp.String())
		writeLabels(obj.Labels)
		writeAnnotations(obj.Annotations)
		w("Selector", metav1.FormatLabelSelector(obj.Spec.Selector))
		w("Replicas", fmt.Sprintf("%d desired | %d ready | %d available | %d unavailable",
			*obj.Spec.Replicas, obj.Status.ReadyReplicas, obj.Status.AvailableReplicas, obj.Status.UnavailableReplicas))
		w("Strategy", string(obj.Spec.Strategy.Type))
		writeContainers(obj.Spec.Template.Spec.Containers)
		var conds []string
		for _, c := range obj.Status.Conditions {
			conds = append(conds, fmt.Sprintf("%-20s %s  (%s) %s", c.Type, string(c.Status), c.Reason, c.Message))
		}
		writeConditions(conds)

	case "StatefulSet":
		obj, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get statefulset: %w", err)
		}
		w("Name", obj.Name)
		w("Namespace", obj.Namespace)
		w("Created", obj.CreationTimestamp.String())
		writeLabels(obj.Labels)
		writeAnnotations(obj.Annotations)
		w("Selector", metav1.FormatLabelSelector(obj.Spec.Selector))
		w("Replicas", fmt.Sprintf("%d desired | %d ready", *obj.Spec.Replicas, obj.Status.ReadyReplicas))
		w("Update Strategy", string(obj.Spec.UpdateStrategy.Type))
		if obj.Spec.ServiceName != "" {
			w("Service Name", obj.Spec.ServiceName)
		}
		writeContainers(obj.Spec.Template.Spec.Containers)

	case "DaemonSet":
		obj, err := clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get daemonset: %w", err)
		}
		w("Name", obj.Name)
		w("Namespace", obj.Namespace)
		w("Created", obj.CreationTimestamp.String())
		writeLabels(obj.Labels)
		writeAnnotations(obj.Annotations)
		w("Selector", metav1.FormatLabelSelector(obj.Spec.Selector))
		w("Nodes", fmt.Sprintf("%d desired | %d current | %d ready | %d available",
			obj.Status.DesiredNumberScheduled, obj.Status.CurrentNumberScheduled,
			obj.Status.NumberReady, obj.Status.NumberAvailable))
		writeContainers(obj.Spec.Template.Spec.Containers)

	case "Job":
		obj, err := clientset.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get job: %w", err)
		}
		w("Name", obj.Name)
		w("Namespace", obj.Namespace)
		w("Created", obj.CreationTimestamp.String())
		writeLabels(obj.Labels)
		writeAnnotations(obj.Annotations)
		w("Completions", fmt.Sprintf("%d", *obj.Spec.Completions))
		w("Parallelism", fmt.Sprintf("%d", *obj.Spec.Parallelism))
		w("Succeeded", fmt.Sprintf("%d", obj.Status.Succeeded))
		if obj.Status.Failed > 0 {
			w("Failed", fmt.Sprintf("%d", obj.Status.Failed))
		}
		writeContainers(obj.Spec.Template.Spec.Containers)

	case "CronJob":
		obj, err := clientset.BatchV1().CronJobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get cronjob: %w", err)
		}
		w("Name", obj.Name)
		w("Namespace", obj.Namespace)
		w("Created", obj.CreationTimestamp.String())
		writeLabels(obj.Labels)
		writeAnnotations(obj.Annotations)
		w("Schedule", obj.Spec.Schedule)
		if obj.Spec.Suspend != nil && *obj.Spec.Suspend {
			w("Suspend", "true")
		}
		if obj.Status.LastScheduleTime != nil {
			w("Last Schedule", obj.Status.LastScheduleTime.String())
		}
		w("Active Jobs", fmt.Sprintf("%d", len(obj.Status.Active)))
		writeContainers(obj.Spec.JobTemplate.Spec.Template.Spec.Containers)

	case "Service":
		obj, err := clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get service: %w", err)
		}
		w("Name", obj.Name)
		w("Namespace", obj.Namespace)
		w("Created", obj.CreationTimestamp.String())
		writeLabels(obj.Labels)
		writeAnnotations(obj.Annotations)
		w("Type", string(obj.Spec.Type))
		w("ClusterIP", obj.Spec.ClusterIP)
		if len(obj.Spec.ExternalIPs) > 0 {
			w("External IPs", strings.Join(obj.Spec.ExternalIPs, ", "))
		}
		if obj.Spec.LoadBalancerIP != "" {
			w("LoadBalancer IP", obj.Spec.LoadBalancerIP)
		}
		for _, ing := range obj.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				w("LB Ingress IP", ing.IP)
			}
			if ing.Hostname != "" {
				w("LB Ingress Host", ing.Hostname)
			}
		}
		sel := make([]string, 0, len(obj.Spec.Selector))
		for k, v := range obj.Spec.Selector {
			sel = append(sel, k+"="+v)
		}
		if len(sel) > 0 {
			w("Selector", strings.Join(sel, ","))
		}
		section("Ports:")
		for _, p := range obj.Spec.Ports {
			fmt.Fprintf(&b, "  %s %d/%s → %s\n", p.Name, p.Port, p.Protocol, p.TargetPort.String())
		}

	case "Ingress":
		obj, err := clientset.NetworkingV1().Ingresses(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get ingress: %w", err)
		}
		w("Name", obj.Name)
		w("Namespace", obj.Namespace)
		w("Created", obj.CreationTimestamp.String())
		writeLabels(obj.Labels)
		writeAnnotations(obj.Annotations)
		if obj.Spec.IngressClassName != nil {
			w("Class", *obj.Spec.IngressClassName)
		}
		section("Rules:")
		for _, rule := range obj.Spec.Rules {
			host := rule.Host
			if host == "" {
				host = "*"
			}
			fmt.Fprintf(&b, "  Host: %s\n", host)
			if rule.HTTP != nil {
				for _, path := range rule.HTTP.Paths {
					backend := ""
					if path.Backend.Service != nil {
						backend = path.Backend.Service.Name
						if path.Backend.Service.Port.Number != 0 {
							backend += fmt.Sprintf(":%d", path.Backend.Service.Port.Number)
						} else if path.Backend.Service.Port.Name != "" {
							backend += ":" + path.Backend.Service.Port.Name
						}
					}
					fmt.Fprintf(&b, "    %s → %s\n", path.Path, backend)
				}
			}
		}

	default:
		return "", fmt.Errorf("unsupported kind: %s", kind)
	}

	// Append recent events for this resource.
	events, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=%s", name, kind),
	})
	if err == nil && len(events.Items) > 0 {
		section("Events:")
		// Show last 10 events.
		items := events.Items
		if len(items) > 10 {
			items = items[len(items)-10:]
		}
		for _, ev := range items {
			age := time.Since(ev.LastTimestamp.Time)
			ageStr := ""
			if age < time.Minute {
				ageStr = fmt.Sprintf("%ds", int(age.Seconds()))
			} else if age < time.Hour {
				ageStr = fmt.Sprintf("%dm", int(age.Minutes()))
			} else {
				ageStr = fmt.Sprintf("%dh", int(age.Hours()))
			}
			fmt.Fprintf(&b, "  %-6s %-8s %s: %s\n", ageStr, ev.Type, ev.Reason, ev.Message)
		}
	}

	return b.String(), nil
}

type envEntry struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source,omitempty"`
}

type containerEnv struct {
	Name string     `json:"name"`
	Env  []envEntry `json:"env"`
}

type envResponse struct {
	Pod        string         `json:"pod"`
	Containers []containerEnv `json:"containers"`
}

func getEnvVars(ctx context.Context, cs *kubernetes.Clientset, namespace, podName string) (*envResponse, error) {
	pod, err := cs.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod: %w", err)
	}

	// Cache fetched ConfigMaps/Secrets to avoid redundant API calls.
	cmCache := map[string]*corev1.ConfigMap{}
	secCache := map[string]*corev1.Secret{}

	fetchCM := func(name string) (*corev1.ConfigMap, error) {
		if cm, ok := cmCache[name]; ok {
			return cm, nil
		}
		cm, err := cs.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		cmCache[name] = cm
		return cm, nil
	}
	fetchSec := func(name string) (*corev1.Secret, error) {
		if sec, ok := secCache[name]; ok {
			return sec, nil
		}
		sec, err := cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		secCache[name] = sec
		return sec, nil
	}

	resp := &envResponse{Pod: podName}

	for _, c := range pod.Spec.Containers {
		ce := containerEnv{Name: c.Name}

		// EnvFrom: bulk import from ConfigMap/Secret.
		for _, ef := range c.EnvFrom {
			prefix := ef.Prefix
			if ef.ConfigMapRef != nil {
				cm, err := fetchCM(ef.ConfigMapRef.Name)
				if err != nil {
					if ef.ConfigMapRef.Optional != nil && *ef.ConfigMapRef.Optional {
						continue
					}
					ce.Env = append(ce.Env, envEntry{
						Name:   prefix + "(*)",
						Value:  fmt.Sprintf("<error: %v>", err),
						Source: "ConfigMap/" + ef.ConfigMapRef.Name,
					})
					continue
				}
				for k, v := range cm.Data {
					ce.Env = append(ce.Env, envEntry{
						Name:   prefix + k,
						Value:  v,
						Source: "ConfigMap/" + ef.ConfigMapRef.Name,
					})
				}
			}
			if ef.SecretRef != nil {
				sec, err := fetchSec(ef.SecretRef.Name)
				if err != nil {
					if ef.SecretRef.Optional != nil && *ef.SecretRef.Optional {
						continue
					}
					ce.Env = append(ce.Env, envEntry{
						Name:   prefix + "(*)",
						Value:  fmt.Sprintf("<error: %v>", err),
						Source: "Secret/" + ef.SecretRef.Name,
					})
					continue
				}
				for k, v := range sec.Data {
					ce.Env = append(ce.Env, envEntry{
						Name:   prefix + k,
						Value:  string(v),
						Source: "Secret/" + ef.SecretRef.Name,
					})
				}
			}
		}

		// Env: individual vars.
		for _, e := range c.Env {
			entry := envEntry{Name: e.Name}
			if e.ValueFrom == nil {
				entry.Value = e.Value
			} else if ref := e.ValueFrom.ConfigMapKeyRef; ref != nil {
				entry.Source = "ConfigMap/" + ref.Name + "/" + ref.Key
				cm, err := fetchCM(ref.Name)
				if err != nil {
					if ref.Optional != nil && *ref.Optional {
						entry.Value = ""
					} else {
						entry.Value = fmt.Sprintf("<error: %v>", err)
					}
				} else {
					entry.Value = cm.Data[ref.Key]
				}
			} else if ref := e.ValueFrom.SecretKeyRef; ref != nil {
				entry.Source = "Secret/" + ref.Name + "/" + ref.Key
				sec, err := fetchSec(ref.Name)
				if err != nil {
					if ref.Optional != nil && *ref.Optional {
						entry.Value = ""
					} else {
						entry.Value = fmt.Sprintf("<error: %v>", err)
					}
				} else {
					entry.Value = string(sec.Data[ref.Key])
				}
			} else if ref := e.ValueFrom.FieldRef; ref != nil {
				entry.Source = "fieldRef/" + ref.FieldPath
			} else if ref := e.ValueFrom.ResourceFieldRef; ref != nil {
				entry.Source = "resourceFieldRef/" + ref.Resource
			}
			ce.Env = append(ce.Env, entry)
		}

		resp.Containers = append(resp.Containers, ce)
	}

	return resp, nil
}

// --- Pod list ---

type podInfo struct {
	Name       string          `json:"name"`
	Phase      string          `json:"phase"`
	Ready      string          `json:"ready"`
	Restarts   int32           `json:"restarts"`
	Age        string          `json:"age"`
	Node       string          `json:"node"`
	IP         string          `json:"ip"`
	Containers []containerInfo `json:"containers"`
}

type containerInfo struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	State string `json:"state"`
	Image string `json:"image"`
}

type podsResponse struct {
	Pods []podInfo `json:"pods"`
}

func listPodsForWorkload(ctx context.Context, cs *kubernetes.Clientset, kind, namespace, name string) (*podsResponse, error) {
	sel, err := selectorForWorkload(ctx, cs, kind, namespace, name)
	if err != nil {
		return nil, err
	}
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sel,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	resp := &podsResponse{}
	for i := range pods.Items {
		p := &pods.Items[i]
		pi := podInfo{
			Name:  p.Name,
			Phase: string(p.Status.Phase),
			Node:  p.Spec.NodeName,
			IP:    p.Status.PodIP,
			Age:   shortAge(time.Since(p.CreationTimestamp.Time)),
		}

		var readyCnt, totalCnt int
		for _, cs := range p.Status.ContainerStatuses {
			totalCnt++
			if cs.Ready {
				readyCnt++
			}
			pi.Restarts += cs.RestartCount

			ci := containerInfo{
				Name:  cs.Name,
				Ready: cs.Ready,
				Image: cs.Image,
			}
			switch {
			case cs.State.Running != nil:
				ci.State = "Running"
			case cs.State.Waiting != nil:
				ci.State = "Waiting: " + cs.State.Waiting.Reason
			case cs.State.Terminated != nil:
				ci.State = "Terminated: " + cs.State.Terminated.Reason
			}
			pi.Containers = append(pi.Containers, ci)
		}
		pi.Ready = fmt.Sprintf("%d/%d", readyCnt, totalCnt)
		resp.Pods = append(resp.Pods, pi)
	}
	// Sort pods by name for consistent ordering.
	sort.Slice(resp.Pods, func(i, j int) bool { return resp.Pods[i].Name < resp.Pods[j].Name })
	return resp, nil
}

// selectorForWorkload returns the label selector string for a workload's pods.
func selectorForWorkload(ctx context.Context, cs *kubernetes.Clientset, kind, namespace, name string) (string, error) {
	var selector *metav1.LabelSelector
	switch kind {
	case "Deployment":
		obj, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get deployment: %w", err)
		}
		selector = obj.Spec.Selector
	case "StatefulSet":
		obj, err := cs.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get statefulset: %w", err)
		}
		selector = obj.Spec.Selector
	case "DaemonSet":
		obj, err := cs.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get daemonset: %w", err)
		}
		selector = obj.Spec.Selector
	case "Job":
		obj, err := cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get job: %w", err)
		}
		selector = obj.Spec.Selector
	default:
		return "", fmt.Errorf("unsupported kind: %s", kind)
	}
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return "", fmt.Errorf("label selector: %w", err)
	}
	return sel.String(), nil
}

func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// --- Pod metrics ---

type containerMetrics struct {
	Name   string `json:"name"`
	CPUm   int64  `json:"cpu_millicores"`
	MemMiB int64  `json:"mem_mib"`
}

type podMetrics struct {
	Name       string             `json:"name"`
	Containers []containerMetrics `json:"containers"`
	TotalCPUm  int64              `json:"total_cpu_millicores"`
	TotalMemMiB int64             `json:"total_mem_mib"`
}

type metricsResponse struct {
	Pods []podMetrics `json:"pods"`
}

func getPodMetrics(ctx context.Context, cs *kubernetes.Clientset, kind, namespace, name string) (*metricsResponse, error) {
	sel, err := selectorForWorkload(ctx, cs, kind, namespace, name)
	if err != nil {
		return nil, err
	}

	// Query the metrics API directly via the REST client.
	// This avoids importing the k8s.io/metrics module.
	path := fmt.Sprintf("/apis/metrics.k8s.io/v1beta1/namespaces/%s/pods", namespace)
	raw, err := cs.RESTClient().Get().
		AbsPath(path).
		Param("labelSelector", sel).
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("metrics API: %w", err)
	}

	var result struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Containers []struct {
				Name  string            `json:"name"`
				Usage map[string]string `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}

	resp := &metricsResponse{}
	for _, item := range result.Items {
		pm := podMetrics{Name: item.Metadata.Name}
		for _, c := range item.Containers {
			cm := containerMetrics{Name: c.Name}
			if cpuStr, ok := c.Usage["cpu"]; ok {
				q, err := resource.ParseQuantity(cpuStr)
				if err == nil {
					cm.CPUm = q.MilliValue()
				}
			}
			if memStr, ok := c.Usage["memory"]; ok {
				q, err := resource.ParseQuantity(memStr)
				if err == nil {
					cm.MemMiB = q.Value() / (1024 * 1024)
				}
			}
			pm.TotalCPUm += cm.CPUm
			pm.TotalMemMiB += cm.MemMiB
			pm.Containers = append(pm.Containers, cm)
		}
		resp.Pods = append(resp.Pods, pm)
	}
	sort.Slice(resp.Pods, func(i, j int) bool { return resp.Pods[i].Name < resp.Pods[j].Name })
	return resp, nil
}
