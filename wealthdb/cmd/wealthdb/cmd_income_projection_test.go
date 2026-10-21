package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runIncome(t *testing.T, cfg string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(append([]string{"-c", cfg, "income"}, args...), nil, &out, &errb)
	return out.String(), errb.String(), code
}

// The forward views read today's holdings of the yield fixture: by
// default over the projection's horizon after the current month, else
// over a window FROM [TO]; a quarterly period is the calendar's.
func TestIncomeForwardWindows(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	out, errb, code := runIncome(t, cfg, "projection", "-f", "csv")
	rows := csvRows(t, out)
	if code != 0 || len(rows) != 14 || rows[0]["period"] != next.Format("2006-01") ||
		rows[12]["period"] != "total" || rows[13]["period"] != "monthly_average" {
		t.Fatalf("default window %d %s\n%s", code, errb, out)
	}
	year := strconv.Itoa(now.Year() + 1)
	out, errb, code = runIncome(t, cfg, "projection", year, "--period", "quarterly", "-f", "csv")
	rows = csvRows(t, out)
	if code != 0 || len(rows) != 6 || rows[0]["period"] != year+"-Q1" || rows[3]["period"] != year+"-Q4" ||
		rows[0]["status"] != "expected" {
		t.Errorf("next year by quarter %d %s\n%s", code, errb, out)
	}
	out, _, code = runIncome(t, cfg, "projection", "--period", "quarterly", "-f", "csv")
	q := (int(next.Month())-1)/3 + 1
	if rows = csvRows(t, out); code != 0 || rows[0]["period"] != next.Format("2006")+"-Q"+strconv.Itoa(q) {
		t.Errorf("default window by quarter %d\n%s", code, out)
	}
	out, _, code = runIncome(t, cfg, "projection", year+"-01-01", year+"-03-31", "-f", "csv")
	if rows = csvRows(t, out); code != 0 || len(rows) != 5 || rows[2]["period"] != year+"-03" {
		t.Errorf("FROM TO %d\n%s", code, out)
	}
	// The MCP tool reads the same default window, and its header says so.
	text, err := callTool(t, newTestMCP(cfg, false), "income", map[string]any{"view": "projection"})
	last := next.AddDate(0, 12, -1).Format(time.DateOnly)
	if err != nil || !strings.HasPrefix(text, "income projection · window "+next.Format(time.DateOnly)+" → "+last) {
		t.Errorf("MCP default window %v\n%.200s", err, text)
	}
}

// The bond of the fixture pays its coupon on its dates, month ends as
// its maturity is; the calendar lists each with its day.
func TestIncomeCalendar(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	y := time.Now().UTC().Year() + 1
	if y >= 2031 {
		t.Skip("the fixture's bond matures in 2031")
	}
	year := strconv.Itoa(y)
	out, errb, code := runIncome(t, cfg, "calendar", year, "-f", "csv", "-C", "date,symbol,type,kind,amount")
	if code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	var coupons []string
	for _, r := range csvRows(t, out) {
		if r["type"] == "coupons" {
			coupons = append(coupons, r["date"]+"="+r["amount"])
		}
		if r["amount"] == "0.00" {
			t.Errorf("a payment of zero: %v", r)
		}
	}
	if strings.Join(coupons, " ") != year+"-06-30=200.00 "+year+"-12-31=200.00" {
		t.Errorf("coupons %v\n%s", coupons, out)
	}
}

// A flag the forward views do not take is refused, and so are the
// received views' forward flags, a period off projection, a window past
// the projection's end, an open bound and a third bound. Under -p no
// amount or account id prints.
func TestIncomeForwardRefusalsAndPrivacy(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	far := strconv.Itoa(time.Now().UTC().Year() + 5)
	for _, args := range [][]string{
		{"summary", "--rates", "forward"},
		{"types", "--shift", "50"},
		{"projection", "--period", "total"},
		{"projection", "--level", "primary"},
		{"projection", "--rates", "sideways"},
		{"projection", far},
		{"projection", "2026", "2027", "2028"},
		{"projection", "2027", "-"},
		{"projection", "-", "2027"},
		{"calendar", "--period", "quarterly"},
		{"positions", "--period", "annual"},
	} {
		if _, _, code := runIncome(t, cfg, args...); code == 0 {
			t.Errorf("%v: accepted", args)
		}
	}
	out, errb, code := runIncome(t, cfg, "positions", "-p", "-f", "csv", "-C", "+position_key")
	if code != 0 || strings.Contains(out, "ACCT7781") || strings.Contains(out, "10000.00") ||
		strings.Contains(out, "5000.00") || !strings.Contains(out, "QBND") {
		t.Errorf("privacy %d %s\n%s", code, errb, out)
	}
}
