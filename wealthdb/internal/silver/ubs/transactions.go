package ubs

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Transactions emits PSN events. hints.veto carries the event ids whose cash
// movement pairs a web-side mirror in the same-day offset veto (see
// buildSameDayOffsetVeto); those are demoted to a non-flow kind here, exactly
// as the web loop demotes its half — a pair must drop on both sides or the
// survivor books a one-sided phantom external flow. hints.withheld names the
// conversion mirrors the export already records, which are not emitted. Both
// are empty when the merged connection has no web subsource.
//
// Beside the events themselves, two rows the feed does not carry as rows are
// booked here, on the accounts it does not speak for: the other leg of a
// conversion one of its rows describes (conversionMirrors), and the cash a
// corporate action paid (corporateActionCashLegs).
func (c *psnReader) Transactions(ctx context.Context, w canonical.Window, hints psnHints) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}

	ibans, err := c.cashAccountIBANs(ctx)
	if err != nil {
		return nil, err
	}
	settled, err := c.buildSettlementFold(ctx, ibans)
	if err != nil {
		return nil, err
	}
	coverage, err := c.cashCoverage(ctx)
	if err != nil {
		return nil, err
	}
	mirrors, err := c.conversionMirrors(ctx, coverage)
	if err != nil {
		return nil, err
	}
	cashLegs, err := c.corporateActionCashLegs(ctx, w, coverage, ibans)
	if err != nil {
		return nil, err
	}

	const q = `
SELECT event_external_id, timestamp, account_external_id, kind, currency_iso, payload
  FROM events
 WHERE timestamp BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("ubs Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	mirrored := 0
	for rows.Next() {
		var (
			eventID, extID, kind, payload string
			currencyISO                   *string
			occurredAt                    int64
		)
		if err := rows.Scan(&eventID, &occurredAt, &extID, &kind, &currencyISO, &payload); err != nil {
			return nil, fmt.Errorf("ubs Transactions scan: %w", err)
		}

		if settled.covers(kind, extID, currencyISO, occurredAt, payload) {
			continue
		}

		tx, err := buildTransaction(eventID, occurredAt, extID, kind, currencyISO, payload, ibans)
		if err != nil {
			return nil, fmt.Errorf("ubs Transactions (event_id=%s): %w", eventID, err)
		}
		// Same-day offset veto, PSN half. The verdict rides the payload
		// so the row keeps a truthful kind — the spending population
		// reads the kind, and a returns-only judgement must not decide
		// whether a row is spending (see withReturnsFlow).
		if hints.veto[eventID] &&
			(tx.Kind == canonical.TxKindDeposit || tx.Kind == canonical.TxKindWithdrawal) {
			tx.Payload, tx.Kind = markReturnsInternal(tx.Payload, tx.Kind)
		}
		// A conversion the row describes whose other account the feed
		// does not speak for: the row names that account and the leg it
		// states, and the leg itself is booked beside it — unless the
		// export already carries it, in which case the export's row is
		// the one gold holds and the feed row still names it.
		if m, ok := mirrors.bySource[eventID]; ok {
			tx.Payload = withStatedConversion(tx.Payload, m)
			if !hints.withheld[m.tx.TransactionExternalID] {
				mirror := m.tx
				if hints.veto[mirror.TransactionExternalID] {
					mirror.Payload, mirror.Kind = markReturnsInternal(mirror.Payload, mirror.Kind)
				}
				out.Transactions = append(out.Transactions, mirror)
				mirrored++
			}
		}
		out.Transactions = append(out.Transactions, tx)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if mirrored > 0 {
		log.Printf("ubs adapter: booked %d conversion counter-leg(s) on accounts the MT940 feed does not cover", mirrored)
	}
	if len(cashLegs) > 0 {
		log.Printf("ubs adapter: booked %d corporate-action cash leg(s) on accounts the MT940 feed does not cover", len(cashLegs))
		out.Transactions = append(out.Transactions, cashLegs...)
	}
	return silver.NewTransactionStream(out), nil
}

// --- per-kind payload structs ---------------------------------------------

type tradeConfirmationPayload struct {
	Side        string             `json:"side"`
	ISIN        string             `json:"isin"`
	GrossAmount *canonical.Decimal `json:"gross_amount"`
	NetAmount   *canonical.Decimal `json:"net_amount"`
	NetCurrency string             `json:"net_currency"`
	Price       *canonical.Decimal `json:"price"`
	Quantity    *canonical.Decimal `json:"quantity"`
	// The confirmation names the cash account in the bank's INTERNAL
	// form (`:97A::CASH//`), which is not the IBAN the account
	// registry is keyed by — see cashAccountIBANs.
	CashAccountExternalID string `json:"cash_account_external_id"`
	SecurityName          string `json:"security_name"`
	// SettlementDateUnix is `:98A::SETT//`, the day the cash leg hits
	// the account. It is the day the MT940 statement books that leg
	// on, which is what pairs the two records (settlementFold).
	SettlementDateUnix int64 `json:"settlement_date_unix"`
}

type cashMovementPayload struct {
	Amount      *canonical.Decimal `json:"amount"`
	CreditDebit string             `json:"credit_debit"`
	Narrative   string             `json:"narrative"`
	Account     string             `json:"account"`
	Funds       string             `json:"funds"` // currency
	// TxnType is the MT940 :61: transaction type identification code
	// (NTRF, NMSC, NCHG, ...): the bank's own classification of the entry.
	TxnType string `json:"txn_type"`
	// BankRef is the :61: account-servicing-institution reference — the
	// bank's own number for the entry, which the account statement
	// prints as "Transaction no.". It is what lets the era text fold
	// (merge.go) find the export's record of the same booking. Absent
	// where the statement carried none, in which case the collector
	// synthesises the event id from the entry's own fields instead.
	BankRef string `json:"bank_ref"`
}

type corporateActionPayload struct {
	ISIN        string `json:"isin"`
	Safekeeping string `json:"safekeeping"`
	// CAEV is the ISO 15022 corporate-action event indicator (DVCA, ...).
	CAEV string `json:"caev"`
}

// A securities trade reaches PSN twice, on two rails that share no id:
// the MT515 confirmation of the trade and the MT940 `:61:` line for the
// cash leg settling it. Both name the same cash account and the same
// figure, so left alone one trade is two rows in the ledger — and every
// count, turnover and per-instrument total built over it is doubled.
//
// The confirmation is the one that survives, and that is the rail's
// property rather than a per-row judgement: it carries the instrument,
// the quantity, the price and the side, where the statement line
// carries a bare booking code (`B37?`) and no security at all. So the
// fold drops the cash line, exactly as buildEraFold drops the
// reconstruction and keeps the machine-readable record.
//
// Only a line the bank itself typed as a securities settlement is
// eligible (`:61:` NSEC). A trade the MT940 feed never carried — the
// minor-currency cash accounts it does not deliver — has nothing to
// fold and keeps its confirmation, which is why the confirmation is
// also the rail that reads completely.
//
// The whole silver, unwindowed, for buildEraFold's reason: whether a
// booking is recorded twice depends on silver's contents alone and
// never on which slice of time a load happens to cover. A window that
// held the cash line but not its confirmation would otherwise emit the
// duplicate the next window folds away.
type settlementFold struct {
	// unpaired counts the confirmations still to be matched at each
	// key, and covers spends one per cash leg it folds. A set would
	// fold EVERY leg that hashes to a key against a single
	// confirmation: two NSEC lines on one account, day, currency and
	// magnitude — one settling a trade, one not — would both vanish,
	// and the second booking's money would leave the cash ledger
	// entirely. Counting bounds the fold at N legs for N
	// confirmations, which is buildEraFold's 1:1 rule in the form a
	// streaming pass can hold.
	//
	// Which of two indistinguishable legs is folded is left to row
	// order, and deliberately: at equal account, day, currency and
	// magnitude the two differ only in their id, so no field the
	// ledger carries can prefer one.
	unpaired map[settlementKey]int
	ibans    map[string]string
}

// settlementKey is the identity the two rails' id schemes cannot
// express: one cash account, one currency, one figure, one settlement
// day. The magnitude is unsigned because the two rails state direction
// differently — the confirmation in its side, the statement in its
// debit/credit mark — and a fold that disagreed with either about the
// sign would pair nothing.
type settlementKey struct {
	account  string
	currency string
	amount   string
	day      int64
}

// unixDay is the UTC day a timestamp falls in. Both rails date the
// settlement to the day and neither carries a time of day on it.
func unixDay(ts int64) int64 {
	const day = 86400
	d := ts / day
	if ts < 0 && ts%day != 0 {
		d--
	}
	return d
}

func newSettlementKey(account, currency string, amount *canonical.Decimal, settlementUnix int64) (settlementKey, bool) {
	if account == "" || amount == nil || settlementUnix == 0 {
		return settlementKey{}, false
	}
	abs := amount.Abs()
	if abs.IsZero() {
		return settlementKey{}, false
	}
	return settlementKey{
		account:  account,
		currency: strings.ToUpper(strings.TrimSpace(currency)),
		amount:   abs.String(),
		day:      unixDay(settlementUnix),
	}, true
}

// buildSettlementFold reads every trade confirmation the silver holds
// and records the cash leg each one settles.
func (c *psnReader) buildSettlementFold(ctx context.Context, ibans map[string]string) (*settlementFold, error) {
	out := &settlementFold{unpaired: map[settlementKey]int{}, ibans: ibans}
	if c == nil || c.db == nil {
		return out, nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT currency_iso, payload FROM events WHERE kind = 'trade_confirmation'`)
	if err != nil {
		return nil, fmt.Errorf("ubs buildSettlementFold: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			currencyISO *string
			payload     string
		)
		if err := rows.Scan(&currencyISO, &payload); err != nil {
			return nil, fmt.Errorf("ubs buildSettlementFold scan: %w", err)
		}
		var p tradeConfirmationPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return nil, fmt.Errorf("ubs buildSettlementFold payload: %w", err)
		}
		currency := p.NetCurrency
		if currency == "" && currencyISO != nil {
			currency = *currencyISO
		}
		key, ok := newSettlementKey(
			cashIBAN(ibans, p.CashAccountExternalID), currency,
			p.NetAmount, p.SettlementDateUnix)
		if ok {
			out.unpaired[key]++
		}
	}
	return out, rows.Err()
}

// covers reports whether an event is the cash leg of a trade a
// confirmation already records in full, and SPENDS that confirmation:
// each one folds at most one leg, so a second leg at the same key
// survives and reaches gold on its own classification. That is the
// conservative direction — a duplicate row is visible and fixable, a
// vanished booking is not.
func (f *settlementFold) covers(kind, account string, currencyISO *string, occurredAt int64, payload string) bool {
	if f == nil || len(f.unpaired) == 0 || kind != "cash_movement" {
		return false
	}
	var p cashMovementPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return false
	}
	if strings.ToUpper(strings.TrimSpace(p.TxnType)) != "NSEC" {
		return false
	}
	acct := account
	if p.Account != "" {
		acct = p.Account
	}
	currency := p.Funds
	if currency == "" && currencyISO != nil {
		currency = *currencyISO
	}
	key, ok := newSettlementKey(cashIBAN(f.ibans, acct), currency, p.Amount, occurredAt)
	if !ok || f.unpaired[key] == 0 {
		return false
	}
	f.unpaired[key]--
	return true
}

// cashAccountIBANs maps the bank's internal cash-account id onto the
// IBAN the rest of the adapter — and gold's account registry — is keyed
// by. The master-data feed states both, and the MT940 feed already
// names accounts by IBAN; only the MT515 confirmation uses the internal
// form, so without the map its rows name an account nothing else does.
func (c *psnReader) cashAccountIBANs(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if c == nil || c.db == nil {
		return out, nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT DISTINCT json_extract(payload, '$.AcctId'), account_external_id
  FROM cash_accounts`)
	if err != nil {
		return nil, fmt.Errorf("ubs cashAccountIBANs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var acctID, iban *string
		if err := rows.Scan(&acctID, &iban); err != nil {
			return nil, fmt.Errorf("ubs cashAccountIBANs scan: %w", err)
		}
		if acctID != nil && iban != nil && *acctID != "" && *iban != "" {
			out[*acctID] = *iban
		}
	}
	return out, rows.Err()
}

// isSellSide reports whether a trade confirmation's side is the one
// that hands securities back for cash.
//
// The side reaches silver from the order's business function
// (`:22H::BUSE//`), which carries two vocabularies in the one tag: a
// market trade names the party the holder was (`BUYI` / `SELL`), a
// fund order the operation (`SUBS` subscribes, `REDM` redeems). The
// collector folds only the market pair to a common spelling, so a fund
// order arrives under its own word. Read for the market vocabulary
// alone, a redemption fell to the buy default and a disposal was
// booked as an acquisition — cash arriving against a row that says
// cash left.
func isSellSide(side string) bool {
	switch strings.ToUpper(strings.TrimSpace(side)) {
	case "S", "SELL", "REDM":
		return true
	}
	return false
}

// cashIBAN resolves a cash-account id the bank wrote in its internal
// form to the IBAN the account registry is keyed by. An id already in
// IBAN form, or one the map does not cover, is returned as given: the
// map is an improvement on the raw id, never a filter on which rows
// reach gold.
func cashIBAN(ibans map[string]string, id string) string {
	if iban, ok := ibans[id]; ok {
		return iban
	}
	return id
}

// buildTransaction routes a silver event into a TransactionChange.
// Each silverKind variant has its own payload shape; common fields
// (account, instrument, kind) fall out per branch. `ibans` maps the
// bank's internal cash-account ids onto the IBANs the account registry
// is keyed by (cashAccountIBANs); nil where the caller emits no kind
// that names a cash account of its own.
func buildTransaction(eventID string, occurredAt int64, defaultAcct, silverKind string, defaultCcy *string, payload string, ibans map[string]string) (canonical.TransactionChange, error) {
	tx := canonical.TransactionChange{
		TransactionExternalID: eventID,
		OccurredAt:            occurredAt,
		AccountExternalID:     defaultAcct,
		Payload:               json.RawMessage(payload),
	}
	if defaultCcy != nil {
		tx.Currency = *defaultCcy
	}

	switch silverKind {
	case "trade_confirmation":
		var p tradeConfirmationPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return tx, err
		}
		tx.Kind = kindFor(silverKind, "", "", "") // Buy default; side flips below
		if isSellSide(p.Side) {
			tx.Kind = canonical.TxKindSell
		}
		if p.ISIN != "" {
			tx.InstrumentExternalID = &p.ISIN
		}
		// The confirmation books against the cash account its
		// settlement leg moves, named in the bank's internal form.
		// Carried through as given it is an account gold has no
		// record of, so the trade reaches the ledger attached to
		// nothing: no kind, no portfolio, and outside every
		// account-scoped filter.
		if acct := cashIBAN(ibans, p.CashAccountExternalID); acct != "" {
			tx.AccountExternalID = acct
		}
		if p.NetCurrency != "" {
			tx.Currency = p.NetCurrency
		}
		tx.GrossAmount = p.GrossAmount
		tx.NetAmount = p.NetAmount
		tx.Quantity = p.Quantity
		tx.Price = p.Price
		tx.Description = textPtr(p.SecurityName)

	case "cash_movement":
		var p cashMovementPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return tx, err
		}
		tx.Kind = kindFor(silverKind, p.Narrative, p.CreditDebit, p.TxnType)
		if p.Account != "" {
			tx.AccountExternalID = p.Account
		}
		if p.Funds != "" {
			tx.Currency = p.Funds
		}
		// MT940 stores amount as a positive number with the
		// direction in `credit_debit` ("C" / "D"). Pre-sign here
		// so reversals from elsewhere — sources that DO supply a
		// signed amount with a deliberate negative — aren't
		// silently re-flipped by ApplyCanonicalSign. After this
		// branch the helper sees an already-signed amount and
		// only acts when sign and kind agree.
		amt := p.Amount
		if amt != nil && p.CreditDebit == "D" && !amt.IsNegative() {
			n := amt.Neg()
			amt = &n
		}
		tx.NetAmount = amt
		// Text columns (docs/adapters/ubs.md §7): the :86: narrative,
		// flattened to one line, is the description — a bare code
		// passes through as that code — and the :61: type code is the
		// provider category. MT940 carries no structured payee, so no
		// counterparty is derived from the free text. The kind above
		// was classified from the raw narrative and is unaffected.
		tx.Description = narrativeText(p.Narrative)
		tx.ProviderCategory = silver.StrPtrIfNonEmpty(p.TxnType)
		// The one payee MT940 does carry, in the :61: code rather than
		// the text: NCHG is "charges and other expenses" and NCOM a
		// commission, both levied by the account-servicing institution
		// itself. They name no party because the party is the bank,
		// and the narrative on them is usually a bare booking code —
		// so without this the row reaches a report with no merchant at
		// all, or with the code standing in for one.
		if isOwnChargeCode(p.TxnType, p.CreditDebit) {
			tx.Counterparty = silver.StrPtrIfNonEmpty(bankName)
		}

	case "corporate_action_confirmation",
		"corporate_action_notification",
		"corporate_action_narrative":
		var p corporateActionPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return tx, err
		}
		tx.Kind = kindFor(silverKind, "", "", "")
		if p.ISIN != "" {
			tx.InstrumentExternalID = &p.ISIN
		}
		if p.Safekeeping != "" {
			tx.AccountExternalID = p.Safekeeping
		}
		tx.Description = textPtr(p.CAEV)

	default:
		tx.Kind = kindFor(silverKind, "", "", "")
	}

	// Currency is required by the gold schema. If a kind didn't
	// populate it, fall back to "XXX" (ISO 4217 "no currency
	// involved") so the row still loads.
	if tx.Currency == "" {
		tx.Currency = "XXX"
	}

	// Normalise amount signs per the canonical convention. UBS
	// MT940 supplies positive amounts plus a credit/debit flag,
	// and the kind already encodes the direction; collapsing both
	// into a signed amount happens here.
	tx.GrossAmount = canonical.ApplyCanonicalSign(tx.Kind, tx.GrossAmount)
	tx.NetAmount = canonical.ApplyCanonicalSign(tx.Kind, tx.NetAmount)

	return tx, nil
}
