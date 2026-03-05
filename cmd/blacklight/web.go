package main

import (
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

	addr := ":" + strconv.Itoa(port)
	fmt.Printf("Serving at http://localhost%s\n", addr)
	return http.ListenAndServe(addr, nil)
}
