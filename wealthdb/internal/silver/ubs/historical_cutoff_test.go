package ubs

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// TestHistoricalCutoffDropsPSNOverlap locks in the fix for the
// gold "duplicate (source, snapshot_at, safekeeping, isin)" crash
// on quarter-end days that both a UBS PDF Statement of Assets AND
// PSN's nightly report cover. Historical is the pre-PSN backfill;
// once PSN's coverage laps a statement's period-end, the two emit
// PositionChange rows under the same key.
//
// Historical portfolio_external_id is the PSN-aligned long form
// (BBBBAAAAAAAANN); the resolver builds the portfolio cutoff by
// joining PSN silver's portfolios table (which owns that id space)
// rather than live web's portfolios (which uses a distinct 4-char
// id space and would miss every historical row).
func TestHistoricalCutoffDropsPSNOverlap(t *testing.T) {
	ctx := context.Background()

	// PSN silver: one portfolio, one cash_account under SFTPCH01
	// with MIN(snapshot_at) = 1500 (the PSN start).
	psnPath, psnDB := newFixtureSilver(t)
	if _, err := psnDB.Exec(`
        INSERT INTO portfolios(snapshot_at, relationship_id, portfolio_external_id, payload, base_currency)
        VALUES (1500, 'SFTPCH01', '0999AAAAAAAA02', '{}', 'CHF');
        INSERT INTO cash_accounts(snapshot_at, relationship_id, account_external_id, payload)
        VALUES (1500, 'SFTPCH01', 'CH99XXXX0000000000001', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	_ = psnPath

	// Web silver: portfolios/accounts tables carry banking_
	// relationship_id (live web id space, only used for cash
	// resolution). Historical securities span pre- and post-cutoff.
	web := newWebFixture(t)
	if _, err := web.db.ExecContext(ctx, `
        CREATE TABLE portfolios (
            snapshot_at INTEGER, portfolio_external_id TEXT,
            banking_relationship_id TEXT, portfolio_full_id TEXT,
            portfolio_uid TEXT, base_currency TEXT, description TEXT,
            payload TEXT);
        CREATE TABLE accounts (
            snapshot_at INTEGER, account_external_id TEXT, kind TEXT,
            iban TEXT, account_acct_id_psn_form TEXT, account_number_raw TEXT,
            account_opaque_id TEXT, banking_relationship_id TEXT,
            portfolio_external_id TEXT, currency_iso TEXT,
            description TEXT, payload TEXT);
        INSERT INTO accounts(snapshot_at, account_external_id, kind,
            banking_relationship_id, payload)
        VALUES (1000, 'CH99XXXX0000000000001', 'cash',
                '0999 AAAAAAAA', '{}');
        INSERT INTO historical_position_snapshots
            (as_of_date, portfolio_external_id, account_external_id,
             instrument_isin, currency_iso, market_value,
             market_value_currency, source_doc_token, payload)
        VALUES
            (1000, '0999AAAAAAAA02', '', 'CH0000000001', 'CHF', 100, 'CHF', 'tok', '{}'),
            (2000, '0999AAAAAAAA02', '', 'CH0000000001', 'CHF', 110, 'CHF', 'tok', '{}');
        INSERT INTO historical_cash_balances
            (period_end, period_start, account_external_id, currency_iso,
             opening_balance, closing_balance, source_doc_token, payload)
        VALUES
            (1000, 900, 'CH99XXXX0000000000001', 'CHF', 50, 60, 'tok', '{}'),
            (2000, 1900, 'CH99XXXX0000000000001', 'CHF', 60, 70, 'tok', '{}');
    `); err != nil {
		t.Fatal(err)
	}

	rels := []silver.RelationshipPair{{WebID: "0999 AAAAAAAA", PSNID: "SFTPCH01"}}
	psn := &psnReader{db: psnDB}
	cutoffByWebRel := map[string]int64{"0999 AAAAAAAA": 1500}

	portfolioCutoff, accountCutoff, err := web.buildHistoricalCutoffs(ctx, cutoffByWebRel, psn, rels)
	if err != nil {
		t.Fatal(err)
	}
	if got := portfolioCutoff["0999AAAAAAAA02"]; got != 1500 {
		t.Errorf("portfolioCutoff = %d, want 1500", got)
	}
	if got := accountCutoff["CH99XXXX0000000000001"]; got != 1500 {
		t.Errorf("accountCutoff = %d, want 1500", got)
	}

	w := canonical.Window{Start: 0, End: 3000, HasChanges: true}
	stream, err := web.snapshotsHistorical(ctx, w, nil, portfolioCutoff, accountCutoff)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	posBySnap := map[int64]int{}
	cashBySnap := map[int64]int{}
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range batch.Positions {
			posBySnap[p.SnapshotAt]++
		}
		for _, cb := range batch.CashBalances {
			cashBySnap[cb.SnapshotAt]++
		}
		if !more {
			break
		}
	}
	if posBySnap[1000] != 1 {
		t.Errorf("pre-cutoff position at t=1000 dropped (got %d, want 1)", posBySnap[1000])
	}
	if posBySnap[2000] != 0 {
		t.Errorf("post-cutoff position at t=2000 kept (got %d, want 0 — collides with PSN)", posBySnap[2000])
	}
	if cashBySnap[900]+cashBySnap[1000] != 2 {
		t.Errorf("pre-cutoff cash rows dropped: opening t=900=%d, closing t=1000=%d, want 1+1", cashBySnap[900], cashBySnap[1000])
	}
	if cashBySnap[1900]+cashBySnap[2000] != 0 {
		t.Errorf("post-cutoff cash rows kept: opening t=1900=%d, closing t=2000=%d, want 0", cashBySnap[1900], cashBySnap[2000])
	}
}
