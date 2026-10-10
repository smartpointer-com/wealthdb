package lots

import "sync"

// Mode says what a pass does for a source.
type Mode uint8

const (
	// Off: the source is not replayed.
	Off Mode = iota
	// Shadow: the replay fills the ledger only, which `gains check`
	// compares with the cost basis the source states; nothing reaches
	// the positions or the realized lots.
	Shadow
	// Fill: the replay also writes the positions and realized lots the
	// source states nothing for.
	Fill
)

var modeNames = [...]string{"off", "shadow", "fill"}

func (m Mode) String() string { return modeNames[m] }

// ParseMode reads a mode as the config spells it.
func ParseMode(s string) (Mode, error) { return parseName[Mode]("lots mode", modeNames[:], s) }

// Grain is the account part of a key.
type Grain uint8

const (
	// GrainAccount keys a book on the account.
	GrainAccount Grain = iota
	// GrainPortfolio pools the accounts of a portfolio into one book:
	// for sources whose accounts are wallets a coin sweeps between, a
	// move inside the portfolio is no event at all.
	GrainPortfolio
)

var grainNames = [...]string{"account", "portfolio"}

func (g Grain) String() string { return grainNames[g] }

// ParseGrain reads a grain as the config spells it.
func ParseGrain(s string) (Grain, error) { return parseName[Grain]("lots grain", grainNames[:], s) }

// Action is what a transaction does to a key, as a policy reads it.
type Action uint8

const (
	// Ignore: the row moves no lot.
	Ignore Action = iota
	// Buy opens a lot at the amount paid.
	Buy
	// Sell relieves lots for the amount received.
	Sell
	// In and Out are the legs of a transfer: paired, a move; unpaired, a
	// receipt of unknown cost or a departure with no proceeds.
	In
	Out
	// Income opens a lot at its market value: a receipt in kind.
	Income
	// Exchange disposes at market value: a fee paid in kind, a spend, a
	// dust sweep.
	Exchange
	// Gone relieves lots with no proceeds: lost, stolen, given away.
	Gone
	// Acquired opens a lot at market value: the receiving side of a
	// dust sweep.
	Acquired
	// Corporate is a corporate action leg; Rule says which.
	Corporate
)

// Corporate rules for a Corporate action.
type CorporateRule uint8

const (
	// CorpReorg pairs the day's outgoing and incoming legs on the
	// account: a merger, conversion, name change or reverse split. An
	// incoming leg alone splits a held instrument or opens a spin-off;
	// an outgoing leg alone is a cash merger (a tender) when it carries
	// an amount, else the lots leave.
	CorpReorg CorporateRule = iota
	// CorpReturnOfCapital lowers the open lots' cost by the amount.
	CorpReturnOfCapital
	// CorpCashInLieu sells the fraction for the amount.
	CorpCashInLieu
	// CorpExpiry disposes at zero proceeds.
	CorpExpiry
	// CorpIgnore moves no lot.
	CorpIgnore
)

// Txn is a transaction as a policy sees it.
type Txn struct {
	ID          string
	Kind        string
	Description string
	// Type is the source's own type, where the adapter keeps one in the
	// payload ("type"); Comment the row's comment ("comment"); Action
	// the payload's "Action" or "action".
	Type, Comment, Action string
	Quantity              float64
	// The trade's two currencies and its fee, where the payload states
	// them ("buy_currency", "sell_currency", "fee_amount",
	// "fee_currency").
	BuyCurrency, SellCurrency string
	FeeAmount                 float64
	FeeCurrency               string
}

// Classified is a policy's reading of one transaction.
type Classified struct {
	Action   Action
	Rule     CorporateRule
	Disposal DisposalKind
	// Fee says the row carries its trade's fee in a third currency,
	// which the feed values and adds to the cost of a buy or takes off
	// the proceeds of a sale. A fee in one of the trade's own currencies
	// is already in its amounts.
	Fee bool
}

// Policy is a source kind's knowledge of its feed: the knobs of
// docs/LOTS.md §6 and the reading of its transactions. A source kind
// registers one beside its adapter (RegisterPolicy); config may
// override Mode, Grain and the method.
type Policy struct {
	Mode  Mode
	Grain Grain
	Fees  Fees
	// Skip names instruments that are never a key, beyond what the
	// positions say: a fiat ticker a crypto source books as an asset.
	Skip func(instrument string) bool
	// Classify reads one transaction; nil reads the canonical kind
	// alone (DefaultClassify).
	Classify func(Txn) Classified
	// DatedBySettlement marks a source whose trade dates are settlement
	// dates, so a term near the one-year line can be off by the lag.
	DatedBySettlement bool
	// Method, when set, is the source kind's own method, which config
	// overrides at any grain but the global one: a source that states
	// an average cost is checked against an average.
	Method *Method
}

// DefaultPolicy is the policy of a source kind that registers none:
// TradingPolicy, off. A lot engine is the wrong model for a source
// nothing says it fits; config can still turn it on.
func DefaultPolicy() Policy {
	p := TradingPolicy()
	p.Mode = Off
	return p
}

// TradingPolicy is the starting point for a source that trades
// securities: fill, keyed on the account, fees unknown.
func TradingPolicy() Policy {
	return Policy{Mode: Fill, Grain: GrainAccount, Fees: FeesUnknown}
}

// A transfer out and a transfer in of the same instrument pair when
// they fall within PairWindow seconds of each other and their
// quantities within PairTolerance of the larger.
const (
	PairWindow    = 7 * 86400
	PairTolerance = 0.05
)

// ShadowPolicy is TradingPolicy in shadow mode at the average method:
// for a source that states an average cost and no lots, where a gap in
// a history would fill with seeds of unknown cost. The ledger is still
// built, and `gains check` compares it with the stated average.
func ShadowPolicy() Policy {
	p := TradingPolicy()
	p.Mode = Shadow
	avg := Average
	p.Method = &avg
	return p
}

// DefaultClassify reads a transaction by its canonical kind and the
// sign of its quantity.
func DefaultClassify(t Txn) Classified {
	switch t.Kind {
	case "buy":
		return Classified{Action: Buy}
	case "sell":
		return Classified{Action: Sell, Disposal: DisposeSell}
	case "transfer_in":
		return Classified{Action: In}
	case "transfer_out":
		return Classified{Action: Out}
	case "journal", "other":
		switch {
		case t.Quantity > 0:
			return Classified{Action: In}
		case t.Quantity < 0:
			return Classified{Action: Out}
		}
	case "staking", "interest", "dividend", "reward", "coupon", "capital_gain", "distribution":
		if t.Quantity > 0 {
			return Classified{Action: Income}
		}
	case "fee":
		if t.Quantity < 0 {
			return Classified{Action: Exchange, Disposal: DisposeFee}
		}
	case "corporate_action":
		return Classified{Action: Corporate, Rule: CorpReorg}
	}
	return Classified{Action: Ignore}
}

// ClassifyCorporate is DefaultClassify with rule reading a corporate
// action: for a source whose corporate actions say what they are only
// in their own words.
func ClassifyCorporate(rule func(Txn) CorporateRule) func(Txn) Classified {
	return func(t Txn) Classified {
		c := DefaultClassify(t)
		if c.Action == Corporate {
			c.Rule = rule(t)
		}
		return c
	}
}

var (
	policyMu sync.RWMutex
	policies = map[string]Policy{}
)

// RegisterPolicy registers a source kind's Policy, from its silver
// package's init beside silver.Register. A second registration for one
// kind panics: it is a build-time mistake.
func RegisterPolicy(kind string, p Policy) {
	policyMu.Lock()
	defer policyMu.Unlock()
	if _, ok := policies[kind]; ok {
		panic("lots: policy for kind " + kind + " already registered")
	}
	policies[kind] = p
}

// PolicyFor is the policy registered for a source kind, DefaultPolicy
// when none is.
func PolicyFor(kind string) (Policy, bool) {
	policyMu.RLock()
	defer policyMu.RUnlock()
	p, ok := policies[kind]
	if !ok {
		return DefaultPolicy(), false
	}
	return p, true
}

// Config is the `lots` block of wealthdb.cfg, parsed: the method in
// force at each grain and the overrides of a source's policy.
type Config struct {
	Method     Method
	Sources    map[string]SourceConfig
	Portfolios map[string]map[string]Method
	Accounts   map[string]map[string]Method
}

// SourceConfig overrides a source's registered policy; nil fields keep
// it.
type SourceConfig struct {
	Method *Method
	Mode   *Mode
	Grain  *Grain
}

// MethodFor resolves the method for a key: the account's, else its
// portfolio's, else its source's, else the source kind's own (own), else
// the global one. A pooled key passes no account.
func (c Config) MethodFor(source, portfolio, account string, own *Method) Method {
	if account != "" {
		if m, ok := c.Accounts[source][account]; ok {
			return m
		}
	}
	if portfolio != "" {
		if m, ok := c.Portfolios[source][portfolio]; ok {
			return m
		}
	}
	if s, ok := c.Sources[source]; ok && s.Method != nil {
		return *s.Method
	}
	if own != nil {
		return *own
	}
	return c.Method
}

// PolicyFor is kind's registered policy with source's overrides applied.
func (c Config) PolicyFor(source, kind string) Policy {
	p, _ := PolicyFor(kind)
	if s, ok := c.Sources[source]; ok {
		if s.Mode != nil {
			p.Mode = *s.Mode
		}
		if s.Grain != nil {
			p.Grain = *s.Grain
		}
	}
	return p
}
