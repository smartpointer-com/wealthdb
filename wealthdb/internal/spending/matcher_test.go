package spending

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// legRail classifies the card-bill rail. What matters is the ASYMMETRY: the
// receipt side is unmistakable and demands a card payment opposite it, while
// the paying side demands nothing, because the bank's record of paying a card
// often carries only the issuer's name, the holder's own, or nothing at all.
// Every value here is synthetic.
func TestLegRail(t *testing.T) {
	cases := []struct {
		name         string
		counterparty string
		description  string
		rail         string
		partner      string
	}{
		{"a card's record of being paid demands a card payment",
			"", "PAYMENT THANK YOU MOBILE", railCardReceipt, railCardPayment},
		{"the issuers' other receipt wording, same demand",
			"", "AUTOPAY PAYMENT RECEIVED THANK YOU", railCardReceipt, railCardPayment},
		{"a bank-side card payment announces its rail and demands none",
			"", "01 31 PAYMENT TO EXAMPLE CARD ENDING IN", railCardPayment, ""},
		{"an issuer's own wording for the same",
			"", "EXAMPLE CREDIT CRD EPAY ONUS WEB ID", railCardPayment, ""},
		// The shapes that must stay unclassified: an unclassified leg
		// constrains nothing and pairs exactly as it did before.
		{"a utility bill names no rail", "Example Power", "EXAMPLE POWER BILLPAY PPD ID", "", ""},
		{"a payment to a person names no rail",
			"", "QUICKPAY WITH ZELLE PAYMENT TO EXAMPLE PERSON", "", ""},
		{"a cheque names no rail", "", "IF YOU SEE A DESCRIPTION IN THE CHECKS PAID SECTION", "", ""},
		{"a blank narrative names no rail", "", "", "", ""},
		// The counterparty column is read too: some feeds carry the wording
		// there and leave the description empty.
		{"the rail may be written in either column",
			"PAYMENT THANK YOU", "", railCardReceipt, railCardPayment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rail, partner := legRail(tc.counterparty, tc.description)
			if rail != tc.rail || partner != tc.partner {
				t.Errorf("legRail(%q, %q) = (%q, %q), want (%q, %q)",
					tc.counterparty, tc.description, rail, partner, tc.rail, tc.partner)
			}
		})
	}
}

// A narrative names the accounts a configured pattern points at, except the
// leg's own source or its own account, which is its institution speaking and
// names no other side. Another account of its own source stays named.
func TestLegNames(t *testing.T) {
	names := []CounterpartyName{
		{Source: "bank", Pattern: regexp.MustCompile(`(?i)example bank`)},
		{Source: "exchange", Account: "wallet-a", Pattern: regexp.MustCompile(`(?i)example exchange`)},
		{Source: "broker", Account: "cash-2", Pattern: regexp.MustCompile(`(?i)to 000002`)},
	}
	cases := []struct {
		group, owner, text string
		want               []string
	}{
		{"broker", "cash-1", "TRANSFER FROM EXAMPLE BANK", []string{"bank"}},
		{"bank", "checking", "EXAMPLE BANK ONLINE TRANSFER", nil},
		{"bank", "checking", "PAYMENT EXAMPLE EXCHANGE", []string{gold.NamedAccount("exchange", "wallet-a")}},
		{"exchange", "wallet-a", "EXAMPLE EXCHANGE DEPOSIT", nil},
		{"broker", "cash-1", "JOURNAL TO 000002", []string{gold.NamedAccount("broker", "cash-2")}},
		{"broker", "cash-2", "JOURNAL TO 000002", nil},
	}
	for _, c := range cases {
		got := legNames(names, c.group, c.owner, "", c.text)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s/%s %q: names %v, want %v", c.group, c.owner, c.text, got, c.want)
		}
	}
}

// reversalRe reads the words a bank uses for undoing an entry, in the
// languages the collected banks write in, and not the words that merely
// look alike. The wording is invented.
func TestReversalWording(t *testing.T) {
	for _, s := range []string{
		"Example Fee Refund", "CANCELLED EXAMPLE PAYMENT", "CANC. EXAMPLE BOOKING; 000000",
		"STORNO VOM 01.01", "RETURNED ITEM", "Rückbuchung Lastschrift", "REVERSAL OF CHARGE",
	} {
		if !reversalRe.MatchString(s) {
			t.Errorf("%q is a reversal", s)
		}
	}
	for _, s := range []string{
		"RETURN OF CAPITAL EXAMPLE FUND", "CANCER RESEARCH DONATION", "EXAMPLE TRANSFER",
	} {
		if reversalRe.MatchString(s) {
			t.Errorf("%q is not a reversal", s)
		}
	}
}

// A coin's dust, whose amount in the coin itself net_amount's four places
// round to zero, pairs on its quantity: it keeps its size and its sign. A
// ledger `match` line names it by that amount and pairs it where the window
// cannot reach, and a line that names both legs but pairs none is counted.
func TestACoinsDustPairsOnItsQuantity(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                  instrument_external_id, kind, currency, quantity, net_amount) VALUES
            ('bank', 'dust:out', ?, 'CUST1', 'ETH', 'withdrawal', 'ETH', -0.00000913, 0),
            ('other-bank', 'dust:in', ?, 'CUST2', 'ETH', 'deposit', 'ETH', 0.00000913, 0),
            ('bank', 'late:out', ?, 'CUST1', 'ETH', 'withdrawal', 'ETH', -0.00000377, 0),
            ('other-bank', 'late:in', ?, 'CUST2', 'ETH', 'deposit', 'ETH', 0.00000377, 0),
            ('bank', 'share:in', ?, 'BRK1', 'VTI', 'deposit', 'USD', 3, 0),
            ('bank', 'cash:in', ?, 'CASH1', NULL, 'deposit', 'USD', NULL, 0.004)`,
		day(10), day(10), day(40), day(60), day(80), day(80)); err != nil {
		t.Fatal(err)
	}
	sel := func(source, account string, at int64, amount float64, ccy string) gold.TransferOverrideSelector {
		return gold.TransferOverrideSelector{Source: source, Account: account, Day: at, Amount: amount, Currency: ccy}
	}
	late := sel("other-bank", "CUST2", day(60), 0.00000377, "ETH")
	idle := sel("bank", "CASH1", day(80), 0.004, "USD")
	res := runPass(t, db, ctx, Options{TransferOverrides: []gold.TransferOverrideRule{
		{Verb: gold.VerbMatch, A: sel("bank", "CUST1", day(40), -0.00000377, "ETH"), B: &late},
		// A share's leg is no coin's: its instrument is not its currency,
		// so its zero stays a zero and pairs in no phase.
		{Verb: gold.VerbMatch, A: sel("bank", "BRK1", day(80), 0, "USD"), B: &idle},
	}})
	got := scanPairs(t, db, `
        SELECT silver_source_id || '/' || transaction_external_id,
               far_transaction_external_id || '|' || evidence
          FROM transfer_links WHERE far_transaction_external_id IS NOT NULL`)
	want := map[string]string{
		"bank/dust:out":      "dust:in|amount",
		"other-bank/dust:in": "dust:out|amount",
		"bank/late:out":      "late:in|ledger",
		"other-bank/late:in": "late:out|ledger",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("links = %v, want %v", got, want)
	}
	if res.UnmatchedTransferOverrides != 0 || res.IdleTransferMatches != 1 {
		t.Errorf("unmatched %d, idle %d; want 0 and 1", res.UnmatchedTransferOverrides, res.IdleTransferMatches)
	}
}

// A coin's legs pair by the coin rule at the coin's USD price on the day
// (gold's fx_daily): a small transfer short by a fee worth a few dollars
// pairs, though the fee is a large share of it and beyond the 1% a coin
// with no price may lose, and a smaller withdrawal beside a larger one
// never takes the larger one's receipt. A leg is a coin's where its
// instrument is its currency and a coin held physical. A price quoted more
// than a week before the leg is no price that day. The same legs pair as
// cash does, within a cent, in a currency no instrument calls a coin, and
// in one whose instrument is cash.
func TestCoinLegsPairByTheCoinRule(t *testing.T) {
	asCoin := map[string]string{"fee:out": "fee:in", "b:large": "c:in"}
	asCash := map[string]string{"fee:out": "fee:in", "a:small": "c:in"}
	for _, c := range []struct {
		name, class, vehicle string
		priced               []int64
		want                 map[string]string
	}{
		{"a coin", "crypto", "physical", []int64{10, 20}, asCoin},
		{"a coin with a stale price", "crypto", "physical", []int64{1}, map[string]string{"b:large": "c:in"}},
		{"no coin", "", "", nil, asCash},
		{"a cash instrument", "cash", "demand_deposit", []int64{10, 20}, asCash},
	} {
		db, ctx := openGold(t)
		if c.class != "" {
			if _, err := db.ExecContext(ctx, `
                INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, first_seen_at, last_seen_at)
                    VALUES ('bank', 'BTC', ?, ?, 1, 1), ('other-bank', 'BTC', ?, ?, 1, 1)`,
				c.class, c.vehicle, c.class, c.vehicle); err != nil {
				t.Fatal(err)
			}
		}
		for _, d := range c.priced {
			if _, err := db.ExecContext(ctx, `
                INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
                    VALUES ('bank', ?, 'USD', 'BTC', 30000)`, day(d)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
                                      instrument_external_id, kind, currency, quantity, net_amount) VALUES
                ('bank', 'fee:out', ?, 'CUST1', 'BTC', 'withdrawal', 'BTC', -0.002, -0.002),
                ('other-bank', 'fee:in', ?, 'CUST2', 'BTC', 'deposit', 'BTC', 0.0017, 0.0017),
                ('bank', 'a:small', ?, 'BRK1', 'BTC', 'withdrawal', 'BTC', -0.01, -0.01),
                ('bank', 'b:large', ?, 'CUST1', 'BTC', 'withdrawal', 'BTC', -0.02, -0.02),
                ('other-bank', 'c:in', ?, 'CUST2', 'BTC', 'deposit', 'BTC', 0.0199, 0.0199)`,
			day(10), day(11), day(20), day(20), day(21)); err != nil {
			t.Fatal(err)
		}
		runPass(t, db, ctx, Options{})
		got := scanPairs(t, db, `
            SELECT transaction_external_id, far_transaction_external_id
              FROM transfer_links
             WHERE far_transaction_external_id IS NOT NULL AND silver_source_id = 'bank'`)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: links = %v, want %v", c.name, got, c.want)
		}
	}
}
