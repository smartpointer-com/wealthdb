package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SetFxPriorities stamps silver_sources.fx_priority with each source's
// rank in `order` (index 0 = highest priority). Sources not listed are
// set to NULL (the fx_norm/fx_daily views sort NULL last, i.e. lowest
// priority). It re-stamps ALL sources in one statement, so editing the
// config's fx_priority and re-loading any source refreshes precedence
// globally without per-source drift.
//
// The load path (cmd_load / cmd_reload) calls this after the config's
// FxSourceOrder() is known and the silver_sources rows are upserted, so
// the SQL FX layer honours config-driven precedence. A no-op-ish call
// with an empty order clears ranks (date-only tiebreaking).
func SetFxPriorities(ctx context.Context, db *sql.DB, order []string) error {
	if len(order) == 0 {
		_, err := db.ExecContext(ctx, `UPDATE silver_sources SET fx_priority = NULL`)
		if err != nil {
			return fmt.Errorf("SetFxPriorities (clear): %w", err)
		}
		return nil
	}
	var b strings.Builder
	b.WriteString("UPDATE silver_sources SET fx_priority = CASE silver_source_id")
	args := make([]any, 0, len(order))
	for i, id := range order {
		b.WriteString(fmt.Sprintf(" WHEN ? THEN %d", i))
		args = append(args, id)
	}
	b.WriteString(" ELSE NULL END")
	if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
		return fmt.Errorf("SetFxPriorities: %w", err)
	}
	return nil
}
