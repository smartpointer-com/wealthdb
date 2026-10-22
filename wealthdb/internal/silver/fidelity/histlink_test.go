package fidelity

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestHistoricalRowsLinkToTheirKeys pins the roads a keyless statement
// row takes to the key the rest of the silver names its security by
// (histlink.go), and the rows that keep their synthetic key. Each case
// fails without the rule it names.
func TestHistoricalRowsLinkToTheirKeys(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 4, 'supplied'), (5000, 4, '/x/5');
        INSERT INTO portfolios(snapshot_at, portfolio_external_id, kind, payload) VALUES (5000, 'P-DAF', 'daf', '{}');
        INSERT INTO accounts(snapshot_at, account_external_id, portfolio_external_id, payload) VALUES (5000, '200000001', 'P-DAF', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, quantity, current_value, payload) VALUES
            (5000, '100000001', 'EXMP', 'EXAMPLE CORP COM', 1, 10.00, '{}'),
            (5000, '100000001', 'EXADY', 'EXAMPLE INDUSTRIES AG UNSP ADR EACH REP 2 ORD', 1, 10.00, '{}'),
            (5000, '100000001', 'ABC000001', 'STATE PLAN 2030 (EXAMPLE BLEND)', 1, 10.00, '{}'),
            (5000, '100000001', 'TWNA', 'EXAMPLE TWIN CO CL A', 1, 10.00, '{}'),
            (5000, '100000001', 'TWNB', 'EXAMPLE TWIN CO CL B', 1, 10.00, '{}'),
            (5000, '100000001', 'EXTCY', 'EXAMPLE TRADING CO SPON ADS EACH REP 1 ORD SHS', 5, 50.00, '{}'),
            (5000, '100000001', 'EXPM', 'EXAMPLE PAPER MILLS CO', 1, 10.00, '{}'),
            (5000, '100000001', 'EXSH', 'EXAMPLE SHORT', 1, 10.00, '{}'),
            (5000, '100000001', 'EXSCX', 'EXAMPLE SHARE CO', 1, 10.00, '{}'),
            (5000, '100000001', 'EXNCX', 'NET CASH POSITION FUND', 1, 10.00, '{}'),
            (5000, '200000001', 'POOL1', 'EXAMPLE CHARITABLE GROWTH POOL', 1, 10.00, '{}');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, quantity, amount, payload) VALUES
            ('t1', 500, '100000001', 'YOU', 'TCKR', 5, -50, '{"Description": "EXAMPLE REIT INC COM"}'),
            ('t2', 500, '100000001', 'YOU', '000000001', 5, -50, '{"description": "EXAMPLE TRADING CO SPON ADS EACH REP 1 ORD "}'),
            ('t3', 2500, '100000001', 'MERGER', '000000001', -5, 0,
             '{"Action": "MERGER MER PAYOUT #REOR M0000000000001 EXAMPLE TRADING CO", "Description": "EXAMPLE TRADING CO SPON ADS EACH REP 1 ORD "}'),
            ('t4', 2500, '100000001', 'MERGER', 'EXTCY', 5, 0,
             '{"ACTION": "MERGER MER FROM 000000001#REOR M0000000000002 EXAMPLE TRADING CO SPONSORED ADR", "Description": "EXAMPLE TRADING CO SPONSORED ADR"}'),
            ('t5', 500, '100000002', 'YOU', '000000002', 3, -30, '{"Description": "EXAMPLE MOTORS AG ADR EACH RE"}'),
            ('t6', 500, '100000002', 'YOU', '000ADR002', 3, -30, '{"Description": "EXAMPLE MOTORS AG ADR EACH RE"}');
        INSERT INTO historical_position_snapshots(as_of_date, account_external_id, description, instrument_key, quantity, price, market_value, currency, payload) VALUES
            (1000, '100000001', 'EXAMPLE REIT INC COM USD0.01 (TCKR) -', NULL, 5, 10.0, 50.00, 'USD', '{}'),
            (2000, '100000001', 'EXAMPLE REIT INC COM USD0.01 (TCKR) - 3 of 9', NULL, 5, 10.0, 50.00, 'USD', '{}'),
            (2000, '100000001', 'EXAMPLE REIT INC COM USD0.01 (TCKR) - 4 of 9', NULL, 5, 10.0, 50.00, 'USD', '{}'),
            (1000, '100000001', 'EXAMPLE INDUSTRIES AG UNSP ADR EACH REP', NULL, 2, 10.0, 20.00, 'USD', '{}'),
            (2000, '100000001', 'EXAMPLE INDUSTRIES AG UNSP ADR EACH REP - 3 of 9', NULL, 2, 10.0, 20.00, 'USD', '{}'),
            (1000, '100000001', 'EXAMPLE PAPER MILLS CO COM NEW', NULL, 2, 10.0, 20.00, 'USD', '{}'),
            (2000, '100000001', 'EXAMPLE TRADING CO SPON ADS EACH REP', NULL, 5, 10.0, 50.00, 'USD', '{}'),
            (2500, '100000001', 'EXAMPLE TRADING CO SPON ADS EACH REP', NULL, 5, 10.0, 50.00, 'USD', '{}'),
            (1000, '100000001', 'STATE PLAN 2030 (EXAMPLE FUNDS)', NULL, 3, 10.0, 30.00, 'USD', '{}'),
            (1000, '100000001', 'EXAMPLE TWIN CO CL', NULL, 3, 10.0, 30.00, 'USD', '{}'),
            (1000, '100000001', 'EXAMPLE SHORT CO SPON ADR', NULL, 3, 10.0, 30.00, 'USD', '{}'),
            (1000, '100000001', 'EXAMPLE SHORT', NULL, 3, 10.0, 30.00, 'USD', '{}'),
            (1000, '100000001', 'EXAMPLE SHARE CO K6 (EXSKX)', NULL, 3, 10.0, 30.00, 'USD', '{}'),
            (1000, '100000001', 'Growth', NULL, 3, 10.0, 30.00, 'USD', '{}'),
            (1000, '100000001', 'EXAMPLE UNKNOWN CO COM (ZZZZ)', NULL, 3, 10.0, 30.00, 'USD', '{}'),
            (1000, '100000001', 'NET CASH POSITION', NULL, NULL, NULL, 30.00, 'USD', '{}'),
            (1000, '100000001', 'EXAMPLE CORP COM', 'EXMP', 1, 10.0, 10.00, 'USD', '{}'),
            (1000, '100000001', 'EXAMPLE CORP COM CL A (EXMP)', NULL, 1, 10.0, 10.00, 'USD', '{}'),
            (1000, '100000003', 'CALL (EXMP) EXAMPLE CORP JAN 21 26 $20', NULL, 1, 1.0, 1.00, 'USD', '{}'),
            (1000, '100000002', 'EXAMPLE MOTORS AG ADR EACH REPR 0.5 SHS SPONSORED', 'EXMOY', 3, 10.0, 30.00, 'USD', '{}'),
            (2000, '100000001', 'EXAMPLE MOTORS AG ADR EACH REPR 0.5 SHS SPONSORED', NULL, 3, 10.0, 30.00, 'USD', '{}'),
            (1000, '100000002', 'EXAMPLE GROWTH FUND (EXGFX)', 'EXGFX', 3, 10.0, 30.00, 'USD', '{}'),
            (2000, '100000002', 'EXAMPLE GROWTH FUND (EXGFX) - 3 of 9', NULL, 3, 10.0, 30.00, 'USD', '{}'),
            (1000, '200000001', 'EXAMPLE CHARITABLE GROWTH POOL', NULL, 3, 10.0, 30.00, 'USD', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	type row struct {
		at         int64
		acct, desc string
	}
	type instrumentAt struct {
		at  int64
		key string
	}
	got := map[row]string{}
	instruments := map[instrumentAt]canonical.InstrumentChange{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range batch.Instruments {
			instruments[instrumentAt{in.FirstSeenAt, in.InstrumentExternalID}] = in
		}
		for _, p := range batch.Positions {
			if p.SnapshotAt == 5000 {
				continue
			}
			got[row{p.SnapshotAt, p.AccountExternalID, *instruments[instrumentAt{p.SnapshotAt, p.PositionKey}].Name}] = p.PositionKey
		}
		if !more {
			break
		}
	}
	synthetic := syntheticHistoricalInstrumentKey
	for _, c := range []struct {
		at          int64
		acct, name  string
		key, reason string
	}{
		{1000, "100000001", "EXAMPLE REIT INC COM USD0.01 (TCKR)", "TCKR", "printed ticker, trailing dash cleaned"},
		{2000, "100000001", "EXAMPLE REIT INC COM USD0.01 (TCKR)", "TCKR", "printed ticker, page footer cleaned"},
		{2000, "100000001", "EXAMPLE REIT INC COM USD0.01 (TCKR) - 4 of 9", synthetic("EXAMPLE REIT INC COM USD0.01 (TCKR) - 4 of 9"), "the day's statement already holds the key"},
		{1000, "100000001", "EXAMPLE INDUSTRIES AG UNSP ADR EACH REP", "EXADY", "a cut name starts a live name"},
		{2000, "100000001", "EXAMPLE INDUSTRIES AG UNSP ADR EACH REP", "EXADY", "a cut name starts a live name, page footer cleaned"},
		{1000, "100000001", "EXAMPLE PAPER MILLS CO COM NEW", "EXPM", "a name starts the description"},
		{2000, "100000001", "EXAMPLE TRADING CO SPON ADS EACH REP", "000000001", "the chain's head before the merger"},
		{2500, "100000001", "EXAMPLE TRADING CO SPON ADS EACH REP", "EXTCY", "the successor from the merger's day"},
		{2000, "100000001", "EXAMPLE MOTORS AG ADR EACH REPR 0.5 SHS SPONSORED", "EXMOY", "the one key of the exact name"},
		{1000, "100000001", "EXAMPLE CORP COM", "EXMP", "a keyed row"},
		{1000, "100000001", "STATE PLAN 2030 (EXAMPLE FUNDS)", synthetic("STATE PLAN 2030 (EXAMPLE FUNDS)"), "no name answers"},
		{1000, "100000001", "EXAMPLE TWIN CO CL", synthetic("EXAMPLE TWIN CO CL"), "two securities answer"},
		{1000, "100000001", "EXAMPLE SHORT CO SPON ADR", synthetic("EXAMPLE SHORT CO SPON ADR"), "the matching name is under three words"},
		{1000, "100000001", "EXAMPLE SHORT", synthetic("EXAMPLE SHORT"), "the exact name is under three words"},
		{1000, "100000001", "EXAMPLE SHARE CO K6 (EXSKX)", synthetic("EXAMPLE SHARE CO K6 (EXSKX)"), "a printed ticker no key carries takes no prefix road"},
		{1000, "100000001", "Growth", synthetic("Growth"), "a one-word name"},
		{1000, "100000001", "EXAMPLE UNKNOWN CO COM (ZZZZ)", synthetic("EXAMPLE UNKNOWN CO COM (ZZZZ)"), "an unknown ticker"},
		{1000, "100000001", "NET CASH POSITION", synthetic("NET CASH POSITION"), "a statement construct"},
		{1000, "100000001", "EXAMPLE CORP COM CL A (EXMP)", synthetic("EXAMPLE CORP COM CL A (EXMP)"), "the account's statement holds the key"},
		{1000, "100000003", "CALL (EXMP) EXAMPLE CORP JAN 21 26 $20", synthetic("CALL (EXMP) EXAMPLE CORP JAN 21 26 $20"), "an option leg"},
		{1000, "200000001", "EXAMPLE CHARITABLE GROWTH POOL", synthetic("EXAMPLE CHARITABLE GROWTH POOL"), "a donor-advised fund's pool"},
		{2000, "100000002", "EXAMPLE GROWTH FUND (EXGFX)", "EXGFX", "printed ticker of a keyed statement row"},
	} {
		if k := got[row{c.at, c.acct, c.name}]; k != c.key {
			t.Errorf("%d %s %q (%s): key %q, want %q", c.at, c.acct, c.name, c.reason, k, c.key)
		}
	}
	if in := instruments[instrumentAt{2000, "000000001"}]; in.AssetClass != canonical.AssetClassPublicEquity {
		t.Errorf("a share linked to its CUSIP reads %q, want public_equity", in.AssetClass)
	}
	keyed, linked := instruments[instrumentAt{1000, "EXGFX"}], instruments[instrumentAt{2000, "EXGFX"}]
	if keyed.AssetClass != linked.AssetClass || keyed.Vehicle != linked.Vehicle || linked.Vehicle != canonical.VehicleFund {
		t.Errorf("a fund linked by its ticker reads %s/%s, its keyed row %s/%s: want both a fund",
			linked.AssetClass, linked.Vehicle, keyed.AssetClass, keyed.Vehicle)
	}
}

// TestHistoricalDescription pins what a statement row's description
// reads as once the page artifacts are off.
func TestHistoricalDescription(t *testing.T) {
	for in, want := range map[string]string{
		"EXAMPLE CO COM (EXMP) -":         "EXAMPLE CO COM (EXMP)",
		"EXAMPLE CO COM (EXMP) - 3 of 9":  "EXAMPLE CO COM (EXMP)",
		"EXAMPLE CO SPON ADS EACH REP...": "EXAMPLE CO SPON ADS EACH REP",
		"EXAMPLE  CO   COM ":              "EXAMPLE CO COM",
		"STATE PLAN 2030 (EXAMPLE FUNDS)": "STATE PLAN 2030 (EXAMPLE FUNDS)",
	} {
		if got := historicalDescription(in); got != want {
			t.Errorf("historicalDescription(%q) = %q, want %q", in, got, want)
		}
	}
}
