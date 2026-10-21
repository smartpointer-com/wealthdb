package config

import (
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Each yield record reads into its type, beside or without the
// dimensions; a record of yield figures alone describes no exposure.
func TestDeclarationYieldRecords(t *testing.T) {
	t.Parallel()
	d, err := declarations(t, `{"silver_source_id":"manual","account":"savings-1","as_of":"2026-01-01",`+
		`"rate":{"pct":0.75,"currency":"CHF","until":"2026-12-31"}}`+"\n"+
		`{"silver_source_id":"manual","instrument":"bond-1","rating":{"bucket":"AA","agency":"Example Ratings"},`+
		`"terms":{"coupon":4.25,"frequency":2,"maturity":"2031-06-15","call_date":"2028-06-15","day_count":"30/360",`+
		`"coupon_kind":"fixed"}}`+"\n"+
		`{"silver_source_id":"manual","instrument":"QSTK","dividend":{"amount":0.42,"frequency":4,"currency":"USD"},`+
		`"country":{"US":100}}`+"\n"+
		`{"silver_source_id":"manual","instrument":"sleeve-1","as_of":"2026-09-30","terms":{"duration":6.1,"yield":4.4}}`+"\n"+
		`{"silver_source_id":"manual","instrument":"mortgage-1","terms":{"coupon":1.85,"fixed_until":"2029-03-31"}}`+"\n"+
		`{"silver_source_id":"manual","instrument":"bond-2","rating":{"bucket":"unrated"}}`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 6 {
		t.Fatalf("records = %+v", d)
	}
	r := d[0]
	if r.AccountExternalID != "savings-1" || r.InstrumentExternalID != "" || r.Rate == nil || r.Rate.Pct != 0.75 ||
		r.Rate.Currency != "CHF" || r.Rate.Until == nil || !r.Rate.Until.Equal(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) ||
		r.StatesExposure() {
		t.Errorf("the rate: %+v %+v", r, r.Rate)
	}
	b := d[1]
	if b.Rating == nil || b.Rating.Bucket != canonical.RatingAA || b.Rating.Agency != "Example Ratings" || b.Terms == nil ||
		*b.Terms.Coupon != 4.25 || *b.Terms.Frequency != 2 || b.Terms.Maturity.Format(time.DateOnly) != "2031-06-15" ||
		b.Terms.CallDate.Format(time.DateOnly) != "2028-06-15" || b.Terms.DayCount != canonical.DayCount30360 ||
		b.Terms.CouponKind != canonical.CouponFixed || b.StatesExposure() {
		t.Errorf("the bond: %+v %+v %+v", b, b.Rating, b.Terms)
	}
	if s := d[2]; s.Dividend == nil || s.Dividend.Amount != 0.42 || s.Dividend.Frequency != 4 ||
		s.Dividend.Currency != "USD" || !s.StatesExposure() || len(s.Leaves) != 1 {
		t.Errorf("the dividend beside a country: %+v %+v", s, s.Dividend)
	}
	if f := d[3].Terms; f == nil || *f.Duration != 6.1 || *f.Yield != 4.4 || f.Coupon != nil || f.Frequency != nil {
		t.Errorf("a fund's factsheet figures: %+v", f)
	}
	if m := d[4].Terms; m == nil || *m.Coupon != 1.85 || m.FixedUntil.Format(time.DateOnly) != "2029-03-31" {
		t.Errorf("a mortgage's fixed period: %+v", m)
	}
	if u := d[5].Rating; u == nil || u.Bucket != canonical.RatingUnrated {
		t.Errorf("an unrated bond: %+v", u)
	}
	// An account takes a rate per currency on one day, and a dividend of
	// 0 is one.
	if _, err := declarations(t, `{"silver_source_id":"manual","account":"a","rate":{"pct":1,"currency":"CHF"}}`+"\n"+
		`{"silver_source_id":"manual","account":"a","rate":{"pct":2,"currency":"USD"}}`+"\n"+
		`{"silver_source_id":"manual","instrument":"x","dividend":{"amount":0,"frequency":1}}`+"\n"); err != nil {
		t.Errorf("a rate per currency, a dividend of 0: %v", err)
	}
	// Each kind takes one record per holding and date, so a rating and the
	// terms of one day sit side by side.
	if _, err := declarations(t, `{"silver_source_id":"manual","instrument":"x","rating":{"bucket":"A"}}`+"\n"+
		`{"silver_source_id":"manual","instrument":"x","terms":{"frequency":1}}`+"\n"+
		`{"silver_source_id":"manual","instrument":"x","country":{"CH":100}}`+"\n"); err != nil {
		t.Errorf("a rating, terms and a country of one holding: %v", err)
	}
}

func TestDeclarationYieldRejects(t *testing.T) {
	t.Parallel()
	hold := func(body string) string { return `{"silver_source_id":"manual","instrument":"x",` + body + "}\n" }
	acct := func(body string) string { return `{"silver_source_id":"manual","account":"a",` + body + "}\n" }
	for body, want := range map[string]string{
		hold(`"rate":{"pct":1}`):                                          "a rate names an account and states nothing else",
		acct(`"rate":{"pct":1},"rating":{"bucket":"A"}`):                  "a rate names an account and states nothing else",
		acct(`"country":{"CH":100}`):                                      "account belongs to a record that states a rate",
		acct(`"rate":{"pct":1,"rat":2}`):                                  `rate: unknown key "rat"; a rate takes pct, currency, until`,
		acct(`"rate":{}`):                                                 "rate: pct is required",
		acct(`"rate":{"pct":"1%"}`):                                       `rate: pct is a number, not "1%"`,
		acct(`"rate":{"pct":125}`):                                        "rate: pct is in percent a year, within ±100",
		acct(`"rate":{"pct":1,"currency":"chf"}`):                         `rate: currency "chf" is not a 3-letter ISO 4217 code`,
		acct(`"rate":{"pct":1,"until":"31.12.2026"}`):                     `rate: until "31.12.2026" is not a date`,
		acct(`"rate":{"pct":1}`) + acct(`"rate":{"pct":2}`):               "line 2: manual account a is given a rate without a date on line 1 already",
		hold(`"rating":{"bucket":"Aa2"}`):                                 `rating: bucket "Aa2" is not one of AAA, AA, A, BBB, BB, B, unrated`,
		hold(`"rating":{"agency":"Example"}`):                             "rating: bucket is required",
		hold(`"rating":"AA"`):                                             "rating: want a JSON object",
		hold(`"dividend":{"amount":0.5}`):                                 "dividend: frequency is required",
		hold(`"dividend":{"amount":0.5,"frequency":3}`):                   "dividend: frequency is the payments a year: 1, 2, 4, 12",
		hold(`"dividend":{"amount":0.5,"frequency":0}`):                   "dividend: frequency is the payments a year: 1, 2, 4, 12",
		hold(`"dividend":{"amount":-1,"frequency":4}`):                    "dividend: amount is a dividend per share, not below 0",
		hold(`"terms":{"frequency":3}`):                                   "terms: frequency is the payments a year: 0, 1, 2, 4, 12",
		hold(`"terms":{}`):                                                "terms: states nothing",
		hold(`"terms":{"coupon":425}`):                                    "terms: coupon is in percent a year",
		hold(`"terms":{"coupon":-1}`):                                     "terms: coupon is a rate a year, not below 0",
		hold(`"terms":{"coupon_kind":"step"}`):                            `terms: coupon_kind "step" is not one of fixed, floating, inflation_linked, zero`,
		hold(`"terms":{"day_count":"ACT/364"}`):                           `terms: day_count "ACT/364" is not one of`,
		hold(`"terms":{"duration":80}`):                                   "terms: duration is in years, 0 to 50",
		hold(`"terms":{"maturity":"2031"}`):                               `terms: maturity "2031" is not a date`,
		hold(`"terms":{"call":"2028-06-15"}`):                             `terms: unknown key "call"`,
		hold(`"terms":{"frequency":1}`) + hold(`"terms":{"frequency":2}`): "line 2: manual x is given terms without a date on line 1 already",
		acct(`"rate":{"pct":1,"currency":"CHF"}`) + acct(`"rate":{"pct":2,"currency":"CHF"}`): "line 2: manual account a is given a rate in CHF without a date on line 1 already",
		hold(`"terms":{"coupon":4,"frequency":0}`):                                            "terms: coupon 4 on paper that pays none",
		hold(`"terms":{"coupon":4,"coupon_kind":"zero"}`):                                     "terms: coupon 4 on paper that pays none",
		hold(`"terms":{"maturity":"2030-01-01","call_date":"2031-01-01"}`):                    "terms: call_date is after the maturity",
		hold(`"terms":{"maturity":"2030-01-01","fixed_until":"2031-01-01"}`):                  "terms: fixed_until is after the maturity",
		hold(`"rating":{"bucket":"AA","agency":""}`):                                          "rating: agency is empty; leave it out",
		`{"silver_source_id":"manual","rate":{"pct":1}}` + "\n":                               "a rate names an account",
	} {
		if _, err := declarations(t, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", body, err, want)
		}
	}
}
