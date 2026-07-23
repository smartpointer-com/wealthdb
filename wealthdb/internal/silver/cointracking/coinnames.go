package cointracking

// coinNames maps cointracking's coin ticker to the
// full coin name used to populate `instruments.name`. Covers the
// ~50 most widely held tickers — large enough that essentially
// every position emitted into gold has a human-readable label.
//
// Unknown tickers fall back to the ticker itself; gold's instruments
// view will still display something sensible.
//
// Fiat tickers (USD, EUR, CHF, …) are included so cash balances
// held inside crypto wallets get a proper display label too;
// asset_class for those rows stays `crypto` because the gold
// layer's account_kind=crypto already segregates them.
var coinNames = map[string]string{
	// Majors.
	"BTC":   "Bitcoin",
	"ETH":   "Ethereum",
	"BNB":   "BNB",
	"XRP":   "XRP",
	"ADA":   "Cardano",
	"SOL":   "Solana",
	"DOT":   "Polkadot",
	"TRX":   "TRON",
	"LTC":   "Litecoin",
	"BCH":   "Bitcoin Cash",
	"XLM":   "Stellar",
	"XMR":   "Monero",
	"ETC":   "Ethereum Classic",
	"DOGE":  "Dogecoin",
	"AVAX":  "Avalanche",
	"MATIC": "Polygon",
	"ATOM":  "Cosmos",
	"NEAR":  "NEAR Protocol",
	"ALGO":  "Algorand",
	"FTM":   "Fantom",

	// DeFi.
	"UNI":  "Uniswap",
	"UNI2": "Uniswap V2",
	"LINK": "Chainlink",
	"AAVE": "Aave",
	"MKR":  "Maker",
	"CRV":  "Curve DAO Token",
	"COMP": "Compound",
	"SNX":  "Synthetix",
	"BAT":  "Basic Attention Token",
	"ZRX":  "0x Protocol",

	// Stablecoins.
	"USDT":  "Tether",
	"USDC":  "USD Coin",
	"DAI":   "Dai",
	"BUSD":  "Binance USD",
	"TUSD":  "TrueUSD",
	"USDP":  "Pax Dollar",
	"FDUSD": "First Digital USD",
	"PYUSD": "PayPal USD",

	// Long-tail names the upstream list lacks.
	"BCHSV": "Bitcoin SV",
	"ETHW":  "EthereumPoW",

	// Older / smaller caps.
	"DASH":  "Dash",
	"NEO":   "Neo",
	"EOS":   "EOS",
	"VET":   "VeChain",
	"FET":   "Fetch.ai",
	"FLR":   "Flare",
	"GAS":   "Gas",
	"IOT":   "IOTA",
	"MIOTA": "IOTA",
	"NANO":  "Nano",

	// Fiat displayed inside crypto wallets — full English names so
	// the instruments table reads naturally.
	"USD": "US Dollar",
	"EUR": "Euro",
	"CHF": "Swiss Franc",
	"GBP": "Pound Sterling",
	"JPY": "Japanese Yen",
	"CAD": "Canadian Dollar",
	"AUD": "Australian Dollar",
}

// coinNameFor returns the full name for a ticker, falling back to
// the ticker itself when the map has no entry.
func coinNameFor(ticker string) string {
	if name, ok := coinNames[ticker]; ok {
		return name
	}
	return ticker
}
