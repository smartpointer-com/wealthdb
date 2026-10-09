package schwab

import (
	"context"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

//go:embed testdata/web_silver_schema.sql
var webSilverSchemaSQL string

// mergedFixture is a pair of seeded silvers, api and web, opened the way
// the config opens them: as subsources of one schwab source. Every
// account number, hash, CUSIP and figure in it is invented.
//
// The api roster holds two accounts. Web suffix "5678" bridges to
// HASH1 and "0042" to HASH2; web suffix "9999" has no api counterpart.
// The api's one position registers ticker VTI as CUSIP CUSIPVTI0, which
// is what the symbol bridge learns.
type mergedFixture struct {
	api, web         *sql.DB
	apiPath, webPath string
}

func newMergedFixture(t *testing.T) *mergedFixture {
	t.Helper()
	apiPath, api := newFixtureSilver(t)
	if _, err := api.Exec(`
        ALTER TABLE accounts  ADD COLUMN account_number       TEXT;
        ALTER TABLE positions ADD COLUMN average_cost         REAL;
        ALTER TABLE positions ADD COLUMN unrealized_gain_loss REAL;
        INSERT INTO accounts(snapshot_at, account_external_id, payload, account_number) VALUES
            (1000, 'HASH1', '{}', '11115678'),
            (1000, 'HASH2', '{}', '22220042');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'HASH1', 'CUSIPVTI0',
             '{"longQuantity":1,"shortQuantity":0,"marketValue":100,"instrument":{"assetType":"COLLECTIVE_INVESTMENT","cusip":"CUSIPVTI0","symbol":"VTI"}}');
    `); err != nil {
		t.Fatalf("seed api: %v", err)
	}

	webPath := filepath.Join(t.TempDir(), "schwab-web.db")
	web, err := sql.Open("sqlite", "file:"+webPath)
	if err != nil {
		t.Fatalf("open web seed: %v", err)
	}
	t.Cleanup(func() { web.Close() })
	if _, err := web.Exec(webSilverSchemaSQL); err != nil {
		t.Fatalf("apply web schema: %v", err)
	}
	if _, err := web.Exec(`
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, '5678', '{}'), (1000, '0042', '{}'), (1000, '9999', '{}');
    `); err != nil {
		t.Fatalf("seed web: %v", err)
	}
	return &mergedFixture{api: api, web: web, apiPath: apiPath, webPath: webPath}
}

// open opens the merged connection over the seeded files.
func (f *mergedFixture) open(t *testing.T) *Connection {
	t.Helper()
	conn, err := (&Adapter{}).Open(context.Background(), silver.OpenSpec{Subsources: []silver.Subsource{
		{Kind: "schwab-api", Path: f.apiPath},
		{Kind: "schwab-web", Path: f.webPath},
	}})
	if err != nil {
		t.Fatalf("Adapter.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn.(*Connection)
}

// everything is a window that covers every fixture timestamp.
var everything = canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}

// utcDay returns the Unix seconds of a calendar date at UTC midnight.
func utcDay(s string) int64 {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

func decStr(d *canonical.Decimal) string {
	if d == nil {
		return "<nil>"
	}
	return d.String()
}

func dateStr(t *time.Time) string {
	if t == nil {
		return "<nil>"
	}
	return t.Format(time.DateOnly)
}
