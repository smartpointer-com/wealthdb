package gold

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// These tests lock a crypto tracker (silver_kind "cointracking") onto the
// default policy: fiat deposits and withdrawals and coin transfer legs are
// all capital. A move between two wallets nets where an aggregate holds
// both, and a wallet that debuts funded by one onboards nothing of it. A leg
// nothing pairs is a flow, valued at the day's rate. All data synthetic /
// placeholder (AGENTS.md §4).

// TestCointrackingSweepFundsColdDebut is the phantom-inflow guard: two
// exchange wallets funded by fiat deposits sweep their coins into two
// cold-storage wallets that debut on the sweep. The netter pairs each sweep
// by value, and its in-leg funds its wallet's debut, so the aggregate flow
// set holds only the fiat deposits, and no onboarding of the debuting
// wallets. A phantom inflow of the debut value would inflate net_flow and
// drag the aggregate TWR negative.
//
// A stored link changes nothing here. It pairs the legs the netter pairs
// without it, and the in-leg on its wallet's debut funds the debut alike.
func TestCointrackingSweepFundsColdDebut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// in is the day the coin lands in cold storage, and inA and inB its
		// value there: the day it left at the same price, or a day later at
		// a price moved inside the netter's tolerance.
		in       int64
		inA, inB int64
		linked   bool
	}{
		{"same day, netted by value", dy(2023, time.June, 30), 12000, 8000, false},
		{"next day at a moved price, links stored", dy(2023, time.July, 1), 12030, 8020, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "ctx", "cointracking")

			wStart := dy(2023, time.January, 2)
			fund := dy(2023, time.March, 31) // in-window fiat deposits
			sweep := dy(2023, time.June, 30)
			end := dy(2023, time.December, 29)

			// EXCH_A: base 6000 -> +6000 deposit = 12000, swept to ~0.
			seedAcct(t, db, ctx, "ctx", "EXCH_A", canonical.AccountKindCrypto, nil,
				[]snap{{wStart, 6000}, {fund, 12000}, {sweep, 1}, {end, 1}},
				[]txn{{fund, canonical.TxKindDeposit, 6000}, {sweep, canonical.TxKindTransferOut, -12000}})
			// EXCH_B: base 4000 -> +4000 deposit = 8000, swept to ~0.
			seedAcct(t, db, ctx, "ctx", "EXCH_B", canonical.AccountKindCrypto, nil,
				[]snap{{wStart, 4000}, {fund, 8000}, {sweep, 1}, {end, 1}},
				[]txn{{fund, canonical.TxKindDeposit, 4000}, {sweep, canonical.TxKindTransferOut, -8000}})
			// The cold wallets debut with the swept coin and gain ~10% by the end.
			seedAcct(t, db, ctx, "ctx", "COLD_A", canonical.AccountKindCrypto, nil,
				[]snap{{tc.in, tc.inA}, {end, 13200}},
				[]txn{{tc.in, canonical.TxKindTransferIn, tc.inA}})
			seedAcct(t, db, ctx, "ctx", "COLD_B", canonical.AccountKindCrypto, nil,
				[]snap{{tc.in, tc.inB}, {end, 8800}},
				[]txn{{tc.in, canonical.TxKindTransferIn, tc.inB}})
			p := params("sources", 0, eod(2023, time.December, 29))
			if tc.linked {
				seedTransferLink(t, db, ctx, "ctx", "EXCH_A-tx1", "ctx", "COLD_A-tx0")
				seedTransferLink(t, db, ctx, "ctx", "EXCH_B-tx1", "ctx", "COLD_B-tx0")
				p.TransferMatching = true
			}

			rows, err := RunReturns(ctx, db, p)
			if err != nil {
				t.Fatalf("RunReturns: %v", err)
			}
			// Only the two fiat deposits (6000 + 4000) cross the boundary.
			if nf := netFlowOf(t, rows, "ctx"); math.Abs(nf-10000) > 1e-6 {
				t.Errorf("net_flow = %.2f, want 10000 (only the fiat deposits; no phantom "+
					"onboarding of the debuting cold wallets)", nf)
			}
			s, ok := summaryFor(rows, "ctx")
			if !ok {
				t.Fatal("no ctx source row")
			}
			if s.TWR == nil || s.MWR == nil {
				t.Fatalf("ctx TWR/MWR should both be defined (TWR=%v MWR=%v)", s.TWR, s.MWR)
			}
			// Real growth on the 10000 put in is a positive return, and MWR
			// exceeds TWR because the capital was in for less than the window.
			if *s.TWR <= 0 {
				t.Errorf("aggregate TWR = %.4f, want positive (phantom onboarding would drag it negative)", *s.TWR)
			}
			if !(*s.TWR < *s.MWR) {
				t.Errorf("want TWR (%.4f) strictly below MWR (%.4f)", *s.TWR, *s.MWR)
			}
			for _, q := range s.Quality {
				if strings.HasPrefix(q, "unmatched_transfers") {
					t.Errorf("every sweep leg pairs: quality %v", s.Quality)
				}
			}

			// Each wallet has a row of its own: the cold wallet's return is its
			// coin's gain on its debut value.
			p.Level = "accounts"
			acct, err := RunReturns(ctx, db, p)
			if err != nil {
				t.Fatalf("RunReturns accounts: %v", err)
			}
			c, ok := summaryFor(acct, "COLD_A")
			if !ok || c.TWR == nil || qualityHas(c, "accounts_grain_meaningless") {
				t.Fatalf("COLD_A needs a computed accounts row: %+v", c)
			}
			if want := 13200/float64(tc.inA) - 1; math.Abs(*c.TWR-want) > 1e-9 {
				t.Errorf("COLD_A TWR = %.6f, want %.6f", *c.TWR, want)
			}
		})
	}
}

// TestCointrackingUnpairedLegsAreFlows: a coin that arrives from a wallet
// nothing tracks is an inflow at the day's rate, and an exchange's journal
// leg counts under the same kind table as any source's. Neither has a
// partner, so the aggregate says so.
func TestCointrackingUnpairedLegsAreFlows(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "ctu", "cointracking")

	wStart := dy(2024, time.January, 2)
	arrive := dy(2024, time.June, 3)
	move := dy(2024, time.September, 2)
	end := dy(2024, time.December, 30)
	seedAcct(t, db, ctx, "ctu", "WALLET", canonical.AccountKindCrypto, nil,
		[]snap{{wStart, 10000}, {arrive, 13000}, {move, 12500}, {end, 14000}}, nil)
	btcIn, _ := canonical.NewDecimalFromString("0.1")
	journalOut, early, onTheDay := canonical.NewDecimalFromInt(-500), canonical.NewDecimalFromInt(25000),
		canonical.NewDecimalFromInt(30000)
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertFxRates(ctx, []canonical.FxRateChange{
			{SilverSourceID: "ctu", SnapshotAt: wStart, BaseCurrency: "USD", QuoteCurrency: "BTC", MidRate: early},
			{SilverSourceID: "ctu", SnapshotAt: arrive, BaseCurrency: "USD", QuoteCurrency: "BTC", MidRate: onTheDay},
		}); err != nil {
			return err
		}
		return w.InsertTransactions(ctx, []canonical.TransactionChange{
			{SilverSourceID: "ctu", TransactionExternalID: "coin-in", OccurredAt: arrive,
				AccountExternalID: "WALLET", Kind: canonical.TxKindTransferIn, Currency: "BTC", NetAmount: &btcIn},
			{SilverSourceID: "ctu", TransactionExternalID: "journal-out", OccurredAt: move,
				AccountExternalID: "WALLET", Kind: canonical.TxKindJournal, Currency: "USD", NetAmount: &journalOut},
		})
	})

	p := params("sources", 0, eod(2024, time.December, 30))
	p.TransferMatching = true
	rows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	// 0.1 coin at the arrival day's 30000, less the 500 journal.
	if nf := netFlowOf(t, rows, "ctu"); math.Abs(nf-2500) > 1e-6 {
		t.Errorf("net_flow = %.2f, want 2500 (the coin at the day's rate, less the journal)", nf)
	}
	s, _ := summaryFor(rows, "ctu")
	for _, flag := range []string{"unmatched_transfers=2", "journal_present"} {
		if !qualityHas(s, flag) {
			t.Errorf("quality %v, want %s", s.Quality, flag)
		}
	}
	if qualityHas(s, "fx_clamped_flow") {
		t.Errorf("the coin's rate is the day's, not a clamp: %v", s.Quality)
	}
}
