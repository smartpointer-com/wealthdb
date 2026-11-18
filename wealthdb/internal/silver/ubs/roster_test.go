package ubs

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// The roster as it stands at its newest snapshot. The loader writes an
// account's master-data row only when the row changed, so an account the
// newest batch left unchanged stands at an older row, and the current
// roster reads it there. Every id is invented.

func TestTheCurrentRosterKeepsAnAccountTheNewestBatchLeftUnchanged(t *testing.T) {
	ctx := context.Background()
	_, db := newFixtureSilver(t)
	silvertest.Exec(t, db, `
INSERT INTO cash_accounts (snapshot_at, relationship_id, account_external_id, portfolio_external_id, payload) VALUES
    (1000, 'R1', 'CH00ROSTERCHF', 'P1', '{"AcctCcyIsoCd":"CHF"}'),
    (1000, 'R1', 'CH00ROSTERUSD', 'P1', '{"AcctCcyIsoCd":"USD"}'),
    (2000, 'R1', 'CH00ROSTERUSD', 'P1', '{"AcctCcyIsoCd":"USD","AcctTpDesc":"Changed"}');
INSERT INTO safekeeping_accounts (snapshot_at, relationship_id, account_external_id, portfolio_external_id, payload) VALUES
    (1000, 'R1', 'SAFE1', 'P1', '{"PrtflId":"P1"}'),
    (2000, 'R1', 'SAFE2', 'P2', '{"PrtflId":"P2"}');`)
	psn := &psnReader{db: db}
	roster, err := psn.roster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cash := roster.portfolioCashAccounts()
	for k, want := range map[portfolioCashKey]string{
		{portfolio: "P1", currency: "CHF"}: "CH00ROSTERCHF",
		{portfolio: "P1", currency: "USD"}: "CH00ROSTERUSD",
	} {
		if cash[k] != want {
			t.Errorf("portfolioCashAccounts[%v] = %q, want %q", k, cash[k], want)
		}
	}
	if got := roster.portfolioOfCashAccount()["CH00ROSTERCHF"]; got != "P1" {
		t.Errorf("portfolioOfCashAccount = %q, want P1", got)
	}
	if got := roster.portfolioOfSafekeeping()["SAFE1"]; got != "P1" {
		t.Errorf("portfolioOfSafekeeping = %q, want P1", got)
	}
	byPortfolio, err := psn.safekeepingByPortfolio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if byPortfolio["P1"] != "SAFE1" || byPortfolio["P2"] != "SAFE2" {
		t.Errorf("safekeepingByPortfolio = %v, want P1 on SAFE1 and P2 on SAFE2", byPortfolio)
	}
}

// The symptom: a trade of the portfolio list settles on a cash account
// the newest batch left unchanged.
func TestAPortfolioTradeSettlesOnAnAccountTheNewestBatchLeftUnchanged(t *testing.T) {
	r := newPortfolioTxFixture(t)
	seedPortfolioTx(t, r, ptxTrade("ptx:REF1"))
	psnDB := newPortfolioPSN(t)
	// A later batch changes the other cash account of the portfolio only.
	silvertest.Exec(t, psnDB, `
INSERT INTO cash_accounts (snapshot_at, relationship_id, account_external_id, portfolio_external_id, payload)
VALUES (2, 'R1', '`+ptxCashHKD+`', '`+ptxPortfolio+`', '{"AcctCcyIsoCd":"HKD","AcctTpDesc":"Changed"}')`)
	tx, ok := emitPortfolio(t, r, psnDB)["ptx:REF1@"+ptxSafe]
	if !ok || tx.AccountExternalID != ptxCashUSD {
		t.Errorf("trade booked = %v on %q, want it on the portfolio's USD cash account", ok, tx.AccountExternalID)
	}
}
