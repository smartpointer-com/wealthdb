// Package config parses the wealthdb JSON config file. See
// docs/DESIGN.md §5 for the schema and defaults.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the in-memory shape of the wealthdb config file.
// Field paths in the file are tilde / $HOME-expanded at Load time;
// callers always see absolute paths.
type Config struct {
	GoldDB          string         `json:"gold_db"`
	DefaultCurrency string         `json:"default_currency"`
	SilverSources   []SilverSource `json:"silver_sources"`
}

// SilverSource is one entry under `silver_sources` in the config
// file.
type SilverSource struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// Load reads and parses the JSON config at the given path,
// expands `~` and `$HOME` in path fields, resolves relative paths
// against the config file's directory, and validates structural
// requirements. The returned Config is ready to use.
func Load(path string) (*Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: resolve %q: %w", path, err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", absPath, err)
	}

	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", absPath, err)
	}

	// Expand path-valued fields. Done before validation so any
	// path-shape checks see the resolved value.
	configDir := filepath.Dir(absPath)
	expanded, err := expandPath(c.GoldDB, configDir)
	if err != nil {
		return nil, fmt.Errorf("config: gold_db: %w", err)
	}
	c.GoldDB = expanded
	for i := range c.SilverSources {
		expanded, err := expandPath(c.SilverSources[i].Path, configDir)
		if err != nil {
			return nil, fmt.Errorf("config: silver_sources[%d].path: %w", i, err)
		}
		c.SilverSources[i].Path = expanded
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Lookup returns the named silver source from the config, or
// (nil, false) if no source with that ID is defined.
func (c *Config) Lookup(id string) (*SilverSource, bool) {
	for i := range c.SilverSources {
		if c.SilverSources[i].ID == id {
			return &c.SilverSources[i], true
		}
	}
	return nil, false
}

