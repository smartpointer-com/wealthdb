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
// survivor books a one-sided phantom external flow. hints.rc is the
// reconcile table's verdict (reconcile.go), which the caller always
// passes: the MT940 lines that give way to a trade confirmation are not
// emitted, nor the conversion mirrors and the cash legs it withholds.
// hints.securities links a dividend line past its confirmation and names
// the reversals that are dividend reversals, and hints.funds a line to the
// private-equity fund whose corporate action it books or that it names
// (psnHints). Without a web subsource the stream builds the feed's own.
//
// Beside the events, the stream books rows the feed does not carry:
//
//   - on an account the feed does not speak for, the other leg of a
//     conversion one of its rows describes (conversionMirrors) and the
//     cash a corporate action paid (corporateActionCashLegs);
//   - beside a dividend line or reversal booked at a gross, the tax
//     withheld or taken back (dividends.go).
func (r *psnReader) Transactions(ctx context.Context, w canonical.Window, hints psnHints) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}

	roster, err := r.roster(ctx)
	if err != nil {
		return nil, err
	}
	ibans := roster.cashAccountIBANs()
	rc := hints.rc
	coverage, err := r.cashCoverage(ctx)
	if err != nil {
		return nil, err
	}
	mirrors, err := r.conversionMirrors(ctx)
	if err != nil {
		return nil, err
	}
	cashLegs, err := r.corporateActionCashLegs(ctx, w, coverage, ibans, rc.withheld)
	if err != nil {
		return nil, err
	}
	dividends, err := r.dividends(ctx, ibans, coverage, hints.securities)
	if err != nil {
		return nil, err
	}
	funds := hints.funds
	if funds == nil {
		places, err := cashAccountPortfolios(ctx, r, nil)
		if err != nil {
			return nil, err
		}
		if funds, err = buildPrivateFunds(ctx, r, nil, places); err != nil {
			return nil, err
		}
		if err := funds.pairConfirmations(ctx, r, nil, rc, nil); err != nil {
			return nil, err
		}
	}

	feed, err := r.feed(ctx)
	if err != nil {
		return nil, err
	}
	out := canonical.TransactionBatch{}
	mirrored, grossed := 0, 0
	emit := func(row *feedRow) error {
		eventID, kind := row.eventID, row.kind
		if row.kind == "cash_movement" && rc.lostTo(recFeed, eventID) != "" {
			return nil
		}

		tx, err := row.transaction(ibans)
		if err != nil {
			return fmt.Errorf("ubs Transactions (event_id=%s): %w", eventID, err)
		}
		// A dividend line names its security and, where its confirmation
		// states the gross, reads the gross with the tax withheld beside it.
		// A reversal of a dividend credit is that security's dividend
		// reversal (creditReversal), of the gross where the credit was
		// booked at one.
		var (
			tax   canonical.TransactionChange
			taxed bool
		)
		if kind == "cash_movement" {
			// The payload decodes: transaction read it above.
			p := row.pl
			var reversed bool
			if tax, taxed, reversed, err = dividends.reverse(&tx); err != nil {
				return fmt.Errorf("ubs Transactions (event_id=%s): %w", eventID, err)
			}
			if !reversed && tx.Kind == canonical.TxKindDividend {
				if tax, taxed, err = dividends.settle(&tx, p); err != nil {
					return fmt.Errorf("ubs Transactions (event_id=%s): %w", eventID, err)
				}
			}
			// A line no road above linked takes the private-equity fund
			// whose corporate action it books, else the one its narrative
			// names, and books as what it is (private_funds.go). A line
			// the offset veto pairs, or whose narrative names an internal
			// transfer, moves cash between own accounts and pays no fund
			// itself. An MT940 line states no counter account.
			if tx.InstrumentExternalID == nil && !hints.veto[eventID] && !namesInternalTransfer(p.Narrative) {
				isin, k, ok := funds.pairedLink(eventID, tx.Kind)
				if !ok {
					isin, k, ok = funds.link(tx.AccountExternalID, markedFlow(tx.Kind, p.CreditDebit),
						p.Narrative, p.CustomerRef)
				}
				if ok {
					tx.InstrumentExternalID, tx.Kind = &isin, k
				}
			}
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
			if !rc.withheld[m.tx.TransactionExternalID] {
				mirror := m.tx
				if hints.veto[mirror.TransactionExternalID] {
					mirror.Payload, mirror.Kind = markReturnsInternal(mirror.Payload, mirror.Kind)
				}
				out.Transactions = append(out.Transactions, mirror)
				mirrored++
			}
		}
		out.Transactions = append(out.Transactions, tx)
		if taxed {
			out.Transactions = append(out.Transactions, tax)
			grossed++
		}
		return nil
	}
	for i := range feed.events {
		if row := &feed.events[i]; row.at >= w.Start && row.at <= w.End {
			if err := emit(row); err != nil {
				return nil, err
			}
		}
	}
	if grossed > 0 {
		log.Printf("ubs adapter: booked %d MT940 dividend line(s) or reversal(s) at the gross, a tax row beside each", grossed)
	}
	if mirrored > 0 {
		log.Printf("ubs adapter: booked %d conversion counter-leg(s) on accounts the MT940 feed does not cover", mirrored)
	}
	if len(cashLegs) > 0 {
		log.Printf("ubs adapter: booked %d row(s) from corporate-action confirmations on accounts the MT940 feed does not cover", len(cashLegs))
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
	// on, which is what pairs the two records (settlementKey).
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
	// CustomerRef is the :61: reference for the account owner. On a
	// dividend UBS writes the security's valor there (`VN <valor>`),
	// which links the line to its security (dividends.go).
	CustomerRef string `json:"customer_ref"`
}

type corporateActionPayload struct {
	ISIN        string `json:"isin"`
	Safekeeping string `json:"safekeeping"`
	// CAEV is the ISO 15022 corporate-action event indicator (DVCA, ...).
	CAEV string `json:"caev"`
}

// settlementKey is the identity the two rails' id schemes cannot
// express: one cash account, one currency, one figure, one settlement
// day (UTC; both rails date the settlement to the day and neither
// carries a time of day on it). The magnitude is unsigned because the
// two rails state direction differently — the confirmation in its side,
// the statement in its debit/credit mark — and a fold that disagreed
// with either about the sign would pair nothing.
type settlementKey struct {
	account  string
	currency string
	amount   string
	day      int64
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
		day:      utcDay(settlementUnix),
	}, true
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

// transaction routes a silver event into a TransactionChange.
// Each kind has its own payload shape; common fields (account,
// instrument, kind) fall out per branch. A cash movement and a trade
// confirmation read the payload the feed decoded (decode), and either
// fails where it does not decode. `ibans` maps the bank's internal
// cash-account ids onto the IBANs the account registry is keyed by
// (cashAccountIBANs); nil where the caller emits no kind that names a
// cash account of its own.
func (row *feedRow) transaction(ibans map[string]string) (canonical.TransactionChange, error) {
	tx := canonical.TransactionChange{
		TransactionExternalID: row.eventID,
		OccurredAt:            row.at,
		AccountExternalID:     row.account,
		Payload:               json.RawMessage(row.payload),
	}
	if row.currency.Valid {
		tx.Currency = row.currency.String
	}
	switch silverKind := row.kind; silverKind {
	case "trade_confirmation":
		if row.err != nil {
			return tx, row.err
		}
		p := row.trade
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
		// The confirmation states magnitudes, and its side the
		// direction: the kind carries the side, so the kind signs them.
		tx.GrossAmount = canonical.ApplyCanonicalSign(tx.Kind, p.GrossAmount)
		tx.NetAmount = canonical.ApplyCanonicalSign(tx.Kind, p.NetAmount)
		tx.Quantity = p.Quantity
		tx.Price = p.Price
		tx.Description = textPtr(p.SecurityName)

	case "cash_movement":
		if row.err != nil {
			return tx, row.err
		}
		p := row.pl
		tx.Kind = kindFor(silverKind, p.Narrative, p.CreditDebit, p.TxnType)
		if p.Account != "" {
			tx.AccountExternalID = p.Account
		}
		if p.Funds != "" {
			tx.Currency = p.Funds
		}
		tx.NetAmount = markSigned(tx.Kind, p.CreditDebit, p.Amount)
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
		if err := json.Unmarshal([]byte(row.payload), &p); err != nil {
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

	return tx, nil
}

// markedFlow is the side of a booking a deposit or withdrawal line makes
// or undoes, by its mark: a credit and a credit taken back (`RC`) are a
// deposit's side, a debit and a returned debit (`RD`) a withdrawal's. The
// line's amount (markSigned) says which way the money moved. Any other
// kind, or a line with no mark, keeps its kind.
func markedFlow(kind canonical.TxKind, mark string) canonical.TxKind {
	if kind != canonical.TxKindDeposit && kind != canonical.TxKindWithdrawal {
		return kind
	}
	switch strings.ToUpper(strings.TrimSpace(mark)) {
	case "C", "RC":
		return canonical.TxKindDeposit
	case "D", "RD":
		return canonical.TxKindWithdrawal
	}
	return kind
}

// markSigned is an MT940 line's amount signed by its credit/debit mark.
// MT940 states the amount as a magnitude and the direction in the mark,
// and the mark, not the kind, says which way the money moved. A credit
// (`C`) and the reversal of a debit (`RD`, a returned payment) bring
// money in. A debit (`D`) and the reversal of a credit (`RC`, the bank
// taking it back) take money out. The kind's canonical sign would book
// either reversal as a second copy of what it cancels: a `DIVIDENDE`
// narrative on an `RC` as a dividend paid, a returned payment as a
// second payment. The stream, the reconcile table and the offset veto
// all project the row here, so they read the one sign. A line with no
// mark is signed by its kind (canonical.ApplyCanonicalSign).
func markSigned(kind canonical.TxKind, mark string, amount *canonical.Decimal) *canonical.Decimal {
	if amount == nil {
		return nil
	}
	var signed canonical.Decimal
	switch strings.ToUpper(strings.TrimSpace(mark)) {
	case "C", "RD":
		signed = amount.Abs()
	case "D", "RC":
		signed = amount.Abs().Neg()
	default:
		return canonical.ApplyCanonicalSign(kind, amount)
	}
	return &signed
}
