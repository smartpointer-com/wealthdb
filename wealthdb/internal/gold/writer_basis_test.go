package gold

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Every id and figure below is invented.

var lotsBasis = canonical.Basis{
	Origin: canonical.BasisStated, Method: canonical.BasisMethodLots, Fees: canonical.BasisFeesIncluded,
}

func basisPosition(key string) canonical.PositionChange {
	return canonical.PositionChange{
		SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC1",
		PositionKey: key, Currency: "USD",
		AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock,
	}
}

func TestInsertPositionsStoresTheBasisStamp(t *testing.T) {
	db, ctx := openMigrated(t)
	p := basisPosition("XYZ")
	bv := canonical.NewDecimalFromInt(1234)
	p.SetBookValue(&bv, lotsBasis)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{p, basisPosition("NOBASIS")})
	})

	var origin, method, fees string
	if err := db.QueryRowContext(ctx, `SELECT basis_origin, basis_method, basis_fees
	      FROM positions WHERE position_key = 'XYZ'`).Scan(&origin, &method, &fees); err != nil {
		t.Fatal(err)
	}
	if origin != "stated" || method != "lots" || fees != "included" {
		t.Errorf("stamp = %s/%s/%s", origin, method, fees)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM positions
	      WHERE position_key = 'NOBASIS' AND basis_origin IS NULL AND basis_method IS NULL
	        AND basis_fees IS NULL AND book_value IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Errorf("unstamped row: n=%d err=%v", n, err)
	}
}

func TestInsertPositionsRefusesAnUnstampedBookValue(t *testing.T) {
	db, ctx := openMigrated(t)
	p := basisPosition("XYZ")
	bv := canonical.NewDecimalFromInt(1)
	p.BookValue = &bv
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = NewWriter(tx).InsertPositions(ctx, []canonical.PositionChange{p})
	if err == nil || !strings.Contains(err.Error(), "stamp") {
		t.Fatalf("err = %v, want a stamp refusal", err)
	}
}

func TestInsertPositionLotsRoundTrips(t *testing.T) {
	db, ctx := openMigrated(t)
	q := canonical.NewDecimalFromInt(10)
	bv := canonical.NewDecimalFromInt(900)
	acquired := time.Date(2021, 3, 4, 0, 0, 0, 0, time.UTC)
	yes := true
	doc := "sha-doc"
	lots := []canonical.PositionLotChange{
		{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC1",
			PositionKey: "XYZ", LotKey: "1", Currency: "USD", Quantity: &q, BookValue: &bv,
			AcquisitionDate: &acquired, Term: canonical.LotTermLong, Covered: &yes,
			BasisOrigin: canonical.BasisStated, SourceDocument: &doc},
		{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC1",
			PositionKey: "XYZ", LotKey: "2", Currency: "USD", Quantity: &q},
	}
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertPositionLots(ctx, lots) })

	var term, origin string
	var day time.Time
	var covered bool
	if err := db.QueryRowContext(ctx, `SELECT term, basis_origin, acquisition_date, covered
	      FROM position_lots WHERE lot_key = '1'`).Scan(&term, &origin, &day, &covered); err != nil {
		t.Fatal(err)
	}
	if term != "long" || origin != "stated" || !day.Equal(acquired) || !covered {
		t.Errorf("lot 1 = %s %s %v %v", term, origin, day, covered)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM position_lots
	      WHERE lot_key = '2' AND book_value IS NULL AND basis_origin IS NULL AND covered IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Errorf("lot 2: n=%d err=%v", n, err)
	}
}

func TestInsertPositionLotsRefusesAMismatchedOrigin(t *testing.T) {
	db, ctx := openMigrated(t)
	bv := canonical.NewDecimalFromInt(1)
	for _, l := range []canonical.PositionLotChange{
		{PositionKey: "X", LotKey: "1", Currency: "USD", BookValue: &bv},
		{PositionKey: "X", LotKey: "2", Currency: "USD", BasisOrigin: canonical.BasisStated},
		{PositionKey: "X", LotKey: "3", Currency: "USD", Term: "SHORT"},
	} {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := NewWriter(tx).InsertPositionLots(ctx, []canonical.PositionLotChange{l}); err == nil {
			t.Errorf("lot %s accepted", l.LotKey)
		}
		_ = tx.Rollback()
	}
}

func TestInsertRealizedLotsRoundTrips(t *testing.T) {
	db, ctx := openMigrated(t)
	q := canonical.NewDecimalFromInt(5)
	proceeds := canonical.NewDecimalFromInt(700)
	cost := canonical.NewDecimalFromInt(500)
	sold := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	box := "A"
	lots := []canonical.RealizedLotChange{{
		SilverSourceID: "test-src", RealizedLotExternalID: "L1", AccountExternalID: "ACC1",
		InstrumentHint: "SOME CORP", DocumentKind: canonical.RealizedForm1099B, TaxYear: 2024,
		AcquiredVarious: true, DisposalDate: &sold, Currency: "USD",
		Quantity: &q, Proceeds: &proceeds, BookValue: &cost, Term: canonical.LotTermShort,
		Form8949Box: &box, Basis: lotsBasis, IsPrimary: true,
	}}
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertRealizedLots(ctx, lots) })

	var hint, kind, method string
	var various, primary bool
	var gain *float64
	if err := db.QueryRowContext(ctx, `SELECT instrument_hint, document_kind, basis_method,
	      acquired_various, is_primary, realized_gain_loss FROM realized_lots`).Scan(
		&hint, &kind, &method, &various, &primary, &gain); err != nil {
		t.Fatal(err)
	}
	if hint != "SOME CORP" || kind != "form_1099b" || method != "lots" || !various || !primary || gain != nil {
		t.Errorf("row = %s %s %s %v %v %v", hint, kind, method, various, primary, gain)
	}
}

func TestInsertRealizedLotsRefusesBadRows(t *testing.T) {
	db, ctx := openMigrated(t)
	cost := canonical.NewDecimalFromInt(1)
	for _, r := range []canonical.RealizedLotChange{
		{RealizedLotExternalID: "kind", DocumentKind: "1099b", Currency: "USD"},
		{RealizedLotExternalID: "stamp", DocumentKind: canonical.RealizedTrade, Currency: "USD", BookValue: &cost},
	} {
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := NewWriter(tx).InsertRealizedLots(ctx, []canonical.RealizedLotChange{r}); err == nil {
			t.Errorf("%s accepted", r.RealizedLotExternalID)
		}
		_ = tx.Rollback()
	}
}

// TestMigration0115DDLIsRerunnable replays the cost-basis migration over a
// migrated database holding a stamped row and a lot of each kind, and
// pins that nothing is lost.
func TestMigration0115DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	p := basisPosition("XYZ")
	bv := canonical.NewDecimalFromInt(1)
	p.SetBookValue(&bv, lotsBasis)
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertPositions(ctx, []canonical.PositionChange{p}); err != nil {
			return err
		}
		if err := w.InsertPositionLots(ctx, []canonical.PositionLotChange{{
			SilverSourceID: "test-src", PositionKey: "XYZ", LotKey: "1", Currency: "USD"}}); err != nil {
			return err
		}
		return w.InsertRealizedLots(ctx, []canonical.RealizedLotChange{{
			SilverSourceID: "test-src", RealizedLotExternalID: "L1", AccountExternalID: "ACC1",
			DocumentKind: canonical.RealizedTrade, TaxYear: 2024, Currency: "USD"}})
	})
	rerunMigrationDDL(t, db, ctx, "0115_cost_basis.sql")
	var n int
	if err := db.QueryRowContext(ctx, `SELECT
	      (SELECT COUNT(*) FROM positions WHERE basis_origin = 'stated')
	    + (SELECT COUNT(*) FROM position_lots)
	    + (SELECT COUNT(*) FROM realized_lots)`).Scan(&n); err != nil || n != 3 {
		t.Errorf("rows after replay = %d (err %v), want 3", n, err)
	}
}
