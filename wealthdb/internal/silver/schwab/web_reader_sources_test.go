package schwab

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// wtxAt builds a parsed web transaction at an explicit epoch-second timestamp
// (not an epoch-day like wtx), with an optional raw silver payload, for the
// 1099-B-authority and distribution-dedup stage tests. Those stages read
// payload.tax_year / payload.transfer_kind and bucket sells by calendar year, so
// the tests need real timestamps and payloads rather than the day-scaled wtx.
func wtxAt(id, source string, kind canonical.TxKind, acct string, ts int64, amount float64, payload string) builtWebTx {
	d := canonical.NewDecimalFromFloat(amount)
	b := builtWebTx{source: source, tx: canonical.TransactionChange{
		TransactionExternalID: id,
		OccurredAt:            ts,
		AccountExternalID:     acct,
		Kind:                  kind,
		NetAmount:             &d,
	}}
	if payload != "" {
		b.tx.Payload = []byte(payload)
	}
	return b
}

// unixYear returns the UTC epoch seconds for noon on Jul 1 of the given year, a
// stable mid-year instant well away from any year boundary / TZ edge.
func unixYear(year int) int64 {
	return time.Date(year, time.July, 1, 12, 0, 0, 0, time.UTC).Unix()
}

// TestSupersedeSalesWith1099B pins INTEROP §8.1: within a covered (account,
// tax_year) the form_1099b lots win and the statement_pdf / tx_history_json sells
// in that calendar year are dropped, while sells outside covered years (and all
// non-sell rows) survive untouched, and a null cost_basis on a surviving 1099-B
// lot is preserved verbatim in the payload.
func TestSupersedeSalesWith1099B(t *testing.T) {
	const pdf, js, f1099 = sourceStatementPDF, sourceTxHistoryJSON, sourceFORM1099B

	cases := []struct {
		name    string
		in      []builtWebTx
		wantIDs []string
	}{
		{
			name: "1099-B supersedes statement and tx-history sells in a covered year",
			in: []builtWebTx{
				// Covered (A, 2022): one 1099-B lot establishes coverage.
				wtxAt("f-2022", f1099, canonical.TxKindSell, "A", unixYear(2022), 1000, `{"tax_year":2022,"security_name":"EXMPL ETF","cost_basis":800,"acquired_date":"2020-01-15"}`),
				// Same-year sells from the other two feeds are superseded.
				wtxAt("p-2022", pdf, canonical.TxKindSell, "A", unixYear(2022), 1000, ""),
				wtxAt("j-2022", js, canonical.TxKindSell, "A", unixYear(2022), 1000, ""),
				// A non-sell in the covered year is untouched.
				wtxAt("j-2022-div", js, canonical.TxKindDividend, "A", unixYear(2022), 12, ""),
			},
			wantIDs: []string{"f-2022", "j-2022-div"},
		},
		{
			name: "sell outside the covered tax year survives",
			in: []builtWebTx{
				wtxAt("f-2022", f1099, canonical.TxKindSell, "A", unixYear(2022), 1000, `{"tax_year":2022,"security_name":"VTI"}`),
				// 2021 is NOT covered for account A → the web sell stays.
				wtxAt("p-2021", pdf, canonical.TxKindSell, "A", unixYear(2021), 500, ""),
				// Different account, same year → not covered → stays.
				wtxAt("p-2022-B", pdf, canonical.TxKindSell, "B", unixYear(2022), 700, ""),
			},
			wantIDs: []string{"f-2022", "p-2021", "p-2022-B"},
		},
		{
			name: "null cost_basis lot is kept (basis stays null, never coerced)",
			in: []builtWebTx{
				wtxAt("f-null", f1099, canonical.TxKindSell, "A", unixYear(2023), 1500, `{"tax_year":2023,"security_name":"QQQ","cost_basis":null,"cost_basis_raw":0,"basis_not_shown":true,"noncovered":true}`),
				wtxAt("p-2023", pdf, canonical.TxKindSell, "A", unixYear(2023), 1500, ""),
			},
			wantIDs: []string{"f-null"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := supersedeSalesWith1099B(c.in)
			got := builtIDs(out)
			if len(got) != len(c.wantIDs) {
				t.Fatalf("survivors = %v, want %v", got, c.wantIDs)
			}
			for _, id := range c.wantIDs {
				if !got[id] {
					t.Errorf("expected %q to survive; survivors = %v", id, got)
				}
			}
			// The surviving 1099-B null-basis lot must keep its payload verbatim
			// (cost_basis still null — not coerced to 0).
			if c.name == "null cost_basis lot is kept (basis stays null, never coerced)" {
				if cb, ok := costBasisFromPayload(t, out, "f-null"); ok && cb != nil {
					t.Errorf("cost_basis must stay null, got %v", *cb)
				}
			}
		})
	}
}

// costBasisFromPayload finds the row with id and decodes payload.cost_basis,
// returning (value, foundRow). A nil *float64 with foundRow=true means the JSON
// key was present and null (the case the test asserts).
func costBasisFromPayload(t *testing.T, rows []builtWebTx, id string) (*float64, bool) {
	t.Helper()
	for _, r := range rows {
		if r.tx.TransactionExternalID != id {
			continue
		}
		var p struct {
			CostBasis *float64 `json:"cost_basis"`
		}
		if err := json.Unmarshal(r.tx.Payload, &p); err != nil {
			t.Fatalf("decode payload for %q: %v", id, err)
		}
		return p.CostBasis, true
	}
	return nil, false
}

// TestSupersedeStatementCashWithDistributions pins INTEROP §8.2 cash transfers: a
// third_party_distribution cash leg is authoritative over a matching statement /
// tx-history cash debit within the cross-feed tolerance (3 days / 0.5% / $1), so
// the statement/tx-history copies are dropped and only the distribution survives;
// a securities distribution is new data and never deduped; non-matching debits
// stay.
func TestSupersedeStatementCashWithDistributions(t *testing.T) {
	const pdf, js, dist = sourceStatementPDF, sourceTxHistoryJSON, sourceThirdPartyDistribution
	const day = int64(86400)

	// Guard the production cash-extraction path directly: a cash distribution
	// carries its magnitude under `cash_amount` (no plain `amount`), so
	// extractWebTxAmounts must read it — otherwise distByAcct stays empty and this
	// whole stage silently no-ops, re-introducing the double-count it exists to
	// prevent. The cases below build their distribution legs via
	// buildWebTxFromPayload (the real extraction path) so this stays pinned.
	if n, _, _ := extractWebTxAmounts(`{"transfer_kind":"cash","method":"wire","cash_amount":5000}`); n == nil {
		t.Fatal("extractWebTxAmounts must read cash_amount for cash distributions")
	}
	// Kinded the way production kinds them: since the "Transfer Out" split
	// a cash distribution books as a withdrawal, and a fixture holding the
	// old kind would be testing a combination the build loop no longer
	// produces. Pinned so it cannot drift back.
	if got := webKind("Transfer Out", nil, nil, `{"transfer_kind":"cash"}`); got != canonical.TxKindWithdrawal {
		t.Fatalf("a cash distribution now kinds as %q; these fixtures are stale", got)
	}

	cases := []struct {
		name    string
		in      []builtWebTx
		wantIDs []string
	}{
		{
			name: "cash distribution supersedes a matching statement debit (no double count)",
			in: []builtWebTx{
				buildWebTxFromPayload("d1", dist, canonical.TxKindWithdrawal, "A", 100*day, `{"transfer_kind":"cash","method":"wire","cash_amount":5000}`),
				wtxAt("p1", pdf, canonical.TxKindWithdrawal, "A", 100*day, -5000, ""),
			},
			wantIDs: []string{"d1"},
		},
		{
			name: "cash distribution supersedes both statement and tx-history copies",
			in: []builtWebTx{
				buildWebTxFromPayload("d1", dist, canonical.TxKindWithdrawal, "A", 100*day, `{"transfer_kind":"cash","method":"wire","cash_amount":5000}`),
				// settlement offset within the 3-day window
				wtxAt("p1", pdf, canonical.TxKindWithdrawal, "A", 102*day, -5000, ""),
				wtxAt("j1", js, canonical.TxKindWithdrawal, "A", 100*day, -5000, ""),
			},
			wantIDs: []string{"d1"},
		},
		{
			name: "securities distribution is new data, never deduped",
			in: []builtWebTx{
				wtxAt("d1", dist, canonical.TxKindTransferOut, "A", 100*day, -25000, `{"transfer_kind":"securities","symbol":"VTI"}`),
				// A statement debit of the same magnitude is NOT consumed by a
				// securities transfer (positions feed records the drop, not cash).
				wtxAt("p1", pdf, canonical.TxKindWithdrawal, "A", 100*day, -25000, ""),
			},
			wantIDs: []string{"d1", "p1"},
		},
		{
			name: "non-matching debit (beyond tolerance) is kept",
			in: []builtWebTx{
				buildWebTxFromPayload("d1", dist, canonical.TxKindWithdrawal, "A", 100*day, `{"transfer_kind":"cash","method":"wire","cash_amount":5000}`),
				wtxAt("p1", pdf, canonical.TxKindWithdrawal, "A", 100*day, -9999, ""),
			},
			wantIDs: []string{"d1", "p1"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := builtIDs(supersedeStatementCashWithDistributions(c.in))
			if len(got) != len(c.wantIDs) {
				t.Fatalf("survivors = %v, want %v", got, c.wantIDs)
			}
			for _, id := range c.wantIDs {
				if !got[id] {
					t.Errorf("expected %q to survive; survivors = %v", id, got)
				}
			}
		})
	}
}

// TestSecuritiesTransferIsExternalNetFlow pins INTEROP §8.2 securities transfers:
// a third_party_distribution securities row maps to TxKindTransferOut, which is in
// externalFlowKinds, so it moves net_flow. The row's net_flow magnitude comes from
// the payload's `market_value` (the documented contract carries no plain `amount`
// for these rows), so this test drives the *real* extraction path
// (extractWebTxAmounts + ApplyCanonicalSign, exactly as the build loop does) rather
// than hand-setting NetAmount — otherwise the assertion couldn't catch a regression
// where production never reads market_value. The row then survives the full
// assembly tail (cash dedup is a no-op for it, 1099-B authority ignores it, PDF↔JSON
// dedup ignores it) and contributes its market_value (negative for the outflow) to
// the summed NetAmount.
func TestSecuritiesTransferIsExternalNetFlow(t *testing.T) {
	const dist = sourceThirdPartyDistribution
	const day = int64(86400)

	if !externalFlowKinds[canonical.TxKindTransferOut] {
		t.Fatal("TxKindTransferOut must be an externalFlowKind so securities transfers move net_flow")
	}

	// Spec-shaped payload: market_value is the flow magnitude and there is NO
	// plain `amount` key (DESIGN.md §6). The NetAmount must therefore come out of
	// extractWebTxAmounts via market_value, not be supplied by the test.
	const payload = `{"transfer_kind":"securities","symbol":"EXMPL","quantity":100,"market_value":25000}`
	const kind = canonical.TxKindTransferOut

	// Guard the production extraction directly: market_value alone must yield a
	// non-nil magnitude.
	rawNet, _, _ := extractWebTxAmounts(payload)
	if rawNet == nil {
		t.Fatal("extractWebTxAmounts returned nil NetAmount for a market_value-only securities transfer; market_value is not being read")
	}

	in := []builtWebTx{
		buildWebTxFromPayload("sec1", dist, kind, "A", 100*day, payload),
	}
	// Run the same stage tail transactionsBeforeAPIStart runs.
	built := spliceNonExternalToJSON(in, nil)
	built = supersedeStatementCashWithDistributions(built)
	built = supersedeSalesWith1099B(built)
	out := dedupeCrossFeedExternalFlows(built)

	if len(out) != 1 || out[0].TransactionExternalID != "sec1" {
		t.Fatalf("securities transfer must survive the assembly tail; got %d rows: %v", len(out), survivingIDs(out))
	}
	var net float64
	for _, r := range out {
		if externalFlowKinds[r.Kind] && r.NetAmount != nil {
			net += r.NetAmount.InexactFloat64()
		}
	}
	if net != -25000 {
		t.Errorf("net_flow from securities transfer = %v, want -25000 (outflow)", net)
	}
}

// buildWebTxFromPayload constructs a builtWebTx the same way the production build
// loop in transactionsBeforeAPIStart does: it parses NetAmount out of the raw
// silver payload via extractWebTxAmounts and applies the canonical sign for the
// kind. Tests that assert on net_flow use this so they exercise the real
// extraction path rather than hand-supplying NetAmount.
func buildWebTxFromPayload(id, source string, kind canonical.TxKind, acct string, ts int64, payload string) builtWebTx {
	netAmount, quantity, price := extractWebTxAmounts(payload)
	return builtWebTx{source: source, tx: canonical.TransactionChange{
		TransactionExternalID: id,
		OccurredAt:            ts,
		AccountExternalID:     acct,
		Kind:                  kind,
		Currency:              "USD",
		NetAmount:             canonical.ApplyCanonicalSign(kind, netAmount),
		Quantity:              quantity,
		Price:                 price,
		Payload:               []byte(payload),
	}}
}

// TestWebKindTransferDirections pins the webKind mappings for the transfer
// words: the directional ones map to the directional canonical kinds, the two
// that name an OUTSIDE bank take their direction from the amount, the three
// that name a movement inside the household's own Schwab accounts stay
// journals whatever their sign, and the 1099-B "Sale" string maps to
// TxKindSell.
func TestWebKindTransferDirections(t *testing.T) {
	out := canonical.NewDecimalFromInt(-2500)
	in := canonical.NewDecimalFromInt(2500)
	zero := canonical.NewDecimalFromInt(0)
	cases := []struct {
		raw    string
		amount *canonical.Decimal
		descr  string
		want   canonical.TxKind
	}{
		{"Transfer Out", nil, "", canonical.TxKindTransferOut},
		{"Transfer In", nil, "", canonical.TxKindTransferIn},
		{"Sale", nil, "", canonical.TxKindSell}, // 1099-B sale rows

		// The two that reach an outside bank. Left as journals these
		// never entered the internal-transfer matcher, so the deposit
		// waiting at the bank could never be paired with them.
		{"Transfer", &out, "", canonical.TxKindWithdrawal},
		{"Transfer", &in, "", canonical.TxKindDeposit},
		{"MoneyLink Transfer", &out, "", canonical.TxKindWithdrawal},
		{"MoneyLink Transfer", &in, "", canonical.TxKindDeposit},

		// No amount, no direction: the sign is the whole of the
		// evidence and a guess would invent a flow.
		{"Transfer", nil, "", canonical.TxKindJournal},
		{"Transfer", &zero, "", canonical.TxKindJournal},
		{"MoneyLink Transfer", nil, "", canonical.TxKindJournal},

		// Movements inside the household's own Schwab accounts. These
		// must NOT move: they are signed too, and a sign-driven split
		// here would turn every own-account journal into a pair of
		// external flows.
		{"Security Transfer", &out, "", canonical.TxKindJournal},
		{"Journal", &out, "", canonical.TxKindJournal},
		{"Journal", &in, "", canonical.TxKindJournal},
		{"Journaled Shares", &out, "", canonical.TxKindJournal},

		// The statement parser's catch-all. A funds journal inside it
		// is cash and takes its direction from the sign, and the other
		// cash shapes are read by their names; everything else in the
		// bucket stays TxKindOther — including the SHARE journal whose
		// narrative begins with the same word.
		{"Unknown", &out, "Journaled Funds JOURNAL TO 00000000", canonical.TxKindWithdrawal},
		{"Unknown", &in, "Journaled Funds JOURNAL FRM 00000000", canonical.TxKindDeposit},
		{"Unknown", &out, "Journaled Shares EXAMPLE FUND: XMPL", canonical.TxKindOther},
		{"Unknown", &in, "Short Sale CALL EXAMPLE INC", canonical.TxKindSell},
		{"Unknown", &out, "Cover Short CALL EXAMPLE INC", canonical.TxKindBuy},
		{"Unknown", &out, "ADR Pass Thru Fee EXAMPLE HLDGS F", canonical.TxKindFee},
		{"Unknown", &in, "Frgn Tax Reclaim EXAMPLE AG F: XMPL", canonical.TxKindTax},
		{"Unknown", &in, "LT Cap Gain EXAMPLE ETF: XMPL", canonical.TxKindDividend},
		{"Unknown", &zero, "Expired CALL EXAMPLE INC", canonical.TxKindOther},
		{"Unknown", &in, "Account Transfer EXAMPLE FUND: XMPL", canonical.TxKindOther},
		{"Unknown", &out, "A note mentioning Journaled Funds midway", canonical.TxKindOther},
		{"Unknown", nil, "Journaled Funds JOURNAL TO 00000000", canonical.TxKindJournal},
	}
	for _, c := range cases {
		if got := webKind(c.raw, c.amount, &c.descr, ""); got != c.want {
			t.Errorf("webKind(%q, %v, %q) = %q, want %q", c.raw, c.amount, c.descr, got, c.want)
		}
	}
}

// TestWebKindCashDistributionIsAWithdrawal pins the two movements the
// "Transfer Out" word covers. A securities delivery is an in-kind leg and
// stays TxKindTransferOut, which the cash flow statement rightly excludes as
// one. A CASH distribution is money leaving the household: booked as the
// same kind it reaches no tier, no matcher and no pin, because that kind is
// in neither family's population — so it books as a withdrawal instead. Only
// the payload tells the two apart, and a payload that says nothing leaves
// the conservative answer in place.
func TestWebKindCashDistributionIsAWithdrawal(t *testing.T) {
	out := canonical.NewDecimalFromInt(-2500)
	descr := ""
	cases := []struct {
		name    string
		payload string
		want    canonical.TxKind
	}{
		{"a cash distribution", `{"transfer_kind":"cash"}`, canonical.TxKindWithdrawal},
		{"a securities delivery", `{"transfer_kind":"securities"}`, canonical.TxKindTransferOut},
		{"an equity-ledger leg, which states no transfer_kind", `{"equity_transfer_ledger":true}`, canonical.TxKindTransferOut},
		{"no payload", "", canonical.TxKindTransferOut},
		{"a malformed payload", "{not json", canonical.TxKindTransferOut},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := webKind("Transfer Out", &out, &descr, c.payload); got != c.want {
				t.Errorf("webKind(Transfer Out, payload=%s) = %q, want %q", c.payload, got, c.want)
			}
		})
	}

	// The inbound word is untouched. The distribution feed carries flows
	// OUT, so a row arriving under "Transfer In" is an in-kind receipt
	// whatever its payload says, and re-kinding it on the same fact would
	// turn every inbound ACAT leg into a phantom deposit.
	in := canonical.NewDecimalFromInt(2500)
	if got := webKind("Transfer In", &in, &descr, `{"transfer_kind":"cash"}`); got != canonical.TxKindTransferIn {
		t.Errorf("webKind(Transfer In, cash payload) = %q, want %q", got, canonical.TxKindTransferIn)
	}
}
