package spending

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// The cash flow statement's half of the enrichment pass.
//
// It is not a family and does not enrich anything. Cashflow adds no
// categorisation tier and buys nothing from a model: it reads the
// verdicts the two families already wrote and folds them into a
// statement. What it needs from the pass is the one thing a macro
// cannot reach — the boundary, which is half an engine default and
// half configuration — stamped into gold so the CLI and the dashboard
// read one answer.
//
// It runs inside the pass's transaction, after both families, for the
// reason the scope stamp runs first inside one: a boundary written in
// a transaction that later rolls back would leave gold describing a
// household nobody configured.

// CashflowOptions is the `cashflow` config block as the pass takes it:
// the pool's exclusions, and the per-wrapper boundary overrides keyed
// by tax wrapper and valued by destination. Both are already validated
// — config.Validate refuses an unknown wrapper or destination — so a
// value reaching here is one canonical recognises.
type CashflowOptions struct {
	Exclude  map[string][]string
	Wrappers map[string]string
}

// CashflowResult is what the stamp did, in the shape the two families'
// counters take.
type CashflowResult struct {
	// ScopeRows is the number of pool exclusions stamped, and
	// UnresolvedScopeAccounts how many of them name an account gold
	// does not hold. Counted rather than refused, for the reason a
	// family's are: the key is an account id, and a source whose
	// accounts have not loaded yet has none to resolve against.
	ScopeRows               int
	UnresolvedScopeAccounts int
	// WrapperRows is the number of wrappers stamped — every value of
	// the enum, defaults included — and WrapperOverrides how many of
	// them a config entry moved.
	WrapperRows      int
	WrapperOverrides int
	// FirstPass reports that the boundary table was EMPTY when this
	// pass began — the state between applying the cashflow migrations
	// and running the first load. In it the statement is not merely
	// incomplete, it is wrong in a way that looks like a finding: the
	// pool is every account, no enrichment row carries a far account,
	// and every matched own-account move therefore resolves to
	// `vehicles · Unpaired transfers`. A reader who opens the
	// dashboard in that window sees one enormous node and has no way
	// to tell it from a data problem, so the load that ends the state
	// says that it did.
	FirstPass bool
	// FarAccounts is how many enrichment rows this pass wrote a far
	// account onto — the other half of what the resolution needs, and
	// a plain counter once the first pass is past. StatedFarAccounts is
	// the subset the SOURCE named rather than the matcher paired, which
	// is the only road that reaches a movement the product collects one
	// side of.
	FarAccounts       int
	StatedFarAccounts int
	// ReferencePairs is how many own-account moves were paired on the
	// reference their source stamped on both legs rather than on
	// amount and day — of the ways the matcher joins two legs the
	// strongest short of the holder's own word, and the only one that
	// reaches a movement whose two legs are denominated differently.
	//
	// It is counted for the same reason StatedFarAccounts is: a road
	// nobody can see the traffic on is a road nobody can tell has
	// stopped working. The number also answers the one operational
	// question this road raises — the references travel in the
	// payload, so a source whose rows predate the adapter that stamps
	// them carries none until a `reload`, and a count of zero where
	// pairs are expected is what says the reload has not happened.
	ReferencePairs int
	// StatedCounterPairs is how many were paired on the OTHER LEG the
	// source described — its currency and its figure — rather than on a
	// reference or on amount and day. It is the road that reaches a
	// currency conversion the source stamped no reference on, and it is
	// counted separately because it is the weakest of the three joins
	// the matcher asserts on: a description is matched rather than
	// read, so a number that grows
	// out of proportion to the references is the shape to look at.
	StatedCounterPairs int
	// NamedPairs is how many were paired because one leg's narrative
	// named the other's account (spending.internal_transfer_matching.names)
	// — still on amount and day, but ahead of every pair those alone make.
	NamedPairs int
	// AmbiguousReferences is how many (source, reference) groups were
	// REFUSED because the source had stamped the same reference on
	// MORE than two rows — so it named something, and what it named
	// was not one movement.
	//
	// It is the early warning that a reference space is not per
	// movement. A handful is ordinary — a bank booking a charge under
	// the reference of the payment it belongs to produces one — but a
	// number that climbs with the archive says the source mints its
	// references per day or per batch, and that no pair drawn from
	// them should be trusted.
	AmbiguousReferences int
	// DeclaredAccounts is how many accounts the deployment declared
	// rather than collected (config `declared_accounts`), and
	// DeclaredPooled how many of them the boundary places inside the
	// household pool — a move to one of those draws nothing, and a
	// declaration is unfalsifiable by the product's own instruments, so
	// this is the one place the number is ever printed.
	// UnusedDeclarations is how many no rule-placed row reached: a
	// declaration nothing points at is a rule that never fired or was
	// never written.
	DeclaredAccounts   int
	DeclaredPooled     int
	UnusedDeclarations int
	// PooledAccountsWithoutWrapper is the boundary's coverage gap:
	// accounts in the pool whose tax wrapper is unset.
	//
	// An unset wrapper reads as household, which is the safe direction
	// — nothing is silently removed from the statement — but it is not
	// free, and nothing downstream can see the cost. An adapter that
	// leaves the column unset puts a retirement or health account
	// inside the pool, where its own trades become household investing
	// and its contributions become invisible; the crossing is ABSENT
	// rather than wrong, so no reconciliation can find it. The boundary
	// rests on wrapper coverage, so the load says so out loud.
	PooledAccountsWithoutWrapper int
}

// stampCashflowBoundary writes the resolved household boundary and the
// cash pool's exclusions into gold, replacing whatever was there.
//
// The wrapper table is stamped WHOLE — every value of the enum, not
// just the overridden ones — which is what makes
// canonical.DefaultWrapperSide the single place a wrapper's side is
// decided. Stamping only the overrides would mean the defaults lived a
// second time in SQL, and a wrapper added for a new jurisdiction would
// take two edits to reach the statement instead of one.
func stampCashflowBoundary(ctx context.Context, tx *sql.Tx, opts CashflowOptions, out *CashflowResult) error {
	// Read BEFORE stampWrapperSides clears the table: an empty
	// boundary is what the state between the migrations and the first
	// load looks like, and it is unrecoverable once this pass has
	// written into it.
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM cashflow_wrapper_sides`).
		Scan(&existing); err != nil {
		return fmt.Errorf("cashflow: read the standing boundary: %w", err)
	}
	out.FirstPass = existing == 0

	var err error
	out.ScopeRows, out.UnresolvedScopeAccounts, err =
		syncAccountScope(ctx, tx, "cashflow", "cashflow_account_scope", nil, opts.Exclude)
	if err != nil {
		return err
	}
	if err := stampWrapperSides(ctx, tx, opts.Wrappers, out); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM spend_txn_enrichment
         WHERE far_silver_source_id IS NOT NULL`).Scan(&out.FarAccounts); err != nil {
		return fmt.Errorf("cashflow: count far accounts: %w", err)
	}
	return countPooledAccountsWithoutWrapper(ctx, tx, out)
}

// stampWrapperSides replaces cashflow_wrapper_sides with the engine's
// boundary as the config amends it.
//
// An override naming a wrapper gold's enum does not hold cannot reach
// here — config.Validate refuses it — so an entry that matches no
// wrapper of the table is impossible rather than merely counted, which
// is the difference between this knob and the account scope: a wrapper
// is a closed vocabulary, an account id is not.
func stampWrapperSides(ctx context.Context, tx *sql.Tx, overrides map[string]string, out *CashflowResult) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM cashflow_wrapper_sides`); err != nil {
		return fmt.Errorf("cashflow: clear wrapper sides: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO cashflow_wrapper_sides (tax_wrapper, side, class)
        VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("cashflow: prepare wrapper sides: %w", err)
	}
	defer stmt.Close()

	for _, b := range canonical.WrapperBoundaries() {
		side, class := b.Side, b.Class
		if dest, ok := overrides[string(b.Wrapper)]; ok {
			s, c, parsed := canonical.ParseWrapperDestination(dest)
			if !parsed {
				// Unreachable through config.Load, which validates
				// against the same predicate. Raised rather than
				// ignored because the alternative is a boundary that
				// silently keeps the default it was asked to move.
				return fmt.Errorf("cashflow: wrapper %q: %q is not a destination", b.Wrapper, dest)
			}
			side, class = s, c
			out.WrapperOverrides++
		}
		if _, err := stmt.ExecContext(ctx, string(b.Wrapper), string(side), nullableString(string(class))); err != nil {
			return fmt.Errorf("cashflow: stamp wrapper %q: %w", b.Wrapper, err)
		}
		out.WrapperRows++
	}
	return nil
}

// countDeclaredAccounts reads the declarations off the accounts
// dimension and the pool macro, and the ones no far column names off
// the spending overlay, so each number says what the statement will
// draw rather than what the config meant.
func countDeclaredAccounts(ctx context.Context, tx *sql.Tx, out *CashflowResult) error {
	if err := tx.QueryRowContext(ctx, `
        SELECT COUNT(*),
               COUNT(*) FILTER (WHERE p.account_external_id IS NOT NULL),
               COUNT(*) FILTER (WHERE NOT EXISTS (
                   SELECT 1 FROM spend_txn_enrichment e
                    WHERE e.far_silver_source_id    = a.silver_source_id
                      AND e.far_account_external_id = a.account_external_id))
          FROM accounts a
          LEFT JOIN cashflow_pool_accounts() p
                 ON p.silver_source_id    = a.silver_source_id
                AND p.account_external_id = a.account_external_id
         WHERE a.silver_source_id = ?`, canonical.DeclaredSourceID).
		Scan(&out.DeclaredAccounts, &out.DeclaredPooled, &out.UnusedDeclarations); err != nil {
		return fmt.Errorf("cashflow: count declared accounts: %w", err)
	}
	return nil
}

// countPooledAccountsWithoutWrapper reads the boundary's coverage gap
// off the pool macro itself, so the number says what the statement
// will actually draw rather than what a restated predicate thinks it
// will.
func countPooledAccountsWithoutWrapper(ctx context.Context, tx *sql.Tx, out *CashflowResult) error {
	if err := tx.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM cashflow_pool_accounts() WHERE tax_wrapper IS NULL`).
		Scan(&out.PooledAccountsWithoutWrapper); err != nil {
		return fmt.Errorf("cashflow: count pooled accounts with no wrapper: %w", err)
	}
	return nil
}
