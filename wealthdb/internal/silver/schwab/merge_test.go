package schwab

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// newBridgeSeedDB creates a fresh on-disk SQLite under t.TempDir()
// holding only the accounts shape one side of the bridge queries.
func newBridgeSeedDB(t *testing.T, schema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("open seed DB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("apply seed schema: %v", err)
	}
	return db
}

// TestBuildAccountBridge pins the two-tier web → api account
// match: exact digits-only equality on the payload's
// `account_number_full` when present, else the trailing-suffix
// heuristic. All account numbers and hashes are synthetic.
func TestBuildAccountBridge(t *testing.T) {
	type apiAcct struct{ hash, number string }
	type webAcct struct {
		snap    int64
		suffix  string
		payload string
	}
	cases := []struct {
		name    string
		api     []apiAcct
		web     []webAcct
		want    map[string]string
		wantErr string // error-substring; empty means success
	}{
		{
			name: "unique suffix bridges without full number",
			api:  []apiAcct{{"HASH1", "12345678"}},
			web:  []webAcct{{1000, "5678", `{"suffix":"5678"}`}},
			want: map[string]string{"5678": "HASH1"},
		},
		{
			name:    "ambiguous suffix without full number fails loudly",
			api:     []apiAcct{{"HASH1", "11115678"}, {"HASH2", "22225678"}},
			web:     []webAcct{{1000, "5678", `{"suffix":"5678"}`}},
			wantErr: `web suffix "5678" matches 2 api accounts`,
		},
		{
			name: "exact full number resolves an ambiguous suffix",
			api:  []apiAcct{{"HASH1", "11115678"}, {"HASH2", "22225678"}},
			web:  []webAcct{{1000, "5678", `{"account_number_full":"1111-5678"}`}},
			want: map[string]string{"5678": "HASH1"},
		},
		{
			name: "full number normalizes separators on both sides",
			api:  []apiAcct{{"HASH1", "1234-5678"}},
			web:  []webAcct{{1000, "5678", `{"account_number_full":" 1234 - 5678 "}`}},
			want: map[string]string{"5678": "HASH1"},
		},
		{
			name: "unknown full number falls back to the suffix heuristic",
			api:  []apiAcct{{"HASH1", "12345678"}},
			web:  []webAcct{{1000, "5678", `{"account_number_full":"9999-5678"}`}},
			want: map[string]string{"5678": "HASH1"},
		},
		{
			name:    "unknown full number keeps the loud suffix ambiguity",
			api:     []apiAcct{{"HASH1", "11115678"}, {"HASH2", "22225678"}},
			web:     []webAcct{{1000, "5678", `{"account_number_full":"3333-5678"}`}},
			wantErr: `web suffix "5678" matches 2 api accounts`,
		},
		{
			name:    "full number disagreeing with the suffix fails loudly",
			api:     []apiAcct{{"HASH1", "11115678"}},
			web:     []webAcct{{1000, "999", `{"account_number_full":"1111-5678"}`}},
			wantErr: `full account number for web suffix "999" does not end in that suffix`,
		},
		{
			name:    "identical digits-only api numbers guard",
			api:     []apiAcct{{"HASH1", "1234-5678"}, {"HASH2", "12345678"}},
			web:     []webAcct{{1000, "5678", `{"account_number_full":"1234-5678"}`}},
			wantErr: `web account number for suffix "5678" matches 2 api accounts`,
		},
		{
			name: "web-only suffix is skipped silently",
			api:  []apiAcct{{"HASH1", "12345678"}},
			web:  []webAcct{{1000, "999", `{"suffix":"999"}`}},
			want: map[string]string{},
		},
		{
			name: "full number from any snapshot participates",
			api:  []apiAcct{{"HASH1", "11115678"}, {"HASH2", "22225678"}},
			web: []webAcct{
				{1000, "5678", `{"suffix":"5678"}`},
				{2000, "5678", `{"account_number_full":"2222-5678"}`},
			},
			want: map[string]string{"5678": "HASH2"},
		},
		{
			name: "full number on the earliest snapshot survives later payloads without it",
			api:  []apiAcct{{"HASH1", "11115678"}, {"HASH2", "22225678"}},
			web: []webAcct{
				{1000, "5678", `{"account_number_full":"2222-5678"}`},
				{2000, "5678", `{"suffix":"5678"}`},
			},
			want: map[string]string{"5678": "HASH2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newBridgeSeedDB(t, `
                CREATE TABLE accounts (
                    snapshot_at         INTEGER NOT NULL,
                    account_external_id TEXT    NOT NULL,
                    payload             TEXT    NOT NULL,
                    account_number      TEXT,
                    PRIMARY KEY (snapshot_at, account_external_id)
                )`)
			for _, a := range c.api {
				if _, err := api.Exec(
					`INSERT INTO accounts(snapshot_at, account_external_id, payload, account_number) VALUES (1000, ?, '{}', ?)`,
					a.hash, a.number); err != nil {
					t.Fatalf("seed api: %v", err)
				}
			}
			web := newBridgeSeedDB(t, `
                CREATE TABLE accounts (
                    snapshot_at         INTEGER NOT NULL,
                    account_external_id TEXT    NOT NULL,
                    nickname            TEXT,
                    payload             TEXT    NOT NULL,
                    PRIMARY KEY (snapshot_at, account_external_id)
                )`)
			for _, wa := range c.web {
				if _, err := web.Exec(
					`INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (?, ?, ?)`,
					wa.snap, wa.suffix, wa.payload); err != nil {
					t.Fatalf("seed web: %v", err)
				}
			}

			got, err := buildAccountBridge(context.Background(), api, web)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildAccountBridge: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("bridge = %v, want %v", got, c.want)
			}
			for suffix, hash := range c.want {
				if got[suffix] != hash {
					t.Errorf("bridge[%q] = %q, want %q", suffix, got[suffix], hash)
				}
			}
		})
	}
}

// TestAPICoverageStart: the api history starts at its first row unless
// that row is a stray — followed by more than apiStrayGap of api silence
// while the web books the account as active.
func TestAPICoverageStart(t *testing.T) {
	const day = int64(86400)
	cases := []struct {
		name     string
		api, web []int64
		want     int64
	}{
		{"a continuous history starts at its first row",
			[]int64{10 * day, 11 * day, 12 * day}, []int64{5 * day, 9 * day}, 10 * day},
		{"a stray before a silence the web fills is set aside",
			[]int64{10 * day, 200 * day, 201 * day}, []int64{10 * day, 50 * day, 120 * day}, 200 * day},
		{"a quiet account keeps its first row",
			[]int64{10 * day, 200 * day}, []int64{5 * day, 205 * day}, 10 * day},
		{"a web row on the stray's own day is not evidence",
			[]int64{10*day + 3600, 200 * day}, []int64{10 * day}, 10*day + 3600},
		{"a silence of exactly the gap is not one",
			[]int64{10 * day, 10*day + apiStrayGap}, []int64{20 * day}, 10 * day},
		{"two strays in a row are both set aside",
			[]int64{10 * day, 100 * day, 300 * day, 301 * day}, []int64{50 * day, 200 * day}, 300 * day},
		{"a web row on the next api row's day is its twin, not evidence",
			[]int64{10 * day, 200*day + 50000}, []int64{200 * day}, 10 * day},
		{"a lone api row is its own start",
			[]int64{10 * day}, []int64{50 * day}, 10 * day},
	}
	for _, c := range cases {
		if got := apiCoverageStart(c.api, c.web); got != c.want {
			t.Errorf("%s: start = %d, want %d", c.name, got/day, c.want/day)
		}
	}
}

// TestAPIFromCoverageStartDropsTheStrays: the api rows before an
// account's coverage start leave, every other row stays.
func TestAPIFromCoverageStartDropsTheStrays(t *testing.T) {
	mk := func(id, acct string, at int64) canonical.TransactionChange {
		return canonical.TransactionChange{TransactionExternalID: id, AccountExternalID: acct, OccurredAt: at}
	}
	inner := silver.NewTransactionStream(canonical.TransactionBatch{Transactions: []canonical.TransactionChange{
		mk("stray", "A", 10), mk("kept", "A", 200), mk("other", "B", 5), mk("unmapped", "C", 1),
	}})
	s := &apiFromCoverageStart{inner: inner, start: map[string]int64{"A": 200, "B": 5}}
	batch, _, err := s.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tx := range batch.Transactions {
		got = append(got, tx.TransactionExternalID)
	}
	if want := []string{"kept", "other", "unmapped"}; !slices.Equal(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
}
