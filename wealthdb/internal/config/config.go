// Package config parses the wealthdb JSON config file. See
// docs/DESIGN.md §5 for the schema and defaults.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ptu/wealthdb/internal/silver"
)

// Config is the in-memory shape of the wealthdb config file.
// Field paths in the file are tilde / $HOME-expanded at Load time;
// callers always see absolute paths.
type Config struct {
	GoldDB          string         `json:"gold_db"`
	DefaultCurrency string         `json:"default_currency"`
	SilverSources   []SilverSource `json:"silver_sources"`
	// AccountOverrides lets the user override the per-account
	// `nickname` and `account_category` columns adapters would
	// otherwise emit. Keyed by silver_source_id (outer) and then
	// account_external_id (inner). Either field of the value may
	// be empty/omitted; an empty value is treated as "no override
	// for that column". The loader applies overrides AFTER the
	// adapter has stamped its own values, so config wins on
	// overlap. See docs/DESIGN.md §13.9.
	AccountOverrides map[string]map[string]AccountOverride `json:"account_overrides,omitempty"`
	// SymbolResolution groups the per-deployment knobs that drive
	// `wealthdb resolve-symbols`: the LLM endpoint and the
	// user-authored override list. Both fields inside are optional;
	// the subcommand fails loudly if Model is unset and
	// --overrides-only wasn't passed.
	SymbolResolution *SymbolResolutionConfig `json:"symbol_resolution,omitempty"`
}

// SymbolResolutionConfig is the `symbol_resolution` block of
// wealthdb.cfg. Groups everything specific to the resolve-symbols
// subcommand so the top-level config file doesn't sprout one
// field per concern.
type SymbolResolutionConfig struct {
	// Model configures the LLM endpoint used to back-fill missing
	// instrument tickers. Required for normal `resolve-symbols`
	// runs; can be omitted when only --overrides-only is used.
	Model *ModelConfig `json:"model,omitempty"`
	// Overrides is the user-authored ticker-mapping override list.
	// Each entry replaces (or suppresses) a row in
	// symbol_resolutions under model_name='manual-override'.
	// Applied at the start of every `wealthdb resolve-symbols`
	// invocation (including --overrides-only). Use cases:
	// correcting an LLM resolution that was wrong, or seeding
	// tickers the LLM can't infer (e.g. private-fund proxies).
	Overrides []SymbolOverride `json:"overrides,omitempty"`
}

// SymbolOverride is one entry under `symbol_resolution.overrides`.
// Mirrors the symbol_resolutions PK + value columns. Two modes:
//
//   - Correction: set `symbol` to the right ticker. The sync UPSERTs
//     this row into symbol_resolutions, winning over any LLM result.
//   - Suppression: set `delete: true` (and omit `symbol`). The sync
//     DELETEs any row with this PK from symbol_resolutions. Use for
//     descriptions where no real ticker exists (US Treasury CUSIPs,
//     private structured products, currency-line placeholders) so
//     the LLM's wrong guess stops surfacing.
type SymbolOverride struct {
	SilverSourceID string `json:"silver_source_id"`
	LookupKind     string `json:"lookup_kind"` // 'instrument_external_id' or 'name'
	LookupValue    string `json:"lookup_value"`
	Symbol         string `json:"symbol,omitempty"`
	Delete         bool   `json:"delete,omitempty"`
}

// ModelConfig is the `symbol_resolution.model` block of
// wealthdb.cfg. The only API shape supported today is the
// OpenAI-compatible Chat Completions endpoint
// (`api: "openai-completions"`); ThinkingFormat lets the
// resolve-symbols pipeline strip R1-style `<think>` blocks from
// the response before parsing.
type ModelConfig struct {
	BaseURL        string `json:"baseUrl"`
	API            string `json:"api"`
	APIKey         string `json:"apiKey,omitempty"`
	Name           string `json:"name"`
	ThinkingFormat string `json:"thinkingFormat,omitempty"`
}

// SilverSource is one entry under `silver_sources` in the config
// file. Most sources are single-file: set `path`. Multi-source
// adapters (UBS = web + PSN) instead set `subsources`, with one
// entry per backing silver. Each subsource is optional; at least
// one must be present when `subsources` is used. Path is empty
// in the multi-source form.
type SilverSource struct {
	ID         string             `json:"id"`
	Kind       string             `json:"kind"`
	Path       string             `json:"path,omitempty"`
	Subsources []SilverSubsource  `json:"subsources,omitempty"`
	// Relationships pairs cross-subsource entity identities under
	// a single user-chosen label. Used by the UBS adapter to link
	// the web `banking_relationship_id` (opaque SPA token) to the
	// PSN `relationship_id` (SFTPCHxx, etc.). Optional.
	Relationships []RelationshipPair `json:"relationships,omitempty"`
}

// SilverSubsource is one entry under `silver_sources[].subsources`.
type SilverSubsource struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// RelationshipPair is one entry under `silver_sources[].relationships`.
// At least one of `web_id` or `psn_id` must be set. `label` is
// the canonical user-readable name the adapter stamps on canonical
// records. `psn_start_override`, when set (YYYY-MM-DD), overrides
// the auto-detected cutover date used to splice web↔PSN
// transactions for this relationship.
type RelationshipPair struct {
	Label            string `json:"label"`
	WebID            string `json:"web_id,omitempty"`
	PSNID            string `json:"psn_id,omitempty"`
	PSNStartOverride string `json:"psn_start_override,omitempty"`
}

// AccountOverride is one per-account override entry. All fields
// are optional; an empty string means "don't override that
// column". TaxWrapper and ManagementStyle are validated against
// the canonical enums at config-load time; bad values fail the
// load rather than landing as gibberish in gold.
type AccountOverride struct {
	Nickname        string `json:"nickname,omitempty"`
	Category        string `json:"category,omitempty"`
	TaxWrapper      string `json:"tax_wrapper,omitempty"`
	ManagementStyle string `json:"management_style,omitempty"`
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
		if c.SilverSources[i].Path != "" {
			expanded, err := expandPath(c.SilverSources[i].Path, configDir)
			if err != nil {
				return nil, fmt.Errorf("config: silver_sources[%d].path: %w", i, err)
			}
			c.SilverSources[i].Path = expanded
		}
		for j := range c.SilverSources[i].Subsources {
			expanded, err := expandPath(c.SilverSources[i].Subsources[j].Path, configDir)
			if err != nil {
				return nil, fmt.Errorf("config: silver_sources[%d].subsources[%d].path: %w", i, j, err)
			}
			c.SilverSources[i].Subsources[j].Path = expanded
		}
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

// ToSilverOpenSpec converts a config.SilverSource into the
// silver.OpenSpec the adapter contract expects. Lives here (not
// in silver) so config carries the JSON tags / parse logic and
// silver stays JSON-free. Returns an error when a relationship's
// psn_start_override fails to parse (YYYY-MM-DD).
func (s *SilverSource) ToSilverOpenSpec() (silver.OpenSpec, error) {
	out := silver.OpenSpec{Path: s.Path}
	for _, sub := range s.Subsources {
		out.Subsources = append(out.Subsources, silver.Subsource{
			Kind: sub.Kind,
			Path: sub.Path,
		})
	}
	for _, rel := range s.Relationships {
		var override int64
		if rel.PSNStartOverride != "" {
			t, err := parseYYYYMMDD(rel.PSNStartOverride)
			if err != nil {
				return silver.OpenSpec{}, fmt.Errorf(
					"silver_sources[%q].relationships[%q].psn_start_override: %w",
					s.ID, rel.Label, err)
			}
			override = t
		}
		out.Relationships = append(out.Relationships, silver.RelationshipPair{
			Label:            rel.Label,
			WebID:            rel.WebID,
			PSNID:            rel.PSNID,
			PSNStartOverride: override,
		})
	}
	return out, nil
}

// parseYYYYMMDD turns a YYYY-MM-DD string into a Unix-seconds
// timestamp at UTC midnight.
func parseYYYYMMDD(s string) (int64, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, fmt.Errorf("invalid YYYY-MM-DD %q: %w", s, err)
	}
	return t.UTC().Unix(), nil
}

