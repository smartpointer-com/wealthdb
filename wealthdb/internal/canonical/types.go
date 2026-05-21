package canonical

import (
	"encoding/json"
	"time"
)

// AccountChange is one upsert into gold's `accounts` table.
// Nullable columns are *T; non-nullable columns are T.
type AccountChange struct {
	SilverSourceID    string
	AccountExternalID string
	AccountKind       AccountKind
	DisplayName       *string
	BaseCurrency      *string
	RelationshipID    *string
	// Nickname is a free-text user-friendly label. Schwab silver
	// supplies it directly; UBS / Swissquote silvers don't have
	// one today (the config-side override fills in for those).
	Nickname *string
	// AccountCategory is a bank-assigned (or config-overridden)
	// label hinting at the wealth-management wrapper — "managed",
	// "advisory", "personal", "utma", "esa", etc. Adapters
	// populate from silver-provided fields; config overrides
	// take precedence at the load layer.
	AccountCategory *string
	// PortfolioExternalID names the parent portfolio in the
	// `portfolios` table that this account belongs to. UBS cash
	// and safekeeping accounts use it; Schwab and Swissquote
	// leave it nil (no portfolio grouping). Accounts whose
	// PortfolioExternalID is nil aggregate into the sentinel
	// NULL portfolio per silver_source in `wealthdb portfolios`.
	PortfolioExternalID *string
	// FirstSeenAt is the earliest snapshot_at where this account
	// has been observed in the current batch. Gold takes the min
	// with whatever's already stored.
	FirstSeenAt int64
	// LastSeenAt is the latest snapshot_at observed in this batch.
	// Gold takes the max with what's stored.
	LastSeenAt int64
	Payload    json.RawMessage
}

// PortfolioChange is one upsert into gold's `portfolios` table.
// Portfolios are wealth-management wrappers that group component
// accounts (UBS-specific today). They do not hold positions or
// cash directly; their value is the sum of their component
// accounts'. See docs/DESIGN.md §13.9 and the new §7.2 portfolios
// table.
type PortfolioChange struct {
	SilverSourceID      string
	PortfolioExternalID string
	DisplayName         *string
	BaseCurrency        *string
	RelationshipID      *string
	Nickname            *string
	FirstSeenAt         int64
	LastSeenAt          int64
	Payload             json.RawMessage
}

// InstrumentChange is one upsert into gold's `instruments` table.
type InstrumentChange struct {
	SilverSourceID       string
	InstrumentExternalID string
	AssetClass           AssetClass
	ISIN                 *string
	CUSIP                *string
	Symbol               *string
	Name                 *string
	Currency             *string
	FirstSeenAt          int64
	LastSeenAt           int64
	Payload              json.RawMessage
}

// PositionChange is one insert into gold's `positions` table.
type PositionChange struct {
	SilverSourceID       string
	SnapshotAt           int64
	AccountExternalID    string
	PositionKey          string
	InstrumentExternalID *string
	AssetClass           AssetClass
	Currency             string
	Quantity             *Decimal
	MarketValue          *Decimal
	BookValue            *Decimal
	AccruedInterest      *Decimal
	// AcquisitionDate is a calendar date (no time component). Stored
	// as DATE in DuckDB. Use time.Time at UTC midnight.
	AcquisitionDate *time.Time
	Payload         json.RawMessage
}

// CashBalanceChange is one insert into gold's `cash_balances` table.
type CashBalanceChange struct {
	SilverSourceID    string
	SnapshotAt        int64
	AccountExternalID string
	Currency          string
	BalanceKind       BalanceKind
	Amount            Decimal
	Payload           json.RawMessage
}

// FxRateChange is one insert into gold's `fx_rates` table.
type FxRateChange struct {
	SilverSourceID string
	SnapshotAt     int64
	BaseCurrency   string
	QuoteCurrency  string
	MidRate        Decimal
	BidRate        *Decimal
	AskRate        *Decimal
	Payload        json.RawMessage
}

// TransactionChange is one insert into gold's `transactions` table.
//
// Sign convention for GrossAmount / NetAmount (see sign.go for
// the per-kind table and ApplyCanonicalSign helper that adapters
// use to enforce it):
//
//   Single-entry, from the account's perspective.
//   Positive  → balance increase (deposit, dividend, coupon,
//               sell proceeds, transfer_in).
//   Negative  → balance decrease (withdrawal, fee, tax, buy
//               cost, transfer_out).
//
// Summing NetAmount across an account's transactions for a
// period equals that account's net cash flow over the period.
// Sources differ in their raw conventions (Schwab API returns
// signed amounts; UBS MT940 returns absolute amounts plus a
// debit/credit flag); the per-source adapter normalises to this
// convention before assigning to NetAmount / GrossAmount.
type TransactionChange struct {
	SilverSourceID        string
	TransactionExternalID string
	OccurredAt            int64
	AccountExternalID     string
	InstrumentExternalID  *string
	Kind                  TxKind
	Currency              string
	GrossAmount           *Decimal
	NetAmount             *Decimal
	Quantity              *Decimal
	Price                 *Decimal
	Payload               json.RawMessage
}

// Status is the return value of silver.Connection.Status(). See
// docs/DESIGN.md §6.2 for semantics. All timestamps are Unix
// seconds UTC; -1 is the "no observable state" sentinel.
type Status struct {
	OldestSnapshotAt    int64
	LatestSnapshotAt    int64
	OldestTransactionAt int64
	LatestTransactionAt int64
	LatestChangeNumber  int64
}

// Window is the return value of silver.Connection.ChangeWindow().
// See docs/DESIGN.md §6.2.
type Window struct {
	// Start is the earliest changed timestamp in this window
	// (inclusive). Unix seconds UTC.
	Start int64
	// End is the latest changed timestamp in this window (inclusive).
	End int64
	// NewChangeNumber is the value the gold-side high_watermark
	// should advance to after the changes for this window have
	// been committed.
	NewChangeNumber int64
	// HasChanges is false when there's nothing to apply — the
	// caller skips Snapshots/Transactions and may still advance
	// the watermark.
	HasChanges bool
}

// SnapshotBatch is one batch yielded by a SnapshotStream.Next call.
// Adapters multiplex change records of different types into one
// batch; gold applies them in the order: dimensions (portfolios,
// accounts, instruments) before facts (positions, cash_balances,
// fx_rates). Portfolios come first because accounts may reference
// them by portfolio_external_id.
type SnapshotBatch struct {
	Portfolios   []PortfolioChange
	Accounts     []AccountChange
	Instruments  []InstrumentChange
	Positions    []PositionChange
	CashBalances []CashBalanceChange
	FxRates      []FxRateChange
}

// TransactionBatch is one batch yielded by a TransactionStream.Next call.
type TransactionBatch struct {
	Transactions []TransactionChange
}
