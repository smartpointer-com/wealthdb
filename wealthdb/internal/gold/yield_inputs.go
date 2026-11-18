package gold

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// YieldInputs assembles what the yield report and the income projection
// read at the instant asOf, valued in ccy (docs/YIELD.md): the holdings
// as `holdings global` values them, their accounts, terms and rates, the
// funds' figures and lines, the curves, the income of the twelve months
// before, and the reported figures of the securities held.
func YieldInputs(ctx context.Context, db querier, asOf int64, ccy string) (*yield.Inputs, error) {
	base, err := readYieldBase(ctx, db, asOf, &yieldShared{}, []string{ccy})
	if err != nil {
		return nil, err
	}
	return base.inputs(ccy), nil
}

// yieldBase is what the inputs of one instant read, once for the
// currencies it is valued in: the accounts, terms, rates, conventions and
// curves, the ledger's dividends, the withholding table, the aliases and
// the instruments held, the dividend calendar and the settlement cycles,
// each holding's amount on each day of the twelve months to the instant,
// the reported figures of the stocks held, the income lines and the units
// of the instruments they pay, and the funds held; and each currency's
// valuation. Every currency's inputs share its maps, and none changes
// them.
type yieldBase struct {
	in *yield.Inputs
	// amounts is each position's amount day by day, in each currency a
	// snapshot states it in (readAmounts), and instrument the instrument
	// of each.
	amounts    map[amountKey]*yield.Daily
	instrument map[yield.HoldingKey]string
	// lag is the lag of the record day of the dividends of each instrument
	// held, where its market fixes one; market the market whose settlement
	// cycle derives their record day from an ex day, where one is known;
	// and share says the instrument is a share, a receipt among them
	// (readYieldHeld).
	lag    map[yield.InstrumentKey]yield.Lag
	market map[yield.InstrumentKey]string
	share  map[yield.InstrumentKey]bool
	// calendar are the distributions the dividend calendar states, by the
	// key of the security (readCalendar), cycles each market's settlement
	// cycles (readSettlementCycles), and gaps the gaps its issuers'
	// distributions teach (readDividendGaps), all shared.
	calendar map[string][]yield.CalendarEvent
	cycles   yield.SettlementCycles
	gaps     dividendGaps
	// currencies are the currencies the instant is valued in, and valued
	// each one's valuation, in their order (readValuations).
	currencies []string
	valued     []valuation
	// lines are the income lines of the 24 months to the instant, values
	// each one's value in each of the currencies (readIncome), and
	// attributions what they pay under each set of holdings a currency
	// holds (attribution).
	lines        []incomeLine
	values       [][]*float64
	attributions []*incomeAttribution
	// units are the units of the instruments the income lines pay
	// (readIncome).
	units *yieldUnits
	// funds are the funds some currency holds, by key (readFunds).
	funds map[string]*fundRead
}

// valuation is what an instant's inputs read in one output currency: the
// holdings as `holdings global` values them, each one's legal situs where
// known, and the exchange rates.
type valuation struct {
	holdings []yield.Holding
	situs    map[yield.HoldingKey]string
	fx       map[string]float64
}

// yieldShared is what the bases of several instants read once, as no
// instant changes it: the positions the situs rule reads as receipts, the
// dividend calendar, the settlement cycles, the learned gaps, and the
// holder of each transaction placed (readPlacements).
type yieldShared struct {
	receipts map[[3]string]bool
	calendar map[string][]yield.CalendarEvent
	cycles   yield.SettlementCycles
	gaps     dividendGaps
	placed   map[yield.TxnKey]unitKey
}

// readYieldBase reads the instant asOf's yieldBase, valued in each
// currency of currencies. The positions read as receipts, the dividend
// calendar, the settlement cycles and the learned gaps come from shared,
// read on the first instant that needs them, as are the transactions'
// holders.
func readYieldBase(ctx context.Context, db querier, asOf int64, shared *yieldShared,
	currencies []string) (*yieldBase, error) {
	day := time.Unix(asOf, 0).UTC()
	in := &yield.Inputs{AsOf: asOf, Day: time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC),
		Accounts: map[yield.AccountKey]yield.Account{}, Terms: map[yield.InstrumentKey]yield.Terms{},
		Rates: map[yield.AccountKey][]yield.Rate{}, Curves: map[string]*yield.Curve{},
		Dividends: map[yield.InstrumentKey]canonical.DeclaredDividend{}, Securities: map[string]yield.Facts{},
		Withholding: map[string]map[string]float64{}, AccountWithholding: map[yield.AccountKey]map[string]float64{},
		Held: map[yield.InstrumentKey][]string{}, HeldSecurity: map[yield.InstrumentKey]string{},
		Aliases: map[yield.AliasKey]string{}}
	b := &yieldBase{in: in, lag: map[yield.InstrumentKey]yield.Lag{},
		market: map[yield.InstrumentKey]string{}, share: map[yield.InstrumentKey]bool{},
		currencies: currencies, funds: map[string]*fundRead{}}
	if err := runSteps("YieldInputs", []step{
		{"accounts", func() error { return readYieldAccounts(ctx, db, in) }},
		{"terms", func() error { return readYieldTerms(ctx, db, in) }},
		{"rates", func() error { return readYieldRates(ctx, db, in) }},
		{"conventions", func() error {
			c, err := readConventions(ctx, db)
			in.Conventions = c
			return err
		}},
		{"curves", func() error { return readYieldCurves(ctx, db, in) }},
		{"dividends", func() error { return readDeclaredDividends(ctx, db, in) }},
		{"daily", func() error { return b.readAmounts(ctx, db) }},
		{"aliases", func() error { return readYieldAliases(ctx, db, in) }},
		{"held", func() error { return readYieldHeld(ctx, db, in, b, shared) }},
		{"calendar", func() error {
			err := shared.readDividendCalendar(ctx, db)
			b.calendar, b.cycles, b.gaps = shared.calendar, shared.cycles, shared.gaps
			return err
		}},
		{"withholding", func() error { return readWithholding(ctx, db, in) }},
		{"holdings", func() error { return b.readValuations(ctx, db) }},
		{"funds", func() error { return b.readFunds(ctx, db) }},
		{"placed", func() error {
			var err error
			if shared.placed == nil {
				shared.placed, err = readPlacements(ctx, db, nil)
			}
			in.Placed = shared.placed
			return err
		}},
		{"income", func() error { return b.readIncome(ctx, db) }},
		{"securities", func() error { return b.readSecurities(ctx, db) }},
	}); err != nil {
		return nil, err
	}
	return b, nil
}

// inputs are the base's instant valued in ccy, one of its currencies: the
// base's figures, the holdings as `holdings global` values them, the
// funds held and their lines, the income of the twelve months before,
// each holding's daily amounts and the exchange rates.
func (b *yieldBase) inputs(ccy string) *yield.Inputs {
	ci := slices.Index(b.currencies, ccy)
	v := b.valued[ci]
	in := *b.in
	in.Currency = ccy
	in.Funds, in.Income, in.AccountIncome = map[string]*yield.Fund{}, map[yield.HoldingKey]yield.Received{},
		map[yield.AccountCurrency]yield.Received{}
	in.Daily, in.FormerDaily = map[yield.HoldingKey]yield.Daily{}, map[yield.InstrumentKey][]yield.Daily{}
	in.Holdings, in.Situs, in.FXRates = slices.Clone(v.holdings), v.situs, v.fx
	b.pickFunds(&in)
	in.Index()
	b.income(&in, ci)
	b.daily(&in)
	return &in
}

// readValuations reads each currency's valuation of the instant
// (yieldBase.valued): the holdings and their situs in one statement, the
// exchange rates in another.
func (b *yieldBase) readValuations(ctx context.Context, db querier) error {
	b.valued = make([]valuation, len(b.currencies))
	for i := range b.valued {
		b.valued[i] = valuation{situs: map[yield.HoldingKey]string{}, fx: map[string]float64{}}
	}
	if err := readYieldHoldings(ctx, db, b.in.AsOf, b.currencies, b.valued); err != nil {
		return err
	}
	return readYieldFX(ctx, db, b.in.AsOf, b.currencies, b.valued)
}

// readYieldHoldings reads the holdings at the instant asOf valued in each
// currency of currencies, and their situs, into the valuation of each.
func readYieldHoldings(ctx context.Context, db querier, asOf int64, currencies []string, out []valuation) error {
	branches := make([]string, len(currencies))
	args := []any{asOf}
	for i, ccy := range currencies {
		args = append(args, ccy)
		branches[i] = fmt.Sprintf("SELECT %d AS v, * FROM yield_holdings($1, $%d)", i, len(args))
	}
	return scanAll(ctx, db, `
SELECT h.v, h.src, h.acct, h.position_key, COALESCE(h.iid, ''), COALESCE(h.asset_class, ''), COALESCE(h.vehicle, ''),
       COALESCE(h.ccy, ''), h.is_cash, h.value, COALESCE(h.amount, 0), h.quantity, h.accrued, h.acquisition_date,
       COALESCE(h.symbol, ''), COALESCE(h.name, ''), COALESCE(h.key, ''),
       COALESCE(h.key, '`+HoldingKeyPrefix+`' || h.src || ':' || COALESCE(h.iid, h.position_key)), COALESCE(st.situs, '')
  FROM (`+strings.Join(branches, "\n        UNION ALL ")+`) h
  LEFT JOIN exposure_situs_at($1) st ON st.silver_source_id = h.src AND st.account_external_id = h.acct
       AND st.position_key = h.position_key
 WHERE h.value IS NOT NULL AND ABS(COALESCE(h.amount, 0)) >= 0.005
 ORDER BY h.v, h.src, h.acct, h.position_key`, func(rows *sql.Rows) error {
		var v int
		var h yield.Holding
		var qty, accrued sql.NullFloat64
		var acquired sql.NullTime
		var situs string
		if err := rows.Scan(&v, &h.Source, &h.Account, &h.PositionKey, &h.Instrument, &h.AssetClass, &h.Vehicle,
			&h.Currency, &h.IsCash, &h.Value, &h.Amount, &qty, &accrued, &acquired, &h.Symbol, &h.Name, &h.SecurityID,
			&h.Key, &situs); err != nil {
			return err
		}
		h.Quantity, h.Accrued, h.Acquired = nullFloatToPtr(qty), nullFloatToPtr(accrued), nullDate(acquired)
		out[v].holdings = append(out[v].holdings, h)
		if situs != "" {
			out[v].situs[h.HoldingKey] = situs
		}
		return nil
	}, args...)
}

func readYieldAccounts(ctx context.Context, db querier, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT a.silver_source_id, a.account_external_id, COALESCE(a.display_name, ''), COALESCE(a.account_kind, ''),
       account_wrapper(a.tax_wrapper), a.tax_wrapper IS NULL, COALESCE(a.nickname, ''),
       COALESCE(a.portfolio_external_id, ''), COALESCE(p.display_name, '')
  FROM accounts a
  LEFT JOIN portfolios p ON p.silver_source_id = a.silver_source_id
       AND p.portfolio_external_id = a.portfolio_external_id`, func(rows *sql.Rows) error {
		var k yield.AccountKey
		var a yield.Account
		if err := rows.Scan(&k.Source, &k.Account, &a.Name, &a.Kind, &a.TaxWrapper, &a.DefaultWrapper, &a.Nickname,
			&a.Portfolio, &a.PortfolioName); err != nil {
			return err
		}
		in.Accounts[k] = a
		return nil
	})
}

func readYieldTerms(ctx context.Context, db querier, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT silver_source_id, instrument_external_id, coupon_pct, coupon_origin = 'declared', maturity,
       maturity_origin = 'declared', frequency, coupon_kind, day_count, call_date, make_whole, rating, in_default,
       fixed_until, reset_date, duration, yield_pct, terms_as_of
  FROM instrument_terms_at(?)`, func(rows *sql.Rows) error {
		var k yield.InstrumentKey
		var t yield.Terms
		var coupon, duration, yieldPct sql.NullFloat64
		var couponDeclared, maturityDeclared sql.NullBool
		var maturity, call, fixed, reset sql.NullTime
		var frequency, asOf sql.NullInt64
		var kind, dayCount, rating sql.NullString
		if err := rows.Scan(&k.Source, &k.Instrument, &coupon, &couponDeclared, &maturity, &maturityDeclared, &frequency,
			&kind, &dayCount, &call, &t.MakeWhole, &rating, &t.InDefault, &fixed, &reset, &duration, &yieldPct,
			&asOf); err != nil {
			return err
		}
		t.Coupon, t.Duration, t.Yield = nullFloatToPtr(coupon), nullFloatToPtr(duration), nullFloatToPtr(yieldPct)
		t.Maturity, t.CallDate, t.FixedUntil, t.Reset = nullDate(maturity), nullDate(call), nullDate(fixed), nullDate(reset)
		t.CouponDeclared, t.MaturityDeclared = couponDeclared.Bool, maturityDeclared.Bool
		if frequency.Valid {
			f := int(frequency.Int64)
			t.Frequency = &f
		}
		t.CouponKind, t.DayCount, t.Rating = canonical.CouponKind(kind.String), canonical.DayCount(dayCount.String),
			rating.String
		if asOf.Valid {
			d := time.Unix(asOf.Int64, 0).UTC()
			t.TermsAsOf = &d
		}
		in.Terms[k] = t
		return nil
	}, in.AsOf)
}

// readYieldRates reads the account rates in force at the instant:
//
//   - a rate read from a snapshot: the latest snapshot's on or before it
//     whose rate holds on the day, so a rate a snapshot states for a later
//     day leaves the one in force standing until then;
//   - a rate a source states once: if it holds from on or before the day;
//   - of either, the newest schedule that holds, every tier of it;
//   - a ledger record's: the latest dated on or before the day, else the
//     undated one. It replaces the credit rates a source states for that
//     account and currency; their debit rates stay.
func readYieldRates(ctx context.Context, db querier, in *yield.Inputs) error {
	return scanAll(ctx, db, `
WITH observed AS (
    SELECT * FROM account_rates
     WHERE observed_at IS NOT NULL AND observed_at <= $1
       AND (valid_from IS NULL OR valid_from <= CAST(to_timestamp($1) AT TIME ZONE 'UTC' AS DATE))
   QUALIFY observed_at = MAX(observed_at) OVER (PARTITION BY silver_source_id, account_external_id, currency, side)),
once AS (
    SELECT * FROM account_rates WHERE observed_at IS NULL AND origin = 'stated'),
stated AS (
    SELECT * FROM (SELECT * FROM observed UNION ALL SELECT * FROM once)
     WHERE valid_from IS NULL OR valid_from <= CAST(to_timestamp($1) AT TIME ZONE 'UTC' AS DATE)
   QUALIFY rank() OVER (PARTITION BY silver_source_id, account_external_id, currency, side
                        ORDER BY valid_from DESC NULLS LAST, observed_at DESC NULLS LAST) = 1),
declared AS (
    SELECT * FROM account_rates
     WHERE origin = 'declared'
       AND (valid_from IS NULL OR valid_from <= CAST(to_timestamp($1) AT TIME ZONE 'UTC' AS DATE))
   QUALIFY row_number() OVER (PARTITION BY silver_source_id, account_external_id, currency
                              ORDER BY valid_from DESC NULLS LAST, line_no DESC) = 1)
SELECT silver_source_id, account_external_id, COALESCE(currency, ''), side, tier, rate_pct, up_to, valid_from,
       term_end, origin = 'declared'
  FROM (SELECT * FROM declared
        UNION ALL
        SELECT s.* FROM stated s ANTI JOIN declared d ON d.silver_source_id = s.silver_source_id
               AND d.account_external_id = s.account_external_id AND d.side = s.side
               AND COALESCE(d.currency, '') IN (COALESCE(s.currency, ''), ''))
 ORDER BY 1, 2, 3, 4, 5`, func(rows *sql.Rows) error {
		var k yield.AccountKey
		var r yield.Rate
		var side string
		var upTo sql.NullFloat64
		var validFrom, termEnd sql.NullTime
		if err := rows.Scan(&k.Source, &k.Account, &r.Currency, &side, &r.Tier, &r.Pct, &upTo, &validFrom, &termEnd,
			&r.Declared); err != nil {
			return err
		}
		r.Credit, r.UpTo, r.ValidFrom, r.TermEnd = side == canonical.RateSideCredit, nullFloatToPtr(upTo), nullDate(validFrom), nullDate(termEnd)
		in.Rates[k] = append(in.Rates[k], r)
		return nil
	}, in.AsOf)
}

func readDeclaredDividends(ctx context.Context, db querier, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT silver_source_id, instrument_external_id, amount, frequency, COALESCE(currency, '')
  FROM yield_declared_dividends
 WHERE as_of IS NULL OR as_of <= ?
QUALIFY row_number() OVER (PARTITION BY silver_source_id, instrument_external_id
                           ORDER BY as_of DESC NULLS LAST, line_no DESC) = 1`, func(rows *sql.Rows) error {
		var k yield.InstrumentKey
		var d canonical.DeclaredDividend
		if err := rows.Scan(&k.Source, &k.Instrument, &d.Amount, &d.Frequency, &d.Currency); err != nil {
			return err
		}
		in.Dividends[k] = d
		return nil
	}, in.AsOf)
}

// readYieldAliases reads the ids the matching folds into others
// (yield.Inputs.Aliases), as holdingAliasJoins reads a holding's key: a
// holding alias for one account, else for every account of its source,
// else, for the CUSIP id of an instrument its source also keys by its
// ticker, the ticker (instrument_aliases).
func readYieldAliases(ctx context.Context, db querier, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT silver_source_id, account_external_id, from_key, to_key FROM holding_aliases
UNION ALL
SELECT ia.src, '', ia.alias, ia.k
  FROM instrument_aliases() ia
  ANTI JOIN holding_aliases hd ON hd.silver_source_id = ia.src AND hd.account_external_id = ''
       AND hd.from_key = ia.alias`, func(rows *sql.Rows) error {
		var k yield.AliasKey
		var to string
		if err := rows.Scan(&k.Source, &k.Account, &k.ID, &to); err != nil {
			return err
		}
		in.Aliases[k] = to
		return nil
	})
}

// readYieldHeld reads, for each instrument a position held at some
// snapshot to the instant, by the id the matching folds it into
// (yield.Inputs.Fold): the accounts that held it (Held); its security, as
// exposure_holdings keys a holding (HeldSecurity); and into the base the
// lag of the record day of its dividends (yield.RecordLag), the market
// whose settlement cycle derives their record day from an ex day
// (yield.SettlementMarket), and whether it is a share. The lag and the
// market read each position's situs at the instant and its basis
// (exposure_situs_at), as the withholding table reads a holding's: the
// situs of a receipt of basis underlying is that of the share it stands
// for. Any position the situs rule reads as a receipt is one
// (receiptPositions): one whose situs is declared reads the declared
// country as its share's, one whose share is unknown has no lag. The
// currency is the one its instrument is listed in, where gold states one,
// else the one its newest snapshot values it in. Of the positions folded
// into one instrument, the one seen newest gives the lag, the market and
// whether it is a share; two seen at that instant that disagree give no
// lag or no market.
func readYieldHeld(ctx context.Context, db querier, in *yield.Inputs, b *yieldBase, shared *yieldShared) error {
	type reading struct {
		k                      yield.InstrumentKey
		position               [3]string
		situs, basis, currency string
		share                  bool
		seen                   int64
	}
	var readings []reading
	maybe := false
	if err := scanAll(ctx, db, `
WITH p AS (
    SELECT silver_source_id AS src, account_external_id AS acct, position_key, instrument_external_id AS iid,
           COALESCE(arg_max(vehicle = 'stock' AND asset_class = 'public_equity', snapshot_at), FALSE) AS share,
           COALESCE(arg_max(currency, snapshot_at), '') AS currency, MAX(snapshot_at) AS seen
      FROM positions
     WHERE snapshot_at <= $1 AND instrument_external_id IS NOT NULL
     GROUP BY ALL),
k AS (
    SELECT p.*, COALESCE(ha.to_key, hd.to_key, ia.k) AS akey
      FROM p`+holdingAliasJoins("p.src", "p.acct", "p.iid", "p.position_key")+`)
SELECT k.src, k.acct, k.position_key, k.iid,
       COALESCE(exposure_key(COALESCE(ik1.security_key, ik2.security_key)), ''), COALESCE(st.situs, ''),
       COALESCE(st.basis, ''), k.share, COALESCE(NULLIF(i.currency, ''), k.currency), k.seen
  FROM k
  LEFT JOIN instrument_keys ik1 ON ik1.silver_source_id = k.src AND ik1.instrument_external_id = k.akey
  LEFT JOIN instrument_keys ik2 ON ik2.silver_source_id = k.src AND ik2.instrument_external_id = k.iid
  LEFT JOIN instruments i ON i.silver_source_id = k.src AND i.instrument_external_id = k.iid
  LEFT JOIN exposure_situs_at($1) st ON st.silver_source_id = k.src AND st.account_external_id = k.acct
       AND st.position_key = k.position_key
 ORDER BY 1, 4, 2, 3`, func(rows *sql.Rows) error {
		var r reading
		var iid, security string
		if err := rows.Scan(&r.position[0], &r.position[1], &r.position[2], &iid, &security, &r.situs, &r.basis,
			&r.share, &r.currency, &r.seen); err != nil {
			return err
		}
		r.k = yield.InstrumentKey{Source: r.position[0], Instrument: in.Fold(r.position[0], r.position[1], iid)}
		in.Held[r.k] = append(in.Held[r.k], r.position[1])
		if security != "" && in.HeldSecurity[r.k] == "" {
			in.HeldSecurity[r.k] = security
		}
		readings, maybe = append(readings, r), maybe || r.basis == situsDeclared || r.basis == situsUnknown || r.basis == ""
		return nil
	}, in.AsOf); err != nil {
		return err
	}
	for k, accts := range in.Held {
		slices.Sort(accts)
		in.Held[k] = slices.Compact(accts)
	}
	if maybe && shared.receipts == nil {
		var err error
		if shared.receipts, err = receiptPositions(ctx, db); err != nil {
			return err
		}
	}
	receipts := shared.receipts
	type newest struct {
		seen               int64
		lag                yield.Lag
		market             string
		share              bool
		mixed, mixedMarket bool
	}
	picks := map[yield.InstrumentKey]*newest{}
	for _, r := range readings {
		receipt := r.basis == situsUnderlying || receipts[r.position]
		lag := yield.RecordLag(r.situs, r.currency, receipt, r.share)
		market := yield.SettlementMarket(r.situs, r.currency, receipt, r.share)
		switch p := picks[r.k]; {
		case p == nil || r.seen > p.seen:
			picks[r.k] = &newest{seen: r.seen, lag: lag, market: market, share: r.share}
		case r.seen == p.seen:
			p.mixed = p.mixed || lag != p.lag
			p.mixedMarket = p.mixedMarket || market != p.market
		}
	}
	for k, p := range picks {
		if !p.mixed && p.lag.Days > 0 {
			b.lag[k] = p.lag
		}
		if !p.mixedMarket && p.market != "" {
			b.market[k] = p.market
		}
		b.share[k] = p.share
	}
	return nil
}

// receiptPositions are the positions the situs rule reads as depositary
// receipts (situsFacts.receipt), by source, account and position key. A
// declared situs comes first in the rule, and a receipt whose share is
// unknown has basis unknown: underlying says a position is one, declared
// and unknown say nothing, and any other basis says it is none.
func receiptPositions(ctx context.Context, db querier) (map[[3]string]bool, error) {
	out := map[[3]string]bool{}
	return out, scanAll(ctx, db, `
SELECT f.src, f.acct, f.position_key
  FROM (`+situsHoldingsSQL+`) f
 WHERE f.receipt`, func(rows *sql.Rows) error {
		var k [3]string
		if err := rows.Scan(&k[0], &k[1], &k[2]); err != nil {
			return err
		}
		out[k] = true
		return nil
	}, receiptWording.String())
}

// readWithholding reads the withholding rates: per situs country and tax
// wrapper, and per account where an account's rate is not its
// wrapper's.
func readWithholding(ctx context.Context, db querier, in *yield.Inputs) error {
	if err := scanAll(ctx, db, `SELECT country, wrapper, rate_pct FROM yield_withholding`, func(rows *sql.Rows) error {
		var country, wrapper string
		var pct float64
		if err := rows.Scan(&country, &wrapper, &pct); err != nil {
			return err
		}
		if in.Withholding[country] == nil {
			in.Withholding[country] = map[string]float64{}
		}
		in.Withholding[country][wrapper] = pct
		return nil
	}); err != nil {
		return err
	}
	return scanAll(ctx, db, `SELECT country, silver_source_id, account_external_id, rate_pct
  FROM yield_withholding_accounts`, func(rows *sql.Rows) error {
		var country string
		var acct yield.AccountKey
		var pct float64
		if err := rows.Scan(&country, &acct.Source, &acct.Account, &pct); err != nil {
			return err
		}
		if in.AccountWithholding[acct] == nil {
			in.AccountWithholding[acct] = map[string]float64{}
		}
		in.AccountWithholding[acct][country] = pct
		return nil
	})
}

// readYieldFX reads the rate from each currency gold holds figures in to
// each currency of currencies, on the day of the instant asOf, into the
// valuation of each.
func readYieldFX(ctx context.Context, db querier, asOf int64, currencies []string, out []valuation) error {
	branches := make([]string, len(currencies))
	args := []any{asOf}
	for i, ccy := range currencies {
		args = append(args, ccy)
		branches[i] = fmt.Sprintf(`(SELECT %d AS v, from_ccy, rate FROM fx_rates_to($%d)
     WHERE day <= $1 // 86400 AND rate IS NOT NULL
   QUALIFY row_number() OVER (PARTITION BY from_ccy ORDER BY day DESC) = 1)`, i, len(args))
	}
	return scanAll(ctx, db, `
SELECT v, from_ccy, rate FROM (`+strings.Join(branches, "\n    UNION ALL ")+`)`, func(rows *sql.Rows) error {
		var v int
		var ccy string
		var rate float64
		if err := rows.Scan(&v, &ccy, &rate); err != nil {
			return err
		}
		out[v].fx[ccy] = rate
		return nil
	}, args...)
}
