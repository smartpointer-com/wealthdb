package yield

// kindSpec is what a kind of holding does (Kind). measure sets its
// figures and project adds its income to the projection; gap is the
// yield its gap compares with its benchmark (gapYield), stated the yield
// the projection reads where its distributions indicate none
// (statedYield), nil for none; heldIn is what holds it in the curve view
// (HeldIn); missing names what it lacks that a ledger record could state
// (missingOf), nil for nothing; fund and cash say the report lists it
// among the funds (Book.Funds) and among the cash (Book.Cash).
type kindSpec struct {
	measure    func(in *Inputs, h Holding, t Terms, m *working)
	project    func(pj *projector, h Holding, t Terms, m *working)
	gap        func(m *working) *float64
	stated     func(m *working, f *Fund) *float64
	heldIn     string
	missing    func(p Position) []string
	fund, cash bool
}

// kinds are the kinds' specs, by kind. init fills it: the measures and
// the projection read it themselves.
var kinds map[Kind]kindSpec

func init() {
	deposit := func(in *Inputs, h Holding, _ Terms, m *working) { measureDeposit(in, h, m) }
	kinds = map[Kind]kindSpec{
		KindDeposit: {measure: deposit, project: (*projector).projectDeposit, gap: rateGap, heldIn: HeldInCash,
			missing: missingRateOf, cash: true},
		KindTermDeposit: {measure: deposit, project: (*projector).projectTermDeposit, gap: rateGap,
			heldIn: HeldInCash, missing: missingRateOf, cash: true},
		KindMoneyMarket: {measure: measureFund, project: (*projector).projectMoneyMarket, gap: rateGap,
			heldIn: HeldInFunds, missing: missingFiguresOf, fund: true, cash: true},
		KindBond: {measure: measureBond, project: unlessDefault((*projector).projectBond), gap: rateGap,
			heldIn: HeldInBonds, missing: missingTermsOf(true)},
		KindBill: {measure: measureBond, project: unlessDefault((*projector).projectBill), gap: rateGap,
			heldIn: HeldInBonds, missing: missingTermsOf(true)},
		KindFloating: {measure: measureBond, project: unlessDefault((*projector).projectFloating), gap: rateGap,
			heldIn: HeldInBonds, missing: missingTermsOf(true)},
		KindNote: {measure: measureBond, project: unlessDefault((*projector).projectNote), gap: rateGap,
			heldIn: HeldInBonds, missing: missingTermsOf(false)},
		KindBondFund: {measure: measureFund, project: (*projector).projectBondFund, gap: rateGap,
			stated: statedBondFund, heldIn: HeldInFunds, missing: missingFiguresOf, fund: true},
		KindEquityFund: {measure: func(in *Inputs, h Holding, _ Terms, m *working) { measureEquityFund(in, h, m) },
			project: (*projector).projectEquityFund, gap: noGap, stated: statedEquityFund, heldIn: HeldInBonds,
			missing: missingDividendOf, fund: true},
		KindMultiAsset: {measure: measureMultiAsset, project: (*projector).projectEquityFund, gap: impliedGap,
			stated: statedEquityFund, heldIn: HeldInFunds, missing: missingDividendOf, fund: true},
		KindStock: {measure: func(in *Inputs, h Holding, _ Terms, m *working) { measureStock(in, h, m) },
			project: (*projector).projectStock, gap: noGap, heldIn: HeldInBonds, missing: missingDividendOf},
		KindLiability: {measure: measureLiability, gap: rateGap, heldIn: HeldInLiabilities,
			missing: missingTermsOf(false)},
		KindOther: {measure: measureOther, project: (*projector).projectOther, gap: rateGap, heldIn: HeldInBonds},
	}
}
