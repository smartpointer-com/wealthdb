package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A currency conversion between two of the relationship's own cash
// accounts is one booking on each, and the MT940 feed carries only the
// accounts it is delivered for. Where it speaks for one side alone,
// that side's :86: narrative still states the whole movement: the
// beneficiary is the relationship itself, and the `/OCMT/` subfield
// names the currency and the figure the other account booked —
//
//	Z24?A. HOLDER U/O B. HOLDER
//	/OCMT/JPY12000000,/
//	KURS JPY/USD 160.0000
//
// The other account follows from that figure's currency: a managed
// portfolio holds one cash account per currency (portfolioCashAccounts),
// and the account the feed booked the movement on says which
// portfolio the conversion stayed inside. So the adapter books the
// other leg itself — a conversion mirror — on that account, for the
// stated amount in the stated currency, on the same value day. Only
// where nothing else records it: the feed does not speak for the
// account on that day (psnCashCoverage), and the export did not emit
// the booking either (the web pass withholds a mirror whose booking it
// already carries — see buildSameDayOffsetVeto).
//
// What it closes is the hole the header of portfolio_txn.go names: an
// account the bank sends no MT940 for had its conversions on no rail at
// all. Without the mirror the paying account shows money leaving the
// relationship and the receiving account shows a purchase funded from
// nothing, and the cash flow statement counts the conversion once as an
// unpaired transfer and once more as the trade it funded.
//
// The two legs then pair exactly as two collected legs would. The
// veto's conversion phase reads the stated leg off the paying row and
// demotes both together for the returns engine (vetoConversions), and
// each leg names the other's account as its counter account, so the
// cash flow statement places both as one own-account move
// (docs/CASHFLOW.md §4). The mirror also carries the bank's reference
// for the entry, the one join that holds across a conversion
// (docs/SPENDING.md §3).
//
// A statement copy of the mirrored leg, reconstructed from a later
// annual statement, folds onto the mirror like any statement copy of a
// feed row (buildEraFold): the derived row keeps the booking and takes
// the statement's narrative.

// conversionMirror is one such counter-leg: the feed row it was read
// from, and the row gold holds for the other side.
type conversionMirror struct {
	source string // event id of the feed row that states the conversion
	tx     canonical.TransactionChange
}

// conversionMirrors is every mirror the silver yields, by the feed row
// each was read from and by the mirror's own id.
type conversionMirrors struct {
	bySource map[string]conversionMirror
	byID     map[string]conversionMirror
}

// mirrorIDPrefix opens a mirror's id, ahead of the feed row's own id.
const mirrorIDPrefix = "mirror:"

func isMirrorID(id string) bool { return strings.HasPrefix(id, mirrorIDPrefix) }

// ocmtInNarrative is the MT940 :86: subfield stating a converted
// movement's original currency and amount, in SWIFT's decimal-comma
// spelling.
var ocmtInNarrative = regexp.MustCompile(`/OCMT/([A-Z]{3})([0-9]+(?:,[0-9]*)?)/`)

// kursInNarrative is the rate line the bank prints under a conversion.
var kursInNarrative = regexp.MustCompile(`KURS\s+([A-Z]{3}/[A-Z]{3})\s+([0-9]+(?:[.,][0-9]+)?)`)

// narrativeBookingCode is the code an :86: narrative opens with, ahead
// of the first subfield.
var narrativeBookingCode = regexp.MustCompile(`^[A-Z0-9]{2,4}\?`)

// statedConversion reads the other leg a feed row's narrative states:
// the currency and the figure, as written. Both or neither.
func statedConversion(narrative string) (currency, amount string) {
	m := ocmtInNarrative.FindStringSubmatch(narrative)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// narrativeBeneficiary is the first line of an :86: narrative less the
// booking code: on a payment order it is the beneficiary, on an
// incoming payment the ordering party.
func narrativeBeneficiary(narrative string) string {
	first, _, _ := strings.Cut(narrative, "\n")
	return strings.TrimSpace(narrativeBookingCode.ReplaceAllString(strings.TrimSpace(first), ""))
}

// normalizeHolderName folds a name to what two feeds spell alike:
// case and the run of spaces the bank pads a name with.
func normalizeHolderName(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), " "))
}

// holderNames reads the names the relationship is held under, as the
// master-data feed states them. A conversion between own accounts names
// the holder on both sides; a payment abroad in a foreign currency,
// which carries the same subfield, names someone else.
func (r *psnReader) holderNames(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	if r == nil || r.db == nil {
		return out, nil
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT DISTINCT json_extract(payload, '$.FrstSurNm') FROM account_holders`)
	if err != nil {
		return nil, fmt.Errorf("ubs holderNames: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("ubs holderNames scan: %w", err)
		}
		if n := normalizeHolderName(name.String); n != "" {
			out[n] = true
		}
	}
	return out, rows.Err()
}

// portfolioOfCashAccount maps each cash account onto the portfolio it
// belongs to, from the account master data's latest snapshot — the
// other half of portfolioCashKey for a row the feed booked on a cash
// account.
func (r *psnReader) portfolioOfCashAccount(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if r == nil || r.db == nil {
		return out, nil
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT account_external_id,
       COALESCE(portfolio_external_id, json_extract(payload, '$.PrtflId'))
  FROM cash_accounts
 WHERE snapshot_at = (SELECT MAX(snapshot_at) FROM cash_accounts)`)
	if err != nil {
		return nil, fmt.Errorf("ubs portfolioOfCashAccount: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var account string
		var portfolio sql.NullString
		if err := rows.Scan(&account, &portfolio); err != nil {
			return nil, fmt.Errorf("ubs portfolioOfCashAccount scan: %w", err)
		}
		if portfolio.String != "" {
			out[account] = portfolio.String
		}
	}
	return out, rows.Err()
}

// mirrorPayload is what a mirror's payload states: the feed row it was
// read from, and the other leg — which is the feed row's own account,
// currency and figure — under the keys every consumer of a counter
// account and a bank reference already reads.
type mirrorPayload struct {
	MirrorOf        string `json:"mirror_of"`
	CounterAccount  string `json:"counter_account"`
	BankRef         string `json:"bank_ref,omitempty"`
	CounterCurrency string `json:"counter_currency"`
	CounterAmount   string `json:"counter_amount"`
	Rate            string `json:"rate,omitempty"`
}

// conversionMirrors reads every conversion the feed describes whose
// other account it does not speak for, and builds the missing leg.
//
// The whole silver, unwindowed, for buildEraFold's reason: which
// bookings exist depends on silver's contents alone, never on which
// slice of time a load covers. The emitting loop selects by the feed
// row's own date.
func (r *psnReader) conversionMirrors(ctx context.Context, coverage psnCashCoverage) (*conversionMirrors, error) {
	out := &conversionMirrors{
		bySource: map[string]conversionMirror{},
		byID:     map[string]conversionMirror{},
	}
	if r == nil || r.db == nil {
		return out, nil
	}
	holders, err := r.holderNames(ctx)
	if err != nil {
		return nil, err
	}
	if len(holders) == 0 {
		return out, nil
	}
	portfolioOf, err := r.portfolioOfCashAccount(ctx)
	if err != nil {
		return nil, err
	}
	cashOf, err := r.portfolioCashAccounts(ctx)
	if err != nil {
		return nil, err
	}
	err = r.eachCashMovement(ctx, "ubs-psn conversionMirrors", func(row psnCashRow) error {
		var p cashMovementPayload
		if err := json.Unmarshal([]byte(row.payload), &p); err != nil || p.Amount == nil {
			return nil
		}
		// A reversal is marked RC / RD; its sign is not the movement's,
		// and a mirror of it would be a mirror of the wrong direction.
		if p.CreditDebit != "C" && p.CreditDebit != "D" {
			return nil
		}
		currency, amount := statedConversion(p.Narrative)
		if currency == "" {
			return nil
		}
		if !holders[normalizeHolderName(narrativeBeneficiary(p.Narrative))] {
			return nil
		}
		source := row.account
		if p.Account != "" {
			source = p.Account
		}
		target := cashOf[portfolioCashKey{portfolio: portfolioOf[source], currency: bookingCurrency(currency)}]
		if target == "" || target == source || coverage.speaksFor(target, row.at) {
			return nil
		}
		figure, err := parseSwiftDecimal(amount)
		if err != nil || figure.IsZero() {
			return nil
		}
		// The feed row's direction, mirrored: what left the paying
		// account arrived on the other.
		kind := canonical.TxKindDeposit
		if p.CreditDebit == "C" {
			kind = canonical.TxKindWithdrawal
		}
		net := canonical.ApplyCanonicalSign(kind, &figure)
		payload := mirrorPayload{
			MirrorOf:        row.eventID,
			CounterAccount:  source,
			BankRef:         p.BankRef,
			CounterCurrency: strings.ToUpper(strings.TrimSpace(p.Funds)),
			CounterAmount:   p.Amount.Abs().String(),
		}
		if payload.CounterCurrency == "" && row.currency.Valid {
			payload.CounterCurrency = strings.ToUpper(strings.TrimSpace(row.currency.String))
		}
		if m := kursInNarrative.FindStringSubmatch(p.Narrative); m != nil {
			payload.Rate = m[1] + " " + m[2]
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("ubs conversionMirrors payload: %w", err)
		}
		mirror := conversionMirror{
			source: row.eventID,
			tx: canonical.TransactionChange{
				TransactionExternalID: mirrorIDPrefix + row.eventID,
				OccurredAt:            row.at,
				AccountExternalID:     target,
				Kind:                  kind,
				Currency:              bookingCurrency(currency),
				NetAmount:             net,
				Description:           narrativeText(p.Narrative),
				ProviderCategory:      textPtr(p.TxnType),
				Payload:               json.RawMessage(encoded),
			},
		}
		out.bySource[row.eventID] = mirror
		out.byID[mirror.tx.TransactionExternalID] = mirror
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// withStatedConversion stamps onto the feed row the other side of the
// conversion it describes — the account the mirror was booked on, and
// the stated leg — under the keys the export era already writes for the
// same facts (withCounterAccount, withCounterLeg).
func withStatedConversion(payload json.RawMessage, m conversionMirror) json.RawMessage {
	out := withCounterAccount(payload, m.tx.AccountExternalID)
	var amount string
	if m.tx.NetAmount != nil {
		amount = m.tx.NetAmount.Abs().String()
	}
	return withCounterLeg(out, m.tx.Currency, amount)
}
