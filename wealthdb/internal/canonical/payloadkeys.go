package canonical

// The keys of a transaction's payload that gold's generic passes read:
// evidence one source states about a movement, which a pass applies to
// every source. A source's own keys, which only its adapter or the lot
// feed reads, are not here.
//
// Each key's writers are named below. Another adapter may write a key
// only with the meaning stated here, since the reader cannot tell the
// writers apart. The txn_evidence() macro (gold derived/40_flows.sql)
// projects every key but PayloadCostBasis, PayloadHolding and the two
// pair keys as a typed column. The enrichment pass reads the first four
// from it, and the other readers extract theirs from the payload.
const (
	// PayloadBankRef is the reference a bank stamps on both halves of
	// one movement, minted once per movement. The enrichment pass pairs
	// two legs on it. Written by the ubs collectors and adapter and by
	// the synthetic adapter.
	PayloadBankRef = "bank_ref"
	// PayloadCounterAccount is the account on the other side of the
	// movement, as the source prints it. The enrichment pass takes it as
	// the far account where gold holds that account. Written by the ubs
	// adapter (from the statement parser), the svb collector and the
	// synthetic adapter.
	PayloadCounterAccount = "counter_account"
	// PayloadCounterCurrency and PayloadCounterAmount are the other leg
	// as the source describes it: the currency the money became or came
	// from, and the figure. The enrichment pass pairs a leg with the one
	// leg that answers. Written by the ubs adapter and the synthetic
	// adapter.
	PayloadCounterCurrency = "counter_currency"
	PayloadCounterAmount   = "counter_amount"
	// PayloadReturnsFlow is "internal" on a row the adapter calls conduit
	// churn rather than the owner's capital. Returns reads it under a
	// policy's ExternalOnly. Written by the ubs adapter.
	PayloadReturnsFlow = "returns_flow"
	// PayloadSourceKind is the source's own kind for the row, before the
	// adapter mapped it. status counts the rows that carry it under a
	// kind other than 'other' (GuessedTxKindCount). Written by the amex,
	// chase and plaid adapters.
	PayloadSourceKind = "source_kind"
	// PayloadFeeFor names the transaction a fee row belongs to, by its
	// transaction_external_id. The lot writer takes the fee off that
	// sale's proceeds. Written by the equityzen adapter.
	PayloadFeeFor = "fee_for"
	// PayloadEquityTransferLedger is true on a row the loader injects
	// from the equity-transfer ledger. The lot feed reads it. Written by
	// the loader.
	PayloadEquityTransferLedger = "equity_transfer_ledger"
	// PayloadCostBasis is the basis the equity-transfer ledger states for
	// the shares a row moves, beside PayloadEquityTransferLedger. The lot
	// feed reads it on a ledger row. Written by the loader, on the same
	// rows.
	PayloadCostBasis = "cost_basis"
	// PayloadPairSource and PayloadPairTransaction name the other leg
	// of a move one equity-transfer ledger row states, by its
	// silver_source_id and transaction_external_id. The enrichment pass
	// links the two legs on them. Written by the loader, on the two
	// legs of such a row only.
	PayloadPairSource      = "pair_silver_source_id"
	PayloadPairTransaction = "pair_transaction_external_id"
	// PayloadHolding names the private holding an equity-transfer ledger
	// out-leg distributes, by the source's instrument id, where the leg
	// moves another instrument: an SPV's exit in the listed shares it
	// received. The private-markets view (private_legs, gold
	// derived/30_holdings.sql) counts the leg as that holding's
	// distribution. Written by the loader, on such an out-leg only.
	PayloadHolding = "holding"
)

// LedgerTransferIDPrefix starts the transaction id of every leg the
// equity-transfer ledger asserts (docs/DESIGN.md §13.9): a hand-stated
// row, no collected one. A load deletes and reinserts a source's ledger
// rows by it without touching the adapter's rows, returns tells a ledger
// leg from a collected row by it, and the enrichment pass bounds its
// reads of the pair keys to such rows. Written by the loader.
const LedgerTransferIDPrefix = "xfer:"
