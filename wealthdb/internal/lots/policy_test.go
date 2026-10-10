package lots

import "testing"

func TestMethodPrecedence(t *testing.T) {
	lifo, hifo, lofo, avg := LIFO, HIFO, LOFO, Average
	c := Config{
		Method:     FIFO,
		Sources:    map[string]SourceConfig{"s": {Method: &hifo}},
		Portfolios: map[string]map[string]Method{"s": {"p": lofo}},
		Accounts:   map[string]map[string]Method{"s": {"a": lifo}},
	}
	cases := []struct {
		src, port, acct string
		own             *Method
		want            Method
	}{
		{"s", "p", "a", nil, LIFO},
		{"s", "p", "b", nil, LOFO},
		{"s", "q", "b", nil, HIFO},
		{"s", "q", "b", &avg, HIFO},
		{"t", "q", "b", &avg, Average},
		{"t", "q", "b", nil, FIFO},
		// A pooled key passes no account.
		{"s", "p", "", nil, LOFO},
	}
	for _, x := range cases {
		if got := c.MethodFor(x.src, x.port, x.acct, x.own); got != x.want {
			t.Errorf("%+v: %s, want %s", x, got, x.want)
		}
	}
}

func TestParseRoundTrips(t *testing.T) {
	for _, m := range []Method{FIFO, LIFO, HIFO, LOFO, Average} {
		if got, err := ParseMethod(m.String()); err != nil || got != m {
			t.Errorf("%s: %v %v", m, got, err)
		}
	}
	for _, m := range []Mode{Off, Shadow, Fill} {
		if got, err := ParseMode(m.String()); err != nil || got != m {
			t.Errorf("%s: %v %v", m, got, err)
		}
	}
	for _, g := range []Grain{GrainAccount, GrainPortfolio} {
		if got, err := ParseGrain(g.String()); err != nil || got != g {
			t.Errorf("%s: %v %v", g, got, err)
		}
	}
	if _, err := ParseMethod("random"); err == nil {
		t.Error("an unknown method is refused")
	}
}

func TestConfigOverridesAPolicy(t *testing.T) {
	if _, ok := PolicyFor("lots-test-kind"); !ok {
		RegisterPolicy("lots-test-kind", TradingPolicy())
	}
	shadow, pooled := Shadow, GrainPortfolio
	c := Config{Sources: map[string]SourceConfig{"x": {Mode: &shadow, Grain: &pooled}}}
	if p := c.PolicyFor("x", "lots-test-kind"); p.Mode != Shadow || p.Grain != GrainPortfolio {
		t.Errorf("overridden %+v", p)
	}
	if p := c.PolicyFor("y", "no-such-kind"); p.Mode != Off || p.Grain != GrainAccount {
		t.Errorf("an unregistered kind is off, with the trading defaults: %+v", p)
	}
}

func TestDefaultClassify(t *testing.T) {
	for _, c := range []struct {
		t    Txn
		want Action
	}{
		{Txn{Kind: "buy"}, Buy},
		{Txn{Kind: "sell"}, Sell},
		{Txn{Kind: "journal", Quantity: 3}, In},
		{Txn{Kind: "journal", Quantity: -3}, Out},
		{Txn{Kind: "journal"}, Ignore},
		{Txn{Kind: "dividend", Quantity: 0}, Ignore},
		{Txn{Kind: "staking", Quantity: 1}, Income},
		{Txn{Kind: "fee", Quantity: -1}, Exchange},
		{Txn{Kind: "fee"}, Ignore},
		{Txn{Kind: "corporate_action"}, Corporate},
		{Txn{Kind: "deposit"}, Ignore},
	} {
		if got := DefaultClassify(c.t).Action; got != c.want {
			t.Errorf("%+v: %v, want %v", c.t, got, c.want)
		}
	}
}
