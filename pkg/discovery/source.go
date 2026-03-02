package discovery

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SourceEdge represents a service dependency found in source code.
type SourceEdge struct {
	Caller string // source directory name (top-level dir under root)
	Callee string // target service name extracted from URL/call
	File   string // relative file path where the reference was found
}

var scanExts = map[string]bool{
	".go":   true,
	".py":   true,
	".js":   true,
	".ts":   true,
	".java": true,
}

var skipDirs = map[string]bool{
	"vendor":       true,
	"node_modules": true,
	".git":         true,
	"testdata":     true,
	"__pycache__":  true,
	"build":        true,
	"dist":         true,
}

// ScanSource walks source files under root and extracts service-to-service
// references from URL literals.
func ScanSource(root string) ([]SourceEdge, error) {
	urlRe := regexp.MustCompile(`"https?://([a-z][-a-z0-9]*)[\.:\/]`)

	type edgeKey struct{ caller, callee string }
	seen := make(map[edgeKey]SourceEdge)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible paths
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !scanExts[filepath.Ext(path)] {
			return nil
		}
		if isTestFile(path) {
			return nil
		}

		relPath, _ := filepath.Rel(root, path)
		caller := callerFromPath(relPath)
		if caller == "" {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			for _, match := range urlRe.FindAllStringSubmatch(line, -1) {
				svc := match[1]
				if svc == "localhost" || svc == "example" || svc == caller {
					continue
				}
				key := edgeKey{caller, svc}
				if _, ok := seen[key]; !ok {
					seen[key] = SourceEdge{Caller: caller, Callee: svc, File: relPath}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	edges := make([]SourceEdge, 0, len(seen))
	for _, e := range seen {
		edges = append(edges, e)
	}
	return edges, nil
}

func isTestFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, "_test.py") ||
		strings.HasSuffix(base, ".test.js") ||
		strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".spec.js") ||
		strings.HasSuffix(base, ".spec.ts") ||
		strings.HasPrefix(base, "test_") ||
		strings.Contains(path, "/tests/") ||
		strings.Contains(path, "/__tests__/")
}

// callerFromPath extracts the top-level directory from a relative path.
// "customers/robots/robots.go" → "customers"
func callerFromPath(relPath string) string {
	parts := strings.SplitN(filepath.ToSlash(relPath), "/", 2)
	if len(parts) < 2 {
		return "" // file at root, skip
	}
	return parts[0]
}

