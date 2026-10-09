package carta

import (
	"fmt"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// k1Schema is the K-1 table of silver migrations 0004 and 0005.
const k1Schema = `
CREATE TABLE k1_capital_accounts (
    content_sha256         TEXT NOT NULL PRIMARY KEY,
    doc_id                 INTEGER,
    entity_external_id     INTEGER,
    tax_year               INTEGER,
    cash_distributions     TEXT,
    property_distributions TEXT,
    payload                TEXT NOT NULL,
    period_start           TEXT,
    period_end             TEXT
);`

// inKindFixture is the fund of fundBookFixture (calls on 2023-02-15 and
// 2024-06-30, NAV rows on 2024-09-30 and 2025-03-31, 150,000 contributed)
// with four K-1s, every figure invented:
//
//   - a fiscal-year 2023 K-1, period 2023-04-01 to 2024-03-31, states
//     20,000 of property distributed; a second document is a copy of it;
//   - a calendar-year 2024 K-1 states 5,000 of property distributed;
//   - a calendar-year 2022 K-1 states cash paid back only.
func inKindFixture(t *testing.T, periods bool) map[int64]canonical.PositionChange {
	t.Helper()
	k1 := k1Schema + `
INSERT INTO k1_capital_accounts(content_sha256, doc_id, entity_external_id, tax_year,
    cash_distributions, property_distributions, payload, period_start, period_end) VALUES
    ('sha-a', 11, 300, 2023, NULL,   '20000', '{}', '2023-04-01', '2024-03-31'),
    ('sha-b', 12, 300, 2023, NULL,   '20000', '{}', '2023-04-01', '2024-03-31'),
    ('sha-c', 13, 300, 2024, NULL,   '5000',  '{}', NULL, NULL),
    ('sha-d', 10, 300, 2022, '7000', NULL,    '{}', NULL, NULL);`
	if !periods {
		k1 += `
ALTER TABLE k1_capital_accounts DROP COLUMN period_start;
ALTER TABLE k1_capital_accounts DROP COLUMN period_end;`
	}
	later := fmt.Sprintf(`
INSERT INTO fund_metrics(snapshot_at, entity_external_id, currency, net_asset_value,
    capital_contributed, payload) VALUES (%d, 300, 'USD', '160000', '150000', '{}');`,
		unixDate(t, "2025-03-31"))
	byDay, _ := fundPositions(t, fundBookFixture(t, false, later, k1))
	return byDay
}

// A K-1's property distribution moves basis out of the fund from the
// K-1's period end on, both while the fund is carried at its calls and
// once it states a NAV. The period end is a snapshot day of its own. A
// copy of the K-1 counts once, and cash paid back reduces nothing.
func TestAPropertyDistributionReducesTheFundsBookValue(t *testing.T) {
	byDay := inKindFixture(t, true)
	reduced := canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
	}
	for _, c := range []struct {
		day, book string
		basis     canonical.Basis
	}{
		{"2023-02-15", "120000", fundCarryBasis},
		{"2024-03-31", "100000", reduced},
		{"2024-06-30", "130000", reduced},
		{"2024-09-30", "130000", reduced},
		{"2024-12-31", "125000", reduced},
		{"2025-03-31", "125000", reduced},
	} {
		p, ok := byDay[unixDate(t, c.day)]
		if !ok {
			t.Errorf("%s: no fund position", c.day)
			continue
		}
		if p.BookValue == nil || p.BookValue.String() != c.book || p.Basis != c.basis {
			t.Errorf("%s: book value %v stamped %+v, want %s stamped %+v",
				c.day, p.BookValue, p.Basis, c.book, c.basis)
		}
	}
	if got, want := string(byDay[unixDate(t, "2024-12-31")].Payload),
		`{"paid_in":"150000","property_distributed":"25000"}`; got != want {
		t.Errorf("NAV payload = %s, want %s", got, want)
	}
	if got, want := string(byDay[unixDate(t, "2024-03-31")].Payload),
		`{"called_capital":"120000","paid_in":"120000","property_distributed":"20000","valuation_basis":"called_capital"}`; got != want {
		t.Errorf("carried payload = %s, want %s", got, want)
	}
}

// A silver from before migration 0005 states no fiscal period: every
// K-1 then ends on Dec 31 of its tax year.
func TestAK1WithoutAPeriodEndsOnDec31(t *testing.T) {
	byDay := inKindFixture(t, false)
	for day, want := range map[string]string{"2023-02-15": "120000", "2023-12-31": "100000"} {
		if p := byDay[unixDate(t, day)]; p.BookValue == nil || p.BookValue.String() != want {
			t.Errorf("%s: book value %v, want %s", day, p.BookValue, want)
		}
	}
}

// capTableHolding loads one company holding the given securities rows
// and returns its position.
func capTableHolding(t *testing.T, rows string) canonical.PositionChange {
	t.Helper()
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (1700000000, 3, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload)
    VALUES (1700000000, 100, 'IND1', 0, 'Example Co', '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    quantity, cost, market_value, position_status, currency, issue_date, payload) VALUES ` + rows); err != nil {
		t.Fatal(err)
	}
	_, p := snapshotLots(t, path)
	return p
}

// A cap-table book value sums the cash paid for every held line. Only
// where every costed line is a share certificate is that the sum of its
// lots; a costed convertible or award beside the shares makes it the
// cash paid. The holding is acquired on its earliest share lot's date
// even where another line is dated earlier.
func TestTheCapTableStampFollowsWhatTheBookValueSums(t *testing.T) {
	const shares = `
    (1700000000, 100, 'share', 1, 1000, 500, 4000, 'held', '$', '02/01/2099',
     '{"original_acquisition_date": "01/15/2097"}')`
	for _, c := range []struct {
		name, rows, book string
		basis            canonical.Basis
	}{
		{"shares", shares, "500", shareBasis},
		{"shares and an uncosted option", shares + `,
    (1700000000, 100, 'option', 2, 5000, NULL, 0, 'held', '$', '01/01/2090', '{}')`, "500", shareBasis},
		{"shares and a convertible", shares + `,
    (1700000000, 100, 'convertible', 3, NULL, 2500, 2500, 'held', '$', '03/15/2090', '{}')`, "3000", cashPaidBasis},
		{"shares and an award", shares + `,
    (1700000000, 100, 'rsa', 4, 100, 10, 400, 'held', '$', '03/15/2090',
     '{"original_acquisition_date": "03/15/2090"}')`, "510", cashPaidBasis},
	} {
		p := capTableHolding(t, c.rows)
		if p.BookValue == nil || p.BookValue.String() != c.book || p.Basis != c.basis {
			t.Errorf("%s: book value %v stamped %+v, want %s stamped %+v", c.name, p.BookValue, p.Basis, c.book, c.basis)
		}
		if want := time.Date(2097, 1, 15, 0, 0, 0, 0, time.UTC); p.AcquisitionDate == nil || !p.AcquisitionDate.Equal(want) {
			t.Errorf("%s: acquisition date %v, want the share lot's %v", c.name, p.AcquisitionDate, want)
		}
	}
	conv := capTableHolding(t, `
    (1700000000, 100, 'convertible', 3, NULL, 2500, 2500, 'held', '$', '03/15/2090', '{}')`)
	if conv.BookValue == nil || conv.BookValue.String() != "2500" || conv.Basis != cashPaidBasis {
		t.Errorf("convertible alone: book value %v stamped %+v, want 2500 cash paid", conv.BookValue, conv.Basis)
	}
}
