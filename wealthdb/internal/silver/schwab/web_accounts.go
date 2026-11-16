package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// snapshotsDimensions emits AccountChange rows from web's
// `accounts` table for every snapshot in the window, rewriting
// account_external_id from web's suffix to the bridged account id.
// Web supplies the user-friendly `nickname`. With the api beside
// it, the api supplies almost everything else, and the per-column
// upsert guard in gold merges them by (silver_source_id,
// account_external_id).
//
// With the api beside it, an account whose web suffix doesn't bridge
// (e.g. a closed account no longer in api) is skipped: it has no api
// counterpart to merge with, and emitting it under the raw suffix
// would create orphaned rows the rest of gold can't join.
//
// alone says the web silver is configured without the api: each row
// then also carries the display name and currency the api's would.
func (r *webReader) snapshotsDimensions(
	ctx context.Context,
	w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch,
	bridge map[string]string,
	alone bool,
) error {
	if !w.HasChanges {
		return nil
	}
	// Silver migration 0003 added `account_registration` (the
	// verbatim statement-PDF registration label —
	// "Contributory IRA" / "Schwab One® Custodial Account
	// (UTMA)" / "Education Savings" / etc.). Older silvers
	// don't have the column; degrade gracefully.
	s, err := r.schema(ctx)
	if err != nil {
		return err
	}
	regCol := silver.ColumnOr(s.registration, "account_registration")
	q := fmt.Sprintf(`
SELECT snapshot_at, account_external_id, nickname, payload, COALESCE(%s, '')
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`, regCol)
	return silver.ScanRows(ctx, r.db, "schwab-web snapshotsDimensions", q, func(rows *sql.Rows) error {
		var (
			snap         int64
			suffix       string
			nickname     sql.NullString
			payload      string
			registration string
		)
		if err := rows.Scan(&snap, &suffix, &nickname, &payload, &registration); err != nil {
			return err
		}
		hash, ok := bridge[suffix]
		if !ok {
			return nil
		}
		batch, ok := byTime[snap]
		if !ok {
			return nil
		}
		change := canonical.AccountChange{
			AccountExternalID: hash,
			AccountKind:       canonical.AccountKindBrokerage,
			Nickname:          silver.StrPtrIfNonEmpty(nickname.String),
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		}
		if alone {
			// The columns the api's account rows carry otherwise: the
			// account number as its display name, in USD.
			change.DisplayName = silver.StrPtrIfNonEmpty(hash)
			change.BaseCurrency = silver.StrPtrIfNonEmpty("USD")
		}
		if registration != "" {
			// Forward the raw Schwab label as AccountCategory
			// (verbatim, for forensics) and derive the canonical
			// TaxWrapper. Unknown labels leave TaxWrapper nil so
			// the render-time default (taxable_personal) kicks in.
			cat := registration
			change.AccountCategory = &cat
			if tw := taxWrapperForRegistration(registration); tw != "" {
				change.TaxWrapper = &tw
			}
		}
		batch.Accounts = append(batch.Accounts, change)
		return nil
	}, w.Start, w.End)
}
