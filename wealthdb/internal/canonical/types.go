package canonical

import (
	"encoding/json"
	"time"
)

// AccountChange is one upsert into gold's `accounts` table.
// Nullable columns are *T; non-nullable columns are T.
// DeclaredSourceID is the silver_source_id of the accounts a
// deployment DECLARES rather than collects (config `declared_accounts`):
// the holder's own accounts at institutions the product does not
// track, written so that a rule can name one as the far side of a
// movement. Reserved: no collected source may take the id, so a
// declaration can never collide with a collected account, and a report
// that groups by source sees the declarations as their own bucket
// rather than blanking a real source's.
const DeclaredSourceID = "declared"

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
	// AccountCategory is a free-text bank-supplied descriptor —
	// UBS's "Custody / Cash-Custody", Schwab's "Personal" /
	// "Custodial", anything the source provides verbatim. Useful
	// as supplementary metadata; for the structured taxonomy use
	// TaxWrapper and ManagementStyle below.
	AccountCategory *string
	// TaxWrapper is the account's tax / regulatory registration
	// — taxable_personal (default), traditional_ira, 529,
	// pillar_3a, trust_non_grantor, etc. Orthogonal to
	// AccountKind (the technical container) and ManagementStyle
	// (who places trades). Adapters populate from silver where
	// possible; config overrides win on overlap.
	TaxWrapper *TaxWrapper
	// ManagementStyle is who places trades — self_directed
	// (default), advisory, discretionary, automated. Orthogonal
	// to AccountKind and TaxWrapper.
	ManagementStyle *ManagementStyle
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
// accounts (UBS, cointracking and fidelity emit them). They do not hold positions or
// cash directly; their value is the sum of their component
// accounts'. See docs/DESIGN.md §13.9 and §7.2.
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
	// AssetClass is the exposure and Vehicle the wrapper — the 2-D
	// instrument taxonomy (TAXONOMY.md). Both required.
	AssetClass  AssetClass
	Vehicle     Vehicle
	ISIN        *string
	CUSIP       *string
	Symbol      *string
	Name        *string
	Currency    *string
	FirstSeenAt int64
	LastSeenAt  int64
	Payload     json.RawMessage
}

// PositionChange is one insert into gold's `positions` table.
type PositionChange struct {
	SilverSourceID       string
	SnapshotAt           int64
	AccountExternalID    string
	PositionKey          string
	InstrumentExternalID *string
	// AssetClass (exposure) + Vehicle (wrapper): the 2-D taxonomy.
	AssetClass      AssetClass
	Vehicle         Vehicle
	Currency        string
	Quantity        *Decimal
	MarketValue     *Decimal
	BookValue       *Decimal
	AccruedInterest *Decimal
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
//	Single-entry, from the account's perspective.
//	Positive  → balance increase (deposit, dividend, coupon,
//	            sell proceeds, transfer_in).
//	Negative  → balance decrease (withdrawal, fee, tax, buy
//	            cost, transfer_out).
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
	// AssetClass and Vehicle are what was TRADED, in the same 2-D
	// taxonomy `positions` and `instruments` carry (TAXONOMY.md):
	// exposure and wrapper. An option on a share is
	// (`public_equity`, `option`) — the exposure is the underlying's
	// and the option is how it was held, which is why a wrapper value
	// never appears in AssetClass.
	//
	// They are columns rather than a lookup through the instrument for
	// the reason `positions` carries them: one instrument is traded in
	// more than one wrapper, so the trio with the instrument is the
	// grain. Both empty where the adapter has nothing to add, which is
	// every row that is not a securities trade and most that are — the
	// instrument's own pair answers for those.
	//
	// Best-effort and denormalised by decision: a transaction points at
	// no position and no lot, because most feeds cannot state which.
	AssetClass AssetClass
	Vehicle    Vehicle
	// InstrumentHint is the token the adapter looked the instrument up
	// by and FAILED on — a valor, a fund name, a ticker, whatever the
	// feed states. Set only where InstrumentExternalID is nil.
	//
	// It exists so the config override that closes the tail has one key
	// for every source. The alternative was for the loader to reach
	// into each feed's payload for a differently-named field, which
	// would put three source-shaped parsers in the one layer that is
	// supposed to be source-blind.
	//
	// Stored (gold migration 0098) rather than consumed and dropped:
	// the token is what a person authoring that link needs to see, and
	// `instrument_external_id IS NULL AND instrument_hint IS NOT NULL`
	// is exactly the set still to close.
	InstrumentHint string
	Kind           TxKind
	Currency       string
	GrossAmount    *Decimal
	NetAmount      *Decimal
	Quantity       *Decimal
	Price          *Decimal
	// Description is a free-text label provided by the adapter
	// when an instrument/account/event identifier doesn't carry
	// enough context on its own. Used as a fallback for the
	// `name` column in CLI output when the instruments-table
	// join misses (typical for Schwab dividends where the
	// payload only has a cash leg plus a top-level description
	// like "VANGUARD TOTAL STOCK MKT ETF"). See gold migration
	// 0005 and docs/adapters/*.md. It is the narrative alone — what
	// the bank wrote; the payer's own message goes in Memo.
	Description *string
	// Memo is the payer's own free text about the row, where the
	// source carries one (the message typed on a UBS e-banking
	// order); nil everywhere else. The gold writer stores it at the
	// END of the description, after DescriptionMemoSeparator
	// (memo.go), so the narrative keeps leading: the memo never
	// enters the merchant signature and never fires a built-in rule.
	// Adapters never compose the join themselves — the writer folds a
	// separator the narrative happens to carry before joining, which
	// is what keeps the separator unambiguous in gold.
	Memo *string
	// Counterparty is the merchant / payee the event settled with.
	// Card adapters populate it; other sources leave it nil. It is
	// not merely informational — it is the input to the merchant
	// signature that groups spend, so the formatting an adapter
	// emits is a stated contract: drift re-keys merchants. Gold
	// migration 0038.
	Counterparty *string
	// ProviderCategory is the provider's own filing of the row,
	// verbatim: a card issuer's spend category ("Groceries",
	// "Travel", …) or a bank's booking type ("ATM WITHDRAWAL",
	// "NTRF", …). Supplementary metadata; never normalised on the
	// way in. The spending provider tier translates it per silver
	// kind (internal/spending/providermap.go).
	ProviderCategory *string
	// CheckNumber is the number written on a paper cheque drawn on the
	// holder's own account, verbatim, kept so a row can be matched
	// against the holder's own paper records. Nil nearly everywhere.
	//
	// An adapter sets it only on an OUTFLOW. The field names an
	// outgoing payment, and the sign test is what keeps it meaning
	// that: a bank may put its own instrument reference in the same
	// silver column on an incoming credit, and a deposit export may
	// head that column "Check or Slip #", where a slip number rides in
	// on an inflow. Gold migration 0075.
	CheckNumber *string
	Payload     json.RawMessage
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
