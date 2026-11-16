package gold

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The derived set stamps the vocabulary tables whole from the binary's
// own (vocabularyStamps): every row, every column, and nothing else.
func TestDerivedStampsTheVocabulary(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	// A stale row and a hand edit, which the next apply replaces.
	mustExec(t, db, `INSERT INTO account_kinds VALUES ('stale', false, false, false, false);
	    UPDATE spend_categories SET label = 'edited' WHERE spend_detailed = 'gift'`)
	mustExec(t, db, `DELETE FROM derived_state`)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, st := range vocabularyStamps() {
		var want []string
		for _, r := range st.rows {
			want = append(want, fmt.Sprint(r...))
		}
		slices.Sort(want)
		var got []string
		cols := ""
		for i, c := range st.cols {
			if i > 0 {
				cols += ", "
			}
			cols += c
		}
		rows, err := db.QueryContext(ctx, `SELECT `+cols+` FROM `+st.table)
		if err != nil {
			t.Fatalf("%s: %v", st.table, err)
		}
		for rows.Next() {
			vals := make([]any, len(st.cols))
			ptrs := make([]any, len(st.cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprint(vals...))
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s holds %d rows, the binary %d:\n got %q\nwant %q", st.table, len(got), len(want), got, want)
		}
	}
}

// The predicates at the head of the flow macros say what the kind and
// value roles say (canonical's kind tables and CategoryRoles): each
// kind at each sign, each value, and a word in neither.
func TestFlowPredicatesHoldTheRoles(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	sign := func(a int) string { return [...]string{"negative", "zero", "positive"}[a+1] }
	rows := append(canonical.TxKindRows(), canonical.TxKindRow{Kind: "no_such_kind"})
	for _, k := range rows {
		for _, a := range []int{-1, 0, 1} {
			var spending, income, pool, unsigned, trade bool
			var incomeFloor, spendFloor sql.NullString
			if err := db.QueryRowContext(ctx, `SELECT kind_is_spending(?1, ?2), kind_is_income(?1, ?2),
			        kind_in_matcher_pool(?1), kind_is_unsigned(?1), kind_is_trade(?1),
			        income_kind_floor(?1, ?2), spend_kind_floor(?1, ?2, 'brokerage', NULL)`,
				string(k.Kind), a).Scan(&spending, &income, &pool, &unsigned, &trade,
				&incomeFloor, &spendFloor); err != nil {
				t.Fatalf("%s: %v", k.Kind, err)
			}
			at := fmt.Sprintf("%s, %s", k.Kind, sign(a))
			if spending != k.Spending.Admits(a) || income != k.Income.Admits(a) {
				t.Errorf("%s: spending %v income %v, the roles %q %q", at, spending, income, k.Spending, k.Income)
			}
			if pool != k.MatcherPool || unsigned != k.Unsigned || trade != k.Trade {
				t.Errorf("%s: pool %v unsigned %v trade %v, the roles %+v", at, pool, unsigned, trade, k)
			}
			wantIncome := ""
			if k.Income.Admits(a) {
				wantIncome = k.IncomeFloor
			}
			if incomeFloor.String != wantIncome {
				t.Errorf("%s: income floor %q, the role %q", at, incomeFloor.String, wantIncome)
			}
			wantSpend := ""
			switch {
			case k.Kind == canonical.TxKindFee:
				wantSpend = canonical.SpendDetailedInvestmentFees
			case k.Spending.Admits(a):
				wantSpend = k.SpendFloor
			}
			if spendFloor.String != wantSpend {
				t.Errorf("%s: spend floor %q, the role %q", at, spendFloor.String, wantSpend)
			}
		}
	}
	// A fee's floor reads the account: a card's, and a cash account's
	// in no portfolio, is a bank fee.
	for _, c := range []struct {
		account   string
		portfolio any
		want      string
	}{
		{"card", nil, "BANK_FEES_OTHER_BANK_FEES"},
		{"card", "P", "BANK_FEES_OTHER_BANK_FEES"},
		{"cash", nil, "BANK_FEES_OTHER_BANK_FEES"},
		{"cash", "P", canonical.SpendDetailedInvestmentFees},
		{"custody", nil, canonical.SpendDetailedInvestmentFees},
	} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT spend_kind_floor('fee', -1, ?, ?)`, c.account, c.portfolio).
			Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("a fee on a %s account in portfolio %v floors to %s, want %s", c.account, c.portfolio, got, c.want)
		}
	}

	cats := slices.Concat(canonical.SpendCategories, []canonical.SpendCategory{{Detailed: "NO_SUCH_VALUE"}})
	for _, c := range cats {
		r := c.Roles()
		var staysSpending, staysIncome bool
		var group string
		if err := db.QueryRowContext(ctx, `SELECT detailed_stays_in_spending(?1), detailed_stays_in_income(?1),
		        fee_group(?1)`, c.Detailed).Scan(&staysSpending, &staysIncome, &group); err != nil {
			t.Fatalf("%s: %v", c.Detailed, err)
		}
		if staysSpending == r.LeavesSpending || staysIncome == r.LeavesIncome {
			t.Errorf("%s: stays in spending %v income %v, the roles leave %v %v", c.Detailed, staysSpending,
				staysIncome, r.LeavesSpending, r.LeavesIncome)
		}
		wantGroup := r.FeeGroup
		if wantGroup == "" {
			wantGroup = canonical.FeeGroupOther
		}
		if group != wantGroup {
			t.Errorf("%s: fee group %q, the role %q", c.Detailed, group, wantGroup)
		}
	}
}
