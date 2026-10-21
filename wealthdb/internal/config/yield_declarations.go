package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The yield records of the declarations ledger (docs/EXPOSURE.md §9), one
// JSON object each under its kind's key:
//
//	{"silver_source_id":"bank-1","account":"savings-1","as_of":"2026-01-01","rate":{"pct":0.75}}
//	{"silver_source_id":"broker-1","instrument":"bond-1","rating":{"bucket":"AA","agency":"Moody's"}}
//	{"silver_source_id":"broker-1","instrument":"QSTK","dividend":{"amount":0.42,"frequency":4}}
//	{"silver_source_id":"broker-1","instrument":"bond-1","terms":{"frequency":2,"call_date":"2028-06-15"}}
//
// Each kind's keys are fixed; any other is refused, as a record's are.

// The keys each yield record takes.
var (
	rateKeys     = []string{"pct", "currency", "until"}
	ratingKeys   = []string{"bucket", "agency"}
	dividendKeys = []string{"amount", "frequency", "currency"}
	termsKeys    = []string{"coupon", "frequency", "maturity", "call_date", "fixed_until", "reset_date",
		"coupon_kind", "day_count", "duration", "yield"}
)

// maxDeclaredRate bounds a declared rate, coupon or yield, in percent
// either way, as the curves' rates are.
const maxDeclaredRate = 100

// parseYieldRecord reads the yield record of kind into d.
func parseYieldRecord(d *canonical.ExposureDeclaration, kind string, raw json.RawMessage) error {
	fields, err := jsonObject(raw)
	if err != nil {
		return err
	}
	keys := map[string][]string{canonical.DeclarationRate: rateKeys, canonical.DeclarationRating: ratingKeys,
		canonical.DeclarationDividend: dividendKeys, canonical.DeclarationTerms: termsKeys}[kind]
	values := map[string]json.RawMessage{}
	for _, f := range fields {
		if !slices.Contains(keys, f.key) {
			return fmt.Errorf("unknown key %q; a %s takes %s", f.key, kind, strings.Join(keys, ", "))
		}
		values[f.key] = f.value
	}
	r := yieldReader{values: values}
	switch kind {
	case canonical.DeclarationRate:
		v := &canonical.DeclaredRate{}
		r.rate("pct", &v.Pct, true)
		v.Currency = r.currency("currency")
		v.Until = r.date("until")
		d.Rate = v
	case canonical.DeclarationRating:
		v := &canonical.DeclaredRating{}
		if b := r.text("bucket", true); b != "" && r.err == nil {
			v.Bucket = canonical.RatingBucket(b)
			if !slices.Contains(canonical.RatingBuckets(), v.Bucket) {
				r.fail("bucket %q is not one of %s", b, joinNames(canonical.RatingBuckets()))
			}
		}
		v.Agency = r.text("agency", false)
		d.Rating = v
	case canonical.DeclarationDividend:
		v := &canonical.DeclaredDividend{}
		if a := r.number("amount", true); a != nil {
			if *a < 0 {
				r.fail("amount is a dividend per share, not below 0")
			}
			v.Amount = *a
		}
		if f := r.frequency(true, false); f != nil {
			v.Frequency = *f
		}
		v.Currency = r.currency("currency")
		d.Dividend = v
	default:
		if len(values) == 0 {
			return fmt.Errorf("states nothing; it takes %s", strings.Join(termsKeys, ", "))
		}
		v := &canonical.DeclaredTerms{}
		var c float64
		if r.rate("coupon", &c, false) {
			if c < 0 {
				r.fail("coupon is a rate a year, not below 0")
			}
			v.Coupon = &c
		}
		v.Frequency = r.frequency(false, true)
		v.Maturity, v.CallDate = r.date("maturity"), r.date("call_date")
		v.FixedUntil, v.ResetDate = r.date("fixed_until"), r.date("reset_date")
		if k := r.text("coupon_kind", false); k != "" {
			v.CouponKind = canonical.CouponKind(k)
			if !v.CouponKind.Valid() {
				r.fail("coupon_kind %q is not one of %s", k, joinNames(canonical.CouponKinds()))
			}
		}
		if dc := r.text("day_count", false); dc != "" {
			v.DayCount = canonical.DayCount(dc)
			if !v.DayCount.Valid() {
				r.fail("day_count %q is not one of %s", dc, joinNames(canonical.DayCounts()))
			}
		}
		if du := r.number("duration", false); du != nil {
			if *du < 0 || *du > float64(canonical.MaxTenorMonths/12) {
				r.fail("duration is in years, 0 to %d", canonical.MaxTenorMonths/12)
			}
			v.Duration = du
		}
		var y float64
		if r.rate("yield", &y, false) {
			v.Yield = &y
		}
		checkTerms(&r, v)
		d.Terms = v
	}
	return r.err
}

// checkTerms refuses terms that contradict one another: a coupon on
// paper that pays none (frequency 0, or kind zero), and a call or the end
// of a fixed period after the maturity.
func checkTerms(r *yieldReader, v *canonical.DeclaredTerms) {
	paysNone := (v.Frequency != nil && *v.Frequency == 0) || v.CouponKind == canonical.CouponZero
	switch {
	case paysNone && v.Coupon != nil && *v.Coupon > 0:
		r.fail("coupon %v on paper that pays none (frequency 0 or coupon_kind zero)", *v.Coupon)
	case v.Maturity != nil && v.CallDate != nil && v.CallDate.After(*v.Maturity):
		r.fail("call_date is after the maturity")
	case v.Maturity != nil && v.FixedUntil != nil && v.FixedUntil.After(*v.Maturity):
		r.fail("fixed_until is after the maturity")
	}
}

// yieldReader reads the fields of one yield record; its first error
// stays.
type yieldReader struct {
	values map[string]json.RawMessage
	err    error
}

func (r *yieldReader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(format, args...)
	}
}

// number reads a JSON number. A required one that is absent fails.
func (r *yieldReader) number(key string, required bool) *float64 {
	raw, ok := r.values[key]
	if !ok {
		if required {
			r.fail("%s is required", key)
		}
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		r.fail("%s: %v", key, err)
		return nil
	}
	n, isNumber := v.(json.Number)
	f, err := n.Float64()
	if !isNumber || err != nil || math.IsInf(f, 0) {
		r.fail("%s is a number, not %s", key, raw)
		return nil
	}
	return &f
}

// rate reads a rate in percent a year into dst, within ±maxDeclaredRate;
// false where the key is absent or bad.
func (r *yieldReader) rate(key string, dst *float64, required bool) bool {
	v := r.number(key, required)
	if v == nil {
		return false
	}
	if math.Abs(*v) >= maxDeclaredRate {
		r.fail("%s is in percent a year, within ±%d: %v reads as a number of another unit", key, maxDeclaredRate, *v)
		return false
	}
	*dst = *v
	return true
}

func (r *yieldReader) text(key string, required bool) string {
	raw, ok := r.values[key]
	if !ok {
		if required {
			r.fail("%s is required", key)
		}
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		r.fail("%s is a string", key)
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" {
		r.fail("%s is empty; leave it out", key)
	}
	return s
}

func (r *yieldReader) currency(key string) string {
	c := r.text(key, false)
	if c != "" && !IsLikelyISO4217(c) {
		r.fail("%s %q is not a 3-letter ISO 4217 code", key, c)
	}
	return c
}

func (r *yieldReader) date(key string) *time.Time {
	s := r.text(key, false)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		r.fail("%s %q is not a date (YYYY-MM-DD)", key, s)
		return nil
	}
	return &t
}

// frequency reads the coupon or dividend payments a year: one of
// canonical.CouponFrequencies, 0 (a discount instrument) only where zero
// is.
func (r *yieldReader) frequency(required, zero bool) *int {
	v := r.number("frequency", required)
	if v == nil {
		return nil
	}
	n := int(*v)
	if float64(n) != *v || !canonical.ValidCouponFrequency(n) || (n == 0 && !zero) {
		r.fail("frequency is the payments a year: %s", frequencyNames(zero))
		return nil
	}
	return &n
}

// frequencyNames lists the frequencies a record takes, 0 (a discount
// instrument) among them where zero is.
func frequencyNames(zero bool) string {
	var out []string
	for _, f := range canonical.CouponFrequencies() {
		if f != 0 || zero {
			out = append(out, fmt.Sprint(f))
		}
	}
	return strings.Join(out, ", ")
}
