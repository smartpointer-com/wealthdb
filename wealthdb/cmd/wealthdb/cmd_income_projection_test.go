package main

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
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

// A bond pays its coupon on its dates, month ends as its maturity is;
// the calendar lists each with its day. The bond matures three years
// after next year, whatever the year.
func TestIncomeCalendar(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	y := time.Now().UTC().Year() + 1
	year := strconv.Itoa(y)
	changeYieldGold(t, cfg,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		 first_seen_at, last_seen_at) VALUES ('brk', 'QCAL', 'fixed_income', 'bond',
		 'EXAMPLE CORP NOTE 4.00000% 06/30/`+strconv.Itoa(y+3)+`', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES
		 ('brk', ?, 'ACCT7781', 'QCAL', 'QCAL', 'fixed_income', 'bond', 'USD', 10000, 10000)`)
	out, errb, code := runIncome(t, cfg, "calendar", year, "-f", "csv", "-C", "date,position_key,type,kind,amount")
	if code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	var coupons []string
	for _, r := range csvRows(t, out) {
		if r["type"] == "coupons" && r["position_key"] == "QCAL" {
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
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"summary", "--rates", "forward"}, "--rates, --maturing and --shift belong to the projection"},
		{[]string{"types", "--shift", "50"}, "--rates, --maturing and --shift belong to the projection"},
		{[]string{"projection", "--period", "total"}, "the forward views take --period monthly | quarterly | annual"},
		{[]string{"projection", "--level", "primary"}, "--level belongs to the types view"},
		{[]string{"projection", "--rates", "sideways"}, "sideways"},
		{[]string{"projection", far}, "the projection ends on"},
		{[]string{"projection", "2026", "2027", "2028"}, "a window is FROM [TO]"},
		{[]string{"projection", "2027", "-"}, "takes no open bound"},
		{[]string{"projection", "-", "2027"}, "takes no open bound"},
		{[]string{"calendar", "--period", "quarterly"}, "of the forward views, --period belongs to projection"},
		{[]string{"positions", "--period", "annual"}, "of the forward views, --period belongs to projection"},
	} {
		if _, errb, code := runIncome(t, cfg, c.args...); code != 2 || !strings.Contains(errb, c.want) {
			t.Errorf("%v: exit %d, want 2 and %q:\n%s", c.args, code, c.want, errb)
		}
	}
	out, errb, code := runIncome(t, cfg, "positions", "-p", "-f", "csv", "-C", "+position_key")
	if code != 0 || strings.Contains(out, "ACCT7781") || strings.Contains(out, "10000.00") ||
		strings.Contains(out, "5000.00") || !strings.Contains(out, "QBND") {
		t.Errorf("privacy %d %s\n%s", code, errb, out)
	}
}

// Every basis and every way of reinvesting the forward views print
// reaches the CLI usage, wrapped to its width, and the MCP quality help,
// in the order of yield.Bases and yield.Rungs.
func TestForwardBasesReachBothFrontEnds(t *testing.T) {
	t.Parallel()
	usage := incomeUsage()
	for _, c := range []struct {
		tags []tagNote
		want []string
	}{
		{incomeBasisTags, stringsOf(yield.Bases)},
		{incomeRungTags, stringsOf(yield.Rungs)},
	} {
		var listed []string
		for _, q := range c.tags {
			listed = append(listed, q.tag)
			if !strings.Contains(usage, "\n    "+q.tag+" ") {
				t.Errorf("usage does not list %s", q.tag)
			}
			if !strings.Contains(qualityHelp, "\n- "+q.tag+": "+q.meaning+".") {
				t.Errorf("describe topic=quality does not list %s", q.tag)
			}
		}
		if !slices.Equal(listed, c.want) {
			t.Errorf("tags %v, want %v", listed, c.want)
		}
		for _, line := range strings.Split(tagNotes(c.tags), "\n") {
			if len(line) > 72 {
				t.Errorf("line is %d columns wide: %q", len(line), line)
			}
		}
	}
}
