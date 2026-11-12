package canonical

// The keys of a transaction's payload that gold's generic passes read:
// evidence one source states about a movement, which a pass applies to
// every source. A source's own keys, which only its adapter or the lot
// feed reads, are not here.
//
// Each key's writers are named below. Another adapter may write a key
// only with the meaning stated here, since the reader cannot tell the
// writers apart. The txn_evidence() macro (gold derived/40_flows.sql)
// projects the keys the enrichment pass reads as typed columns.
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
	// adapter mapped it. status counts the rows by it. Written by the
	// amex, chase and plaid adapters.
	PayloadSourceKind = "source_kind"
	// PayloadFeeFor names the transaction a fee row belongs to, by its
	// transaction_external_id. The lot writer takes the fee off that
	// sale's proceeds. Written by the equityzen adapter.
	PayloadFeeFor = "fee_for"
	// PayloadEquityTransferLedger is true on a row the loader injects
	// from the equity-transfer ledger. The lot feed reads it. Written by
	// the loader.
	PayloadEquityTransferLedger = "equity_transfer_ledger"
)
