package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/TouwaStar/BlackLight/pkg/discovery"
	"github.com/TouwaStar/BlackLight/pkg/render"
	"github.com/TouwaStar/BlackLight/pkg/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

//go:embed static
var staticFS embed.FS

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
		kind := r.URL.Query().Get("kind")
		namespace := r.URL.Query().Get("namespace")
		name := r.URL.Query().Get("name")
		if kind == "" || namespace == "" || name == "" {
			http.Error(w, "kind, namespace, and name required", http.StatusBadRequest)
			return
		}
		tailLines := int64(100)
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

	addr := ":" + strconv.Itoa(port)
	fmt.Printf("Serving at http://localhost%s\n", addr)
	return http.ListenAndServe(addr, nil)
}

// findPodForWorkload resolves a workload (Deployment, StatefulSet, etc.) to a running pod.
func findPodForWorkload(ctx context.Context, clientset *kubernetes.Clientset, kind, namespace, name string) (string, string, error) {
	var selector *metav1.LabelSelector
	switch kind {
	case "Deployment":
		obj, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", "", fmt.Errorf("get deployment: %w", err)
		}
		selector = obj.Spec.Selector
	case "StatefulSet":
		obj, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", "", fmt.Errorf("get statefulset: %w", err)
		}
		selector = obj.Spec.Selector
	case "DaemonSet":
		obj, err := clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", "", fmt.Errorf("get daemonset: %w", err)
		}
		selector = obj.Spec.Selector
	case "Job":
		obj, err := clientset.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", "", fmt.Errorf("get job: %w", err)
		}
		selector = obj.Spec.Selector
	default:
		return "", "", fmt.Errorf("unsupported kind: %s", kind)
	}

	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return "", "", fmt.Errorf("label selector: %w", err)
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sel.String(),
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
