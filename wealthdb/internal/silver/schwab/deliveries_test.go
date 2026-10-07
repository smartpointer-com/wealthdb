package schwab

import (
	"context"
	"strconv"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestReceiveAndDeliverKinds: RECEIVE_AND_DELIVER always carries a zero
// net amount, so its kind comes from the description and the security
// leg. A row with a corporate-action marker, and every row booked with
// it on the same account at the same instant, is a corporate action.
// Legs of one instrument whose quantities cancel at one instant move
// shares between sub-accounts, a journal. Every other row is a
// delivery in or out by the sign of its quantity.
func TestReceiveAndDeliverKinds(t *testing.T) {
	path, seed := newFixtureSilver(t)
	leg := func(id string, ts int, acct, desc, sym string, qty string) string {
		return `('` + id + `', ` + strconv.Itoa(ts) + `, '` + acct + `', 'RECEIVE_AND_DELIVER',
             '{"netAmount":0.0,"description":"` + desc + `","transferItems":[
                {"instrument":{"assetType":"EQUITY","symbol":"` + sym + `"},
                 "amount":` + qty + `,"cost":0.0,"price":0.0}]}')`
	}
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ` + leg("RS-NEW", 1100, "ACC", "EXAMPLE CORP", "XMPL", "10") + `,
            ` + leg("RS-OLD", 1100, "ACC", "EXAMPLE CORP XXXREVERSE SPLIT EFF", "XMPLOLD", "-100") + `,
            ` + leg("FWD", 1200, "ACC", "EXAMPLE ETF FORWARD SPLIT WITH STOCK SPLIT SHARES", "XETF", "50") + `,
            ` + leg("EXP", 1300, "ACC", "Removed due to Expiration CALL EXAMPLE CORP $9 EXP 01/15/99", "XMPL  990115C00009000", "-1") + `,
            ` + leg("MV-OUT", 1400, "ACC", "EXAMPLE CORP CLASS A", "XMPL", "-25") + `,
            ` + leg("MV-IN", 1400, "ACC", "EXAMPLE CORP CLASS A", "XMPL", "25") + `,
            ` + leg("DLV-IN", 1500, "ACC", "EXAMPLE CORP CLASS A", "XMPL", "40") + `,
            ` + leg("MV2-OUT", 1600, "ACC", "EXAMPLE CORP CLASS A", "XMPL", "-40") + `,
            ` + leg("MV2-IN", 1600, "ACC", "EXAMPLE CORP CLASS A", "XMPL", "40") + `,
            ` + leg("OTHER-ACCT", 1100, "ACC2", "EXAMPLE CORP", "XMPL", "10") + `,
            ` + leg("DLV-OUT", 1700, "ACC", "EXAMPLE CORP", "XMPL", "-5") + `;
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]canonical.TxKind{}
	for _, tx := range batch.Transactions {
		got[tx.TransactionExternalID] = tx.Kind
	}
	want := map[string]canonical.TxKind{
		"RS-NEW":     canonical.TxKindCorporateAction,
		"RS-OLD":     canonical.TxKindCorporateAction,
		"FWD":        canonical.TxKindCorporateAction,
		"EXP":        canonical.TxKindCorporateAction,
		"MV-OUT":     canonical.TxKindJournal,
		"MV-IN":      canonical.TxKindJournal,
		"DLV-IN":     canonical.TxKindTransferIn,
		"MV2-OUT":    canonical.TxKindJournal,
		"MV2-IN":     canonical.TxKindJournal,
		"OTHER-ACCT": canonical.TxKindTransferIn,
		"DLV-OUT":    canonical.TxKindTransferOut,
	}
	for id, k := range want {
		if got[id] != k {
			t.Errorf("%s: kind = %q, want %q", id, got[id], k)
		}
	}
}

func TestCorporateActionDescription(t *testing.T) {
	for d, want := range map[string]bool{
		"EXAMPLE CORP XXXREVERSE SPLIT EFF":                 true,
		"EXAMPLE ETF FORWARD SPLIT WITH STOCK SPLIT SHARES": true,
		"EXAMPLE FUND XXXMANDATORY MERGER EFF":              true,
		"Removed due to Expiration CALL EXAMPLE CORP $9":    true,
		"EXAMPLE CORP SPIN-OFF":                             true,
		"EXAMPLE CORP CLASS A":                              false,
		"EXAMPLE SPLITROCK HOLDINGS":                        false,
		"":                                                  false,
	} {
		if got := isCorporateActionDescription(d); got != want {
			t.Errorf("isCorporateActionDescription(%q) = %v, want %v", d, got, want)
		}
	}
}
