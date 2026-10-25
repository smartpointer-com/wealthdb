package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// A source's `schwab` object reaches the loader's SourceSpec, which hands
// it to the adapter's OpenSpec: the guard a load opens with is the
// configured one, and a source without the object states none.
func TestBuildSourceSpecForwardsTheMarkGuard(t *testing.T) {
	t.Parallel()
	spike, revert := 15.0, 1.5
	cfg := &config.Config{}
	spec, err := buildSourceSpec(config.SilverSource{
		ID: "s", Kind: "schwab", Path: "/tmp/s.db",
		Schwab: &config.SchwabSettings{MarkSpikePct: &spike, MarkRevertPct: &revert},
	}, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (silver.MarkGuard{SpikePct: 15, RevertPct: 1.5}); spec.MarkGuard != want {
		t.Errorf("MarkGuard = %+v, want %+v", spec.MarkGuard, want)
	}
	spec, err = buildSourceSpec(config.SilverSource{ID: "s", Kind: "schwab", Path: "/tmp/s.db"}, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.MarkGuard != (silver.MarkGuard{}) {
		t.Errorf("MarkGuard = %+v for a source with no schwab object, want the zero guard", spec.MarkGuard)
	}
}

// The class pass keeps the pair the config pins with instrument_overrides
// and reads OpenFIGI's type over an unpinned instrument of a schwab
// source. Every identifier and name is invented.
func TestRunClassPassKeepsConfigPins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
			high_watermark, first_loaded_at, last_loaded_at)
		VALUES ('brk', 'schwab', '/tmp/brk.db', -1, 0, 0);
		INSERT INTO instruments(silver_source_id, instrument_external_id, asset_class, vehicle,
			symbol, name, currency, first_seen_at, last_seen_at)
		VALUES ('brk', 'PINNED', 'public_equity', 'stock', 'EXPN', 'EXAMPLE BITCOIN TRUST', 'USD', 0, 0),
		       ('brk', 'FREE',   'public_equity', 'stock', 'EXFR', 'EXAMPLE BITCOIN TRUST', 'USD', 0, 0);
		INSERT INTO security_identities(silver_source_id, security_key, as_of, provider, security_type, payload)
		VALUES ('refdata', 'symbol:EXPN', 100, 'openfigi', 'Mutual Fund', '{"found": true}'),
		       ('refdata', 'symbol:EXFR', 100, 'openfigi', 'Mutual Fund', '{"found": true}')`); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{InstrumentOverrides: map[string]map[string]config.InstrumentOverride{
		"brk": {"PINNED": {AssetClass: "public_equity", Vehicle: "stock"}},
	}}
	if got := pinnedInstruments(cfg); len(got) != 1 || !got["brk"]["PINNED"] || len(got["brk"]) != 1 {
		t.Fatalf("pinnedInstruments = %v, want brk/PINNED only", got)
	}
	if err := runClassPass(ctx, db, cfg); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT instrument_external_id, asset_class || ' ' || vehicle FROM instruments`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, pair string
		if err := rows.Scan(&id, &pair); err != nil {
			t.Fatal(err)
		}
		got[id] = pair
	}
	if got["PINNED"] != "public_equity stock" {
		t.Errorf("the pinned instrument reads %q, want its pinned pair", got["PINNED"])
	}
	if got["FREE"] != "crypto fund" {
		t.Errorf("the unpinned instrument reads %q, want crypto fund", got["FREE"])
	}
}
