package ubs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// RelationshipPair is one relationship of the source, as parseSettings
// compiled it from the config.
// Either WebID or PSNID (or both) must be set; only the
// configured side is used.
type RelationshipPair struct {
	// Label is the canonical (user-readable) name the adapter
	// stamps on canonical records so downstream sees a single key
	// regardless of which subsource produced the record.
	Label string
	// WebID, when set, is the web silver's banking_relationship_id.
	WebID string
	// PSNID, when set, is the PSN silver's relationship_id.
	PSNID string
	// PSNStartOverride, when non-zero, overrides the auto-detected
	// PSN-start cutover date used to splice transactions between
	// web and PSN. Unix seconds UTC. Zero = auto-detect.
	PSNStartOverride int64
}

// relationshipEntry is one entry under `silver_sources[].relationships`
// in the config. At least one of `web_id` or `psn_id` must be set.
// `label` is the display name the adapter stamps on canonical records.
// `psn_start_override`, when set (YYYY-MM-DD), overrides the
// auto-detected cutover date used to splice web↔PSN transactions for
// this relationship.
type relationshipEntry struct {
	Label            string `json:"label"`
	WebID            string `json:"web_id,omitempty"`
	PSNID            string `json:"psn_id,omitempty"`
	PSNStartOverride string `json:"psn_start_override,omitempty"`
}

// parseSettings reads a source's `relationships` array
// (silver.Settings): each entry's label, at least one of its ids, and
// its psn_start_override as the epoch of the day at UTC midnight. A
// source of any kind may carry it; only a ubs source reads it.
func parseSettings(raw json.RawMessage, _ string) (any, error) {
	var entries []relationshipEntry
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("relationships: %w", err)
	}
	var out []RelationshipPair
	for j, e := range entries {
		if e.Label == "" {
			return nil, fmt.Errorf("relationships[%d].label is required", j)
		}
		if e.WebID == "" && e.PSNID == "" {
			return nil, fmt.Errorf("relationships[%d]: at least one of web_id or psn_id must be set", j)
		}
		rel := RelationshipPair{Label: e.Label, WebID: e.WebID, PSNID: e.PSNID}
		if e.PSNStartOverride != "" {
			t, err := time.Parse("2006-01-02", e.PSNStartOverride)
			if err != nil {
				return nil, fmt.Errorf("relationships[%d].psn_start_override: invalid YYYY-MM-DD %q: %w", j, e.PSNStartOverride, err)
			}
			rel.PSNStartOverride = t.UTC().Unix()
		}
		out = append(out, rel)
	}
	return out, nil
}
