package plaid

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// account is one row of silver's `accounts` table, with its kind and the
// name of its institution as the same run states it.
type account struct {
	id, name, officialName, mask, typ, subtype, currency string
	institution                                          string
	balance, available                                   sql.NullString
	payload                                              string
	kind                                                 canonical.AccountKind
}

// accountChange is the gold account of a silver account row, seen over
// [first, last]. Its category is Plaid's type and subtype, followed by the
// institution's official name for the account, which can say more than
// the subtype.
func accountChange(a account, first, last int64) canonical.AccountChange {
	category := a.typ
	if a.subtype != "" {
		category += " / " + a.subtype
	}
	if a.officialName != "" {
		category += " · " + a.officialName
	}
	return canonical.AccountChange{
		AccountExternalID: a.id,
		AccountKind:       a.kind,
		DisplayName:       silver.StrPtrIfNonEmpty(displayName(a)),
		Nickname:          nickname(a),
		BaseCurrency:      silver.StrPtrIfNonEmpty(a.currency),
		AccountCategory:   silver.StrPtrIfNonEmpty(category),
		TaxWrapper:        wrapperFor(a.kind, a.subtype),
		ManagementStyle:   styleFor(a.kind),
		FirstSeenAt:       first,
		LastSeenAt:        last,
		Payload:           json.RawMessage(a.payload),
	}
}

// displayName is the institution's official name for the account, else
// the institution's own name, followed by the account's mask. Where the
// run states no institution name, Plaid's subtype stands in, else its
// type. The account's own name never shows here: it is the nickname.
func displayName(a account) string {
	name := cmp.Or(a.officialName, a.institution, a.subtype, a.typ)
	if a.mask == "" {
		return name
	}
	return name + " …" + a.mask
}

// nickname is the account's own name, unless it repeats the official name.
// The holder or the institution sets it, and Plaid does not say which, so
// it counts as the holder's words.
func nickname(a account) *string {
	if a.name == a.officialName {
		return nil
	}
	return silver.StrPtrIfNonEmpty(a.name)
}

// bankLedgerKind reports whether the bank and card ledger of an account of
// this kind is projected. A loan's own ledger is not: its payment is booked
// once, on the account it was paid from.
func bankLedgerKind(k canonical.AccountKind) bool {
	return k == canonical.AccountKindCash || k == canonical.AccountKindCard
}

func investmentLedgerKind(k canonical.AccountKind) bool {
	return k == canonical.AccountKindBrokerage || k == canonical.AccountKindCrypto
}

const accountColumns = `account_id, COALESCE(name, ''), COALESCE(official_name, ''),
       COALESCE(mask, ''), COALESCE(type, ''), COALESCE(subtype, ''),
       COALESCE(currency, ''), balance_current, balance_available, payload,
       COALESCE((SELECT institution_name FROM item_states
                  WHERE run_at = accounts.snapshot_at), '')`

func scanAccount(rows *sql.Rows) (account, bool, error) {
	var a account
	if err := rows.Scan(&a.id, &a.name, &a.officialName, &a.mask, &a.typ, &a.subtype,
		&a.currency, &a.balance, &a.available, &a.payload, &a.institution); err != nil {
		return a, false, err
	}
	kind, projected := accountKindFor(a.typ, a.subtype)
	a.kind = kind
	return a, projected, nil
}

// accountsAt are the projected accounts of the run at t.
func (c *Connection) accountsAt(ctx context.Context, t int64) ([]account, error) {
	var out []account
	if err := silver.ScanRows(ctx, c.db, "plaid accountsAt", `SELECT `+accountColumns+`
  FROM accounts WHERE snapshot_at = ? ORDER BY account_id`, func(rows *sql.Rows) error {
		a, projected, err := scanAccount(rows)
		if err != nil {
			return err
		}
		if projected {
			out = append(out, a)
		}
		return nil
	}, t); err != nil {
		return nil, err
	}
	return out, nil
}

// latestAccounts is every projected account in the description of the
// latest run that listed it.
func (c *Connection) latestAccounts(ctx context.Context) (map[string]account, error) {
	out := map[string]account{}
	if err := silver.ScanRows(ctx, c.db, "plaid latestAccounts", `SELECT `+accountColumns+`
  FROM accounts
 WHERE (snapshot_at, account_id) IN (SELECT MAX(snapshot_at), account_id
                                       FROM accounts GROUP BY account_id)`, func(rows *sql.Rows) error {
		a, projected, err := scanAccount(rows)
		if err != nil {
			return err
		}
		if projected {
			out[a.id] = a
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Connection) holdingsAt(ctx context.Context, t int64) ([]holding, error) {
	var out []holding
	if err := silver.ScanRows(ctx, c.db, "plaid holdingsAt", `
SELECT account_id, security_id, quantity, institution_value, cost_basis,
       institution_price, vested_quantity, vested_value, COALESCE(currency, ''),
       tax_lots, payload
  FROM holdings WHERE snapshot_at = ? ORDER BY account_id, security_id, seq`, func(rows *sql.Rows) error {
		var (
			h    holding
			lots string
		)
		if err := rows.Scan(&h.accountID, &h.securityID, &h.quantity, &h.value,
			&h.costBasis, &h.price, &h.vestedQuantity, &h.vestedValue, &h.currency,
			&lots, &h.payload); err != nil {
			return err
		}
		var err error
		if h.lots, err = parseTaxLots(lots); err != nil {
			return fmt.Errorf("plaid holdingsAt: holding %s of %s: %w", h.securityID, h.accountID, err)
		}
		out = append(out, h)
		return nil
	}, t); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Connection) readSecurities(ctx context.Context) (map[string]security, error) {
	out := map[string]security{}
	if err := silver.ScanRows(ctx, c.db, "plaid readSecurities", `
SELECT s.security_id, COALESCE(s.name, ''), COALESCE(s.ticker_symbol, ''),
       COALESCE(s.type, ''), COALESCE(s.currency, ''), COALESCE(s.cusip, ''),
       COALESCE(s.isin, ''), COALESCE(s.cfi_code, ''), s.payload,
       s.security_id IN (
           SELECT security_id FROM holdings
            WHERE CAST(institution_price AS REAL) NOT IN (0, 1)
           UNION ALL
           SELECT security_id FROM investment_transactions
            WHERE security_id IS NOT NULL
              AND LOWER(TRIM(type)) IN ('buy', 'sell')
              AND CAST(price AS REAL) NOT IN (0, 1))
  FROM securities s`, func(rows *sql.Rows) error {
		var s security
		if err := rows.Scan(&s.id, &s.name, &s.ticker, &s.typ, &s.currency,
			&s.cusip, &s.isin, &s.cfi, &s.payload, &s.priced); err != nil {
			return err
		}
		s.bond = readsAsBond(s)
		out[s.id] = s
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}
