package main

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds settings loaded from .blacklight.yaml.
type Config struct {
	Source    string `yaml:"source"`
	Namespace string `yaml:"namespace"`
	DataDir   string `yaml:"data_dir"` // persistence directory (default: ~/.local/share/blacklight)
}

// loadConfig reads .blacklight.yaml from the current directory, falling back
// to ~/.config/blacklight/config.yaml. Returns zero Config if neither exists.
func loadConfig() Config {
	for _, path := range configPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg Config
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			continue
		}
		// Resolve relative source path relative to the config file's directory.
		if cfg.Source != "" && !filepath.IsAbs(cfg.Source) {
			cfg.Source = filepath.Join(filepath.Dir(path), cfg.Source)
		}
		return cfg
	}
	return Config{}
}

func configPaths() []string {
	paths := []string{".blacklight.yaml"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "blacklight", "config.yaml"))
	}
	return paths
}
