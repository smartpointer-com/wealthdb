package canonical

// Size is a stock's size bucket: large, mid or small.
type Size string

const (
	SizeLarge Size = "large"
	SizeMid   Size = "mid"
	SizeSmall Size = "small"
)

// The market caps, in US dollars, from which a stock is large or mid.
const (
	LargeCapUSD = 10e9
	MidCapUSD   = 2e9
)

// A market's stocks, taken by falling market cap, are large until they
// hold LargeCumPct of the market's value, then mid until MidCumPct; the
// rest are small. These are the size segments of the broad index
// families. A fund that covers only part of its markets (a large and mid
// cap fund covers 85%) holds only their larger stocks.
const (
	LargeCumPct = 70.0
	MidCumPct   = 85.0
)
