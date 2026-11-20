package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The private-equity fund a cash row names.
//
// A capital call settles as a buy of the fund's units under the bank's own
// booking type (bookings.go), and its valor names the fund. Other rows
// that move a fund's cash state no instrument: a distribution or a return
// of capital credited as a plain credit, a call paid by payment order, a
// statement line whose valor the index does not know. Only the text names
// the fund, by its name, its ISIN or its valor. Without the instrument
// the fund's paid-out side is missing wherever gold reads a holding's
// legs, and a call paid as a withdrawal reads as capital leaving the
// relationship.
//
// So a row that no other road links to an instrument takes a fund by one
// of two roads. Where a corporate action on the fund confirms the cash
// the row moved, the row is that action's (pairConfirmations), and the
// action says what it is: a credit is a distribution, a debit a call.
// Else it takes the fund its text names by its ISIN, its valor or one of
// its names, where the text names exactly one of the private-equity funds
// of its account's portfolios (privateFunds.named), and books as what its
// kind says (fundKind). The amount, the day and the id stay the row's. A
// row that names none, or several, stays as it is, as does a move between
// the relationship's own accounts: its other leg is the record that pays
// the fund.
//
// The valor index (valor_index.go) reads only the valor a statement
// parser promotes, and answers for the whole relationship, so it drops a
// valor two securities share. A fund's valor in a cash row's free text,
// or a promoted one the index dropped, still names the fund here: the
// search runs only over the funds of the row's own portfolios, as a name
// does.

// privateFunds is what a row is read against: the portfolios each cash
// account belongs to, the private-equity funds each portfolio holds, and
// every name the silvers give each fund.
type privateFunds struct {
	// portfolios maps a cash account onto its portfolios.
	portfolios accountPortfolios
	// held maps a portfolio onto the ISINs of the funds it holds.
	held map[string]map[string]bool
	// withFunds marks the cash accounts one of whose portfolios holds a
	// fund: no other account's text is read.
	withFunds map[string]bool
	// names maps a fund's ISIN onto its names, folded (fundText).
	names map[string][]string
	// valors maps a fund's ISIN onto the valors the silvers state for it
	// (normalizeValor).
	valors map[string][]string
	// funds are the ISINs the adapter classifies as private-equity funds.
	funds map[string]bool
	// paired maps each cash line a fund's corporate action pairs with
	// (pairConfirmations) onto that action: its fund and its direction.
	// A web row is keyed by the id it reaches gold under, a feed line by
	// its event id.
	paired map[string]fundCash
}

// fundCash is the cash a fund's corporate action moved: the fund, and
// whether it credited the account.
type fundCash struct {
	isin   string
	credit bool
}

// pairedLink gives a line a fund's corporate action pairs with the fund,
// and the kind the action says it is: a credit is a distribution, a debit
// a call, a buy of the units. false, and the line as it was, where no
// action pairs it, or where its kind is none a fund's cash books under.
func (f *privateFunds) pairedLink(id string, kind canonical.TxKind) (isin string, _ canonical.TxKind, ok bool) {
	if f == nil {
		return "", kind, false
	}
	c, paired := f.paired[id]
	if _, cash := fundKind(kind); !paired || !cash {
		return "", kind, false
	}
	if c.credit {
		return c.isin, canonical.TxKindDistribution, true
	}
	return c.isin, canonical.TxKindBuy, true
}

// fundKind is the kind a row about a private-equity fund books as. A
// trade keeps its own. A withdrawal pays the fund a call, so it is a
// buy of its units, as the bank's own call booking is. A deposit is cash
// the fund paid out: a distribution, a return of capital among them.
// Any other kind says what the row is without the fund: false.
func fundKind(kind canonical.TxKind) (canonical.TxKind, bool) {
	switch kind {
	case canonical.TxKindBuy, canonical.TxKindSell:
		return kind, true
	case canonical.TxKindWithdrawal:
		return canonical.TxKindBuy, true
	case canonical.TxKindDeposit:
		return canonical.TxKindDistribution, true
	}
	return "", false
}

// link gives the row the fund its text names on its account, and the
// kind it then books as; false, and the row as it was, where it names
// none or several, or where its kind is none a fund's cash books under.
func (f *privateFunds) link(account string, kind canonical.TxKind, text ...string) (isin string, _ canonical.TxKind, ok bool) {
	k, ok := fundKind(kind)
	if !ok || f == nil || !f.withFunds[account] {
		return "", kind, false
	}
	if isin, ok = f.named(account, strings.Join(text, " ")); !ok {
		return "", kind, false
	}
	return isin, k, true
}

// named is the one fund of the account's portfolios the text names: by
// its ISIN, its valor, or one of its names whole. A fund the text names
// by name alone, and only inside the longer name of another fund it
// names, is not named: "<X> PARTNERS" inside "<X> PARTNERS II" names
// the second. false for none or several.
func (f *privateFunds) named(account, text string) (string, bool) {
	if f == nil || !f.withFunds[account] {
		return "", false
	}
	folded := fundText(text)
	numbers := valorTokens(folded)
	var hits []fundHit
	for _, portfolio := range f.portfolios[account] {
		for isin := range f.held[portfolio] {
			if slices.ContainsFunc(hits, func(h fundHit) bool { return h.isin == isin }) {
				continue
			}
			if h, ok := f.textNames(folded, numbers, isin); ok {
				hits = append(hits, h)
			}
		}
	}
	var hit string
	for _, h := range hits {
		if !h.byID {
			rest := folded
			for _, o := range hits {
				if o.isin != h.isin && len(o.name) > len(h.name) && strings.Contains(o.name, h.name) {
					rest = strings.ReplaceAll(rest, o.name, " ")
				}
			}
			if _, ok := f.textNames(rest, nil, h.isin); !ok {
				continue
			}
		}
		if hit != "" {
			return "", false
		}
		hit = h.isin
	}
	return hit, hit != ""
}

// fundHit is how a text names a fund: whether by its ISIN or a valor,
// and the longest of its names the text holds, folded ("" for none).
type fundHit struct {
	isin, name string
	byID       bool
}

// textNames is how the folded text, whose valor-shaped numbers are
// numbers (valorTokens), names the fund; false where it does not.
func (f *privateFunds) textNames(folded string, numbers map[string]bool, isin string) (fundHit, bool) {
	h := fundHit{isin: isin, byID: strings.Contains(folded, " "+isin+" ")}
	for _, v := range f.valors[isin] {
		h.byID = h.byID || numbers[v]
	}
	for _, name := range f.names[isin] {
		if len(name) > len(h.name) && strings.Contains(folded, name) {
			h.name = name
		}
	}
	return h, h.byID || h.name != ""
}

// fundText folds a text for the name test: letters upper-cased, digits
// kept, every other run of characters one space, and a space at each
// end, so a name matches whole words only.
func fundText(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte(' ')
	space := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToUpper(r))
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	if !space {
		b.WriteByte(' ')
	}
	return b.String()
}

// minValorLen is the fewest digits a number in a text must have to be
// read as a valor: a shorter one is a count, a day or a small figure far
// more often than a security.
const minValorLen = 5

// valorTokens are the whole numbers of a folded text that can be a valor,
// leading zeros dropped (normalizeValor).
func valorTokens(folded string) map[string]bool {
	var out map[string]bool
	for _, tok := range strings.Fields(folded) {
		if v := normalizeValor(tok); len(v) >= minValorLen {
			if out == nil {
				out = map[string]bool{}
			}
			out[v] = true
		}
	}
	return out
}

// addValor records a valor of the ISIN, once.
func (f *privateFunds) addValor(isin, valor string) {
	v := normalizeValor(valor)
	if len(v) < minValorLen {
		return
	}
	for _, have := range f.valors[isin] {
		if have == v {
			return
		}
	}
	f.valors[isin] = append(f.valors[isin], v)
}

// fundName is a name, folded, that the name test can use: one of two
// words or more. A single word names too little to tell a fund by.
func fundName(name string) (string, bool) {
	folded := fundText(name)
	return folded, len(strings.Fields(folded)) >= 2
}

// buildPrivateFunds reads the funds both silvers state. A fund is an
// instrument the adapter classifies (private_equity, fund), as it emits
// it: by the feed's pair where the feed describes the ISIN, else by a
// printed description. It is held by each portfolio a position in it
// sits in: the feed's holdings, by their custody account's portfolio,
// and the printed statements of assets. A cash account belongs to the
// portfolios places names (cashAccountPortfolios), as for a dividend's
// ticker (dividends.go). A nil or absent side contributes nothing.
func buildPrivateFunds(ctx context.Context, psn *psnReader, web *webReader, places accountPortfolios) (*privateFunds, error) {
	f := &privateFunds{
		portfolios: places,
		held:       map[string]map[string]bool{},
		withFunds:  map[string]bool{},
		names:      map[string][]string{},
		valors:     map[string][]string{},
		funds:      map[string]bool{},
		paired:     map[string]fundCash{},
	}
	feedPair, err := psn.fundNames(ctx, f)
	if err != nil {
		return nil, err
	}
	// webFund marks the ISINs the feed does not describe that a printed
	// description classifies as a fund.
	webFund := map[string]bool{}
	held := map[string]map[string]bool{}
	hold := func(portfolio, isin string) {
		if portfolio == "" || isin == "" {
			return
		}
		if held[portfolio] == nil {
			held[portfolio] = map[string]bool{}
		}
		held[portfolio][isin] = true
	}
	printed, err := web.printedPositions(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range printed {
		if !p.isin.Valid {
			continue
		}
		isin, description := p.isin.String, p.description.String
		hold(p.portfolio.String, isin)
		f.addName(isin, description)
		f.addValor(isin, p.valor.String)
		f.addValor(isin, valorFromSwissISIN(isin))
		if _, ok := feedPair[isin]; !ok {
			if ac, veh, _ := taxonomyPairForWebDescription(description); isPrivateEquityFund(ac, veh) {
				webFund[isin] = true
			}
		}
	}
	roster, err := psn.roster(ctx)
	if err != nil {
		return nil, err
	}
	ofSafekeeping := roster.portfolioOfSafekeeping()
	pairs, err := psn.holdingPairs(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range pairs {
		hold(ofSafekeeping[h.safekeeping], h.isin)
	}
	for isin, pair := range feedPair {
		if isPrivateEquityFund(pair.AssetClass, pair.Vehicle) {
			f.funds[isin] = true
		}
	}
	for isin := range webFund {
		f.funds[isin] = true
	}
	for portfolio, isins := range held {
		for isin := range isins {
			if f.funds[isin] {
				if f.held[portfolio] == nil {
					f.held[portfolio] = map[string]bool{}
				}
				f.held[portfolio][isin] = true
			}
		}
	}
	for account, portfolios := range f.portfolios {
		for _, portfolio := range portfolios {
			if len(f.held[portfolio]) > 0 {
				f.withFunds[account] = true
			}
		}
	}
	return f, nil
}

// isPrivateEquityFund reports whether the pair is a private-equity fund's.
func isPrivateEquityFund(ac canonical.AssetClass, veh canonical.Vehicle) bool {
	return ac == canonical.AssetClassPrivateEquity && veh == canonical.VehicleFund
}

// addName records a name of the ISIN, once, where the name test can use
// it (fundName).
func (f *privateFunds) addName(isin, name string) {
	folded, ok := fundName(name)
	if !ok {
		return
	}
	for _, n := range f.names[isin] {
		if n == folded {
			return
		}
	}
	f.names[isin] = append(f.names[isin], folded)
}

// fundNames reads the feed's instrument dimension: each ISIN's pair, as
// its latest row states it (appendInstruments), and every name and valor
// a row gives it, a Swiss ISIN's own valor among them (valorFromSwissISIN).
func (r *psnReader) fundNames(ctx context.Context, f *privateFunds) (map[string]taxPair, error) {
	pairs := map[string]taxPair{}
	if r == nil || r.db == nil {
		return pairs, nil
	}
	// One row per distinct payload, the latest last: the names and
	// valors are a union, and the pair the latest payload's.
	err := silver.ScanRows(ctx, r.db, "ubs-psn fund names", `
SELECT isin, payload FROM instruments GROUP BY isin, payload ORDER BY max(snapshot_at)`, func(rows *sql.Rows) error {
		var isin, payload string
		if err := rows.Scan(&isin, &payload); err != nil {
			return fmt.Errorf("ubs-psn fund names scan: %w", err)
		}
		var p struct {
			instrumentPayload
			InstrIdtfr struct {
				Valor string `json:"Valor"`
			} `json:"InstrIdtfr"`
		}
		_ = json.Unmarshal([]byte(payload), &p)
		ac, veh := taxonomyPairForInstrument(p.InstrCtgyCFI, p.UacAsstClsCd, p.InstrNm.Best())
		pairs[isin] = taxPair{AssetClass: ac, Vehicle: veh}
		f.addName(isin, p.InstrNm.LngNmEnglish)
		f.addName(isin, p.InstrNm.ShrtNmEnglish)
		f.addValor(isin, p.InstrIdtfr.Valor)
		f.addValor(isin, valorFromSwissISIN(isin))
		return nil
	})
	return pairs, err
}

// pairConfirmations pairs each corporate action on a fund that moves
// cash with the cash line that books it, as a dividend line pairs with
// its confirmation (buildDividendConfirmations): the cash account, the
// payment day, the amount posted, signed by the action's credit or debit
// mark, and its currency. An action that stands counts; a cancellation,
// a reversal and an action one of them withdraws do not. The pair holds
// only where one line answers the key and the actions that answer it
// name one fund (firstAgreeing). A line is a feed line or a web row the
// reconcile table leaves standing (webStands); a nil side has none.
func (f *privateFunds) pairConfirmations(ctx context.Context, psn *psnReader, web *webReader, rc *reconciliation, webStands func(*webRow) bool) error {
	if psn == nil || psn.db == nil {
		return nil
	}
	roster, err := psn.roster(ctx)
	if err != nil {
		return err
	}
	ibans := roster.cashAccountIBANs()
	confirmed, err := psn.corporateActions(ctx)
	if err != nil {
		return err
	}
	actions := map[bookingKey][]cashConfirmation{}
	for _, row := range confirmed.all {
		m, isin := row.m, strings.TrimSpace(row.f.ISIN)
		if !f.funds[isin] || !m.payingCash || !m.hasPosted || m.move.cashAccount == "" ||
			!isNewConfirmation(m.function) || confirmed.gone[row.eventID] != "" {
			continue
		}
		posted := m.move.posted.Abs()
		if !m.move.credit {
			posted = posted.Neg()
		}
		day := m.move.paymentDay
		if day == 0 {
			day = row.at
		}
		if k, ok := newBookingKey(cashIBAN(ibans, m.move.cashAccount), day, &posted, m.move.postedCcy); ok {
			actions[k] = append(actions[k], cashConfirmation{eventID: row.eventID, isin: isin, move: m.move})
		}
	}
	if len(actions) == 0 {
		return nil
	}
	lines := map[bookingKey][]string{}
	if err := psn.eachCashMovement(ctx, func(row *feedRow) error {
		tx, _, ok := row.project()
		if !ok || rc != nil && rc.lostTo(recFeed, row.eventID) != "" {
			return nil
		}
		if k, ok := txBookingKey(tx); ok && actions[k] != nil {
			lines[k] = append(lines[k], row.eventID)
		}
		return nil
	}); err != nil {
		return err
	}
	if web != nil && web.db != nil {
		if err := web.eachWebRow(ctx, func(row *webRow) error {
			if row.signed == nil || !webStands(row) {
				return nil
			}
			if k, ok := newBookingKey(row.account, row.valueDate, row.signed, row.currency); ok && actions[k] != nil {
				lines[k] = append(lines[k], row.emittedID())
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for k, ids := range lines {
		c, ok := oneSecurity(actions[k])
		if ok && len(ids) == 1 {
			f.paired[ids[0]] = fundCash{isin: c.isin, credit: c.move.credit}
		}
	}
	return nil
}
