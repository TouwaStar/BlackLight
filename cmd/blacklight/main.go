package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TouwaStar/BlackLight/pkg/discovery"
	"github.com/TouwaStar/BlackLight/pkg/render"
	"github.com/TouwaStar/BlackLight/pkg/store"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	showVersion := flag.Bool("version", false, "Print version and exit")

	cfg := loadConfig()

	namespace := flag.String("namespace", cfg.Namespace, "Kubernetes namespace (default: all)")
	output := flag.String("output", "web", "Output format: web, mermaid, json")
	port := flag.Int("port", 8080, "Port for web server (when output=web)")
	source := flag.String("source", cfg.Source, "Path to source code root for static dependency scanning")
	dataDir := flag.String("data-dir", cfg.DataDir, "Directory for persistent data (default: ~/.local/share/blacklight)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("blacklight %s (commit: %s, built: %s)\n", version, commit, buildTime)
		os.Exit(0)
	}

	discoverer, err := discovery.NewDiscoverer(*namespace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	g, err := discoverer.Discover(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discover: %v\n", err)
		os.Exit(1)
	}

	if *source != "" {
		sourceEdges, err := discovery.ScanSource(*source)
		if err != nil {
			fmt.Fprintf(os.Stderr, "source scan: %v\n", err)
			os.Exit(1)
		}
		discovery.MergeSourceEdges(g, sourceEdges)
	}

	switch *output {
	case "mermaid":
		fmt.Println(render.Mermaid(g, "LR"))
	case "json":
		data, err := render.JSON(g)
		if err != nil {
			fmt.Fprintf(os.Stderr, "json: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
	case "web":
		dd := *dataDir
		if dd == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				fmt.Fprintf(os.Stderr, "home dir: %v\n", err)
				os.Exit(1)
			}
			dd = filepath.Join(home, ".local", "share", "blacklight")
		}
		if err := os.MkdirAll(dd, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "data dir: %v\n", err)
			os.Exit(1)
		}
		st, err := store.New(filepath.Join(dd, "blacklight.db"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "store: %v\n", err)
			os.Exit(1)
		}
		defer st.Close()

		mgr := NewManager(discoverer, st, *source)
		mgr.SetInitialGraph(g)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go mgr.Run(ctx)
		if err := serveWeb(mgr, *port); err != nil {
			fmt.Fprintf(os.Stderr, "web: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown output: %s (use mermaid, json, or web)\n", *output)
		os.Exit(1)
	}
}
