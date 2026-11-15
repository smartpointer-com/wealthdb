// Package bank projects the silver of a deposit or card relationship into
// canonical change records. Four collectors share the shape: amex, chase,
// firstcitizens and raiffeisen_at. Their silvers hold the same tables
// (dump_runs, accounts, transactions) and differ in column names, in where
// the closing balances come from, and in the rule that kinds a ledger row.
// A source states those as a Source, and New gives the adapter that walks
// them.
//
// The projection, for every source:
//
//   - one ACCOUNT per roster account, from its latest roster row: cash for a
//     deposit account, card for a credit card, in the taxable personal
//     wrapper and self-directed (silver.BankRosterRow.Account);
//   - CASH BALANCES, never positions: cash is not an instrument. Gold's
//     report_cash macro synthesises the read-time cash position from them.
//     A CURRENT balance from the roster's live figure, and a CLOSING series
//     from the source's own history (Source.Closings);
//   - TRANSACTIONS: the whole ledger.
//
// A card's figures sit in silver the provider's way, the balance as the
// POSITIVE amount owed. Gold carries a revolving-credit liability as
// negative cash, so a card's balances are negated here, and only here
// (signed). A deposit figure passes through. The collectors round money
// to cents before storing, so each REAL→Decimal step is exact at display
// precision.
package bank

import (
	"context"
	"database/sql"
	"math"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Source declares one bank silver: what its roster, balance series and
// ledger are called, and its own rules.
type Source struct {
	// Kind is the silver kind and the collector's name.
	Kind string
	// DefaultCurrency is the currency of an account whose latest roster
	// row names none.
	DefaultCurrency string
	// Returns is the kind's returns policy.
	Returns returns.ReturnsPolicy
	// Clock answers Status and ChangeWindow.
	Clock silver.Clock

	// NameCol and TypeCol are the roster's name and type columns
	// (silver.ReadBankRoster); TypeCol "" reads no type.
	NameCol, TypeCol string
	// NameIsNickname says the name column holds the holder's nickname:
	// the account record carries it as such, and -p treats the display
	// name as the holder's words (silver.LabelTraits.NicknameIsLabel).
	NameIsNickname bool
	// ProductCol is the roster column that tells a card from a deposit
	// account, and CardProduct its value for a card. An id with no
	// roster row reads as a deposit. With ProductCol "", every account
	// is of AccountKind, cash or card.
	ProductCol, CardProduct string
	AccountKind             canonical.AccountKind
	// Display, where set, is an account's display name; nil takes the
	// name, else the mask (silver.BankRosterRow.DisplayName).
	Display func(r silver.BankRosterRow) string
	// Account, where set, completes an account record.
	Account func(acc *canonical.AccountChange) error

	// Listing is the table that records each account a run's roster
	// listed. Each such run then stamps a CURRENT balance
	// (silver.AppendReportedBalances). With Listing "", one CURRENT
	// balance per account is stamped at the source's latest roster
	// snapshot (silver.BankRosterRow.CurrentBalance).
	Listing string
	// Closings are the CLOSING balance series, emitted in this order
	// after the CURRENT balances.
	Closings []Series
	// Probe, where set, reads at Open which optional parts the silver
	// holds and yields the pass Snapshots applies last; nil for none.
	Probe func(ctx context.Context, db *sql.DB) (Pass, error)

	// IDCol is the ledger's id column.
	IDCol string
	// Cols are the ledger's other columns.
	Cols LedgerCols
	// TxKind kinds a ledger row of an account that is a card or not. known
	// is false for a type the rule does not recognise: the raw kind then
	// rides in the payload (canonical.PayloadSourceKind). resign forces
	// the kind's canonical sign on the amount (canonical.ApplyCanonicalSign);
	// without it the source sign is kept.
	TxKind func(r Row, card bool) (kind canonical.TxKind, known, resign bool)
	// Text, where set, revises a row's text columns.
	Text func(r Row, tx *canonical.TransactionChange)
}

// Series is one CLOSING balance series.
type Series struct {
	// Label names the series in a query error, after the kind.
	Label string
	// Basis is the payload's basis.
	Basis string
	// Query yields (account, time, balance) rows, ordered by account and
	// time, whose time lies in the two bounds it binds first; Args follow.
	Query string
	Args  []any
	// DayEnd says Query's time is a day's midnight and the balance speaks
	// for the whole day: it is stamped at the day's last second.
	DayEnd bool
	// Cards says every row is a card's, so its figure is negated whatever
	// the account's product.
	Cards bool
}

// Pass is a source's own step over the snapshot batch.
type Pass func(ctx context.Context, db *sql.DB, batch *canonical.SnapshotBatch) error

// LedgerCols are the SQL expressions of a ledger row's columns, "" where
// the ledger has none.
type LedgerCols struct {
	Kind, Description, Counterparty, Category string
	// TxnDate is the date the row was transacted, beside its post date.
	TxnDate string
	// Currency is the row's own currency; an empty one takes the
	// account's.
	Currency string
	// Pending is nonzero on a row not yet posted.
	Pending string
	// CheckNumber is the row's cheque number.
	CheckNumber string
}

// Row is one ledger row as read: the text columns "" where NULL or absent.
type Row struct {
	ID, Account                               string
	Posted                                    int64
	Amount                                    canonical.Decimal
	Kind, Description, Counterparty, Category string
	TxnDate                                   sql.NullInt64
	Currency, CheckNumber, Payload            string
	Pending                                   bool
}

// Adapter is the silver.Adapter of a Source.
type Adapter struct {
	src *Source
}

// New is the adapter of src.
func New(src Source) *Adapter { return &Adapter{src: &src} }

// Source is the kind's registry entry (silver.Register).
func (a *Adapter) Source() silver.Source {
	rp := a.src.Returns
	return silver.Source{
		Kind:    a.src.Kind,
		Open:    a.Open,
		Returns: &rp,
		Labels:  silver.LabelTraits{NicknameIsLabel: a.src.NameIsNickname},
	}
}

func (a *Adapter) Kind() string { return a.src.Kind }

func (a *Adapter) Open(ctx context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, a.src.Kind+" silver")
	if err != nil {
		return nil, err
	}
	c := &connection{src: a.src, db: db}
	if a.src.Probe != nil {
		if c.pass, err = a.src.Probe(ctx, db); err != nil {
			db.Close()
			return nil, err
		}
	}
	return c, nil
}

type connection struct {
	src  *Source
	db   *sql.DB
	pass Pass
}

func (c *connection) Close() error { return silver.CloseDB(&c.db) }

// Status and ChangeWindow are the source's clock's (silver.Clock).
func (c *connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.src.Clock.Status(ctx, c.db)
}

func (c *connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.src.Clock.ChangeWindow(ctx, c.db, since)
}

// facts is what a projection knows of an account beyond the row it reads:
// its currency and whether it is a card.
type facts struct {
	currency func(accountID string) string
	card     func(accountID string) bool
}

// readFacts reads each account's currency and product from its latest
// roster row.
func (c *connection) readFacts(ctx context.Context) (facts, error) {
	s := c.src
	if s.ProductCol == "" {
		ccy, err := silver.ReadAccountCurrencies(ctx, c.db, s.DefaultCurrency)
		card := s.AccountKind == canonical.AccountKindCard
		return facts{currency: ccy.Of, card: func(string) bool { return card }}, err
	}
	q := `
SELECT a.account_external_id, COALESCE(a.currency, ''), a.` + s.ProductCol + `
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(a2.snapshot_at) FROM accounts a2
                         WHERE a2.account_external_id = a.account_external_id)`
	type fact struct{ currency, product string }
	byID := map[string]fact{}
	err := silver.ScanRows(ctx, c.db, "", q, func(rows *sql.Rows) error {
		var id string
		var f fact
		if err := rows.Scan(&id, &f.currency, &f.product); err != nil {
			return err
		}
		byID[id] = f
		return nil
	})
	return facts{
		currency: func(id string) string {
			if ccy := byID[id].currency; ccy != "" {
				return ccy
			}
			return s.DefaultCurrency
		},
		card: func(id string) bool { return byID[id].product == s.CardProduct },
	}, err
}

// signed is a silver balance in the canonical sign: a card's figure
// negated, a deposit's as stored.
func signed(amount float64, card bool) canonical.Decimal {
	d := canonical.NewDecimalFromFloat(amount)
	if card {
		return d.Neg()
	}
	return d
}

// Snapshots emits, in one batch: one ACCOUNT per roster account, the
// CURRENT balances (Source.Listing), each CLOSING series in turn, then the
// source's own pass.
//
// A CURRENT and a CLOSING mark are told apart by kind rather than by date.
// They can fall on the same day and do not compete when they do.
func (c *connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	f, err := c.readFacts(ctx)
	if err != nil {
		return nil, err
	}
	var batch canonical.SnapshotBatch
	if err := c.appendAccounts(ctx, w, f, &batch); err != nil {
		return nil, err
	}
	if c.src.Listing != "" {
		if err := silver.AppendReportedBalances(ctx, c.db, c.src.Kind, c.src.Listing, w, f.currency, &batch); err != nil {
			return nil, err
		}
	}
	for _, s := range c.src.Closings {
		if err := c.appendClosings(ctx, w, f, s, &batch); err != nil {
			return nil, err
		}
	}
	if c.pass != nil {
		if err := c.pass(ctx, c.db, &batch); err != nil {
			return nil, err
		}
	}
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
}

// appendAccounts emits one AccountChange per roster account from its latest
// snapshot. Without a Listing it adds that account's CURRENT balance when
// the roster captured one.
func (c *connection) appendAccounts(ctx context.Context, w canonical.Window, f facts,
	batch *canonical.SnapshotBatch) error {
	s := c.src
	roster, err := silver.ReadBankRoster(ctx, c.db, s.Kind+" appendAccounts", s.NameCol, s.TypeCol)
	if err != nil {
		return err
	}
	for _, r := range roster {
		card := f.card(r.ID)
		kind := canonical.AccountKindCash
		if card {
			kind = canonical.AccountKindCard
		}
		display := r.DisplayName()
		if s.Display != nil {
			display = s.Display(r)
		}
		currency := f.currency(r.ID)
		acc := r.Account(kind, display, currency)
		if s.NameIsNickname {
			acc.Nickname = silver.StrPtrIfNonEmpty(r.Name)
		}
		if s.Account != nil {
			if err := s.Account(&acc); err != nil {
				return err
			}
		}
		batch.Accounts = append(batch.Accounts, acc)
		if s.Listing != "" {
			continue
		}
		if cb, ok := r.CurrentBalance(w, currency, signed(r.Balance.Float64, card)); ok {
			batch.CashBalances = append(batch.CashBalances, cb)
		}
	}
	return nil
}

// appendClosings emits one CLOSING balance per row of the series whose
// stamp falls in w.
//
// A DayEnd series is read over the days whose last second falls in w,
// then stamped there. A run's CURRENT balance earlier that day holds only
// until the day's close, and the series gives the day's value. A full
// window's integer limits bind as they are: shifted, they would wrap.
func (c *connection) appendClosings(ctx context.Context, w canonical.Window, f facts, s Series,
	batch *canonical.SnapshotBatch) error {
	lo, hi := w.Start, w.End
	if s.DayEnd {
		if lo != math.MinInt64 {
			lo -= silver.DayEndOffset
		}
		if hi != math.MaxInt64 {
			hi -= silver.DayEndOffset
		}
	}
	from := len(batch.CashBalances)
	err := silver.AppendClosingBalances(ctx, c.db, c.src.Kind+" "+s.Label,
		s.Query, append([]any{lo, hi}, s.Args...), `{"basis":"`+s.Basis+`"}`, f.currency,
		func(id string, v float64) canonical.Decimal { return signed(v, s.Cards || f.card(id)) },
		batch)
	if s.DayEnd {
		for i := from; i < len(batch.CashBalances); i++ {
			batch.CashBalances[i].SnapshotAt += silver.DayEndOffset
		}
	}
	return err
}

// Transactions yields the whole ledger, in post-date and id order.
//
// OccurredAt is the POST date: the date the balance moved, which is what
// the balance series is keyed on. A row's charge date (LedgerCols.TxnDate)
// rides in the payload as txn_date rather than replacing it: a
// statement-era row knows only the charge date and fills posted_at with
// it, so the eras disagree about what posted_at holds.
func (c *connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
	}
	s := c.src
	f, err := c.readFacts(ctx)
	if err != nil {
		return nil, err
	}
	text := func(expr string) string {
		if expr == "" {
			return "''"
		}
		return "COALESCE(" + expr + ", '')"
	}
	or := func(expr, none string) string {
		if expr == "" {
			return none
		}
		return expr
	}
	q := `
SELECT ` + s.IDCol + `, posted_at, account_external_id, amount,
       ` + text(s.Cols.Kind) + `, ` + text(s.Cols.Description) + `,
       ` + text(s.Cols.Counterparty) + `, ` + text(s.Cols.Category) + `,
       ` + or(s.Cols.TxnDate, "NULL") + `, ` + text(s.Cols.Currency) + `,
       ` + text(s.Cols.CheckNumber) + `, ` + or(s.Cols.Pending, "0") + `, payload
  FROM transactions
 WHERE posted_at BETWEEN ? AND ?
 ORDER BY posted_at, ` + s.IDCol

	var out canonical.TransactionBatch
	if err := silver.ScanRows(ctx, c.db, s.Kind+" Transactions", q, func(rows *sql.Rows) error {
		var (
			r        Row
			amtFloat float64
			pending  int
		)
		if err := rows.Scan(&r.ID, &r.Posted, &r.Account, &amtFloat, &r.Kind, &r.Description,
			&r.Counterparty, &r.Category, &r.TxnDate, &r.Currency, &r.CheckNumber, &pending,
			&r.Payload); err != nil {
			return err
		}
		r.Amount = canonical.NewDecimalFromFloat(amtFloat)
		r.Pending = pending != 0
		kind, known, resign := s.TxKind(r, f.card(r.Account))
		extra := map[string]any{}
		if !known {
			extra[canonical.PayloadSourceKind] = r.Kind
		}
		if r.TxnDate.Valid {
			extra["txn_date"] = time.Unix(r.TxnDate.Int64, 0).UTC().Format("2006-01-02")
		}
		if r.Pending {
			extra["pending"] = true
		}
		currency := r.Currency
		if currency == "" {
			currency = f.currency(r.Account)
		}
		amt := r.Amount
		net := &amt
		if resign {
			net = canonical.ApplyCanonicalSign(kind, &amt)
		}
		tx := canonical.TransactionChange{
			TransactionExternalID: r.ID,
			OccurredAt:            r.Posted,
			AccountExternalID:     r.Account,
			Kind:                  kind,
			Currency:              currency,
			GrossAmount:           net,
			NetAmount:             net,
			Description:           silver.StrPtrIfNonEmpty(r.Description),
			// The counterparty verbatim, as silver holds it. It is the
			// input to gold's merchant signature, so any reformatting
			// here would re-key every merchant it touched.
			Counterparty: silver.StrPtrIfNonEmpty(r.Counterparty),
			// The provider's own category, verbatim and un-normalised:
			// what the spending provider tier translates without the
			// model.
			ProviderCategory: silver.StrPtrIfNonEmpty(r.Category),
			CheckNumber:      silver.CheckNumberOnOutflow(r.CheckNumber, net),
			Payload:          silver.PayloadWith(r.Payload, extra),
		}
		if s.Text != nil {
			s.Text(r, &tx)
		}
		out.Transactions = append(out.Transactions, tx)
		return nil
	}, w.Start, w.End); err != nil {
		return nil, err
	}
	return silver.NewTransactionStream(out), nil
}
