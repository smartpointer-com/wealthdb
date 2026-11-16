package spending

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// evidenceEquityLedger is the evidence of a link the equity-transfer
// ledger states: one ledger row names both legs of a move in kind.
// It is not `ledger`, which is the transfer override ledger's word.
const evidenceEquityLedger = "equity-ledger"

// statedMoveLegs are the legs of the moves one equity-transfer ledger
// row states, each with its partner. The loader names a leg's partner
// in its payload; a leg whose partner gold does not hold, or does not
// name it back, has no link. The `xfer:` prefix is the loader's mark of
// a ledger row, and it bounds the payload reads to those rows.
const statedMoveLegs = `
    SELECT a.silver_source_id, a.transaction_external_id,
           b.silver_source_id AS far_silver_source_id,
           b.account_external_id AS far_account_external_id,
           b.transaction_external_id AS far_transaction_external_id
      FROM transactions a
      JOIN transactions b
        ON b.silver_source_id = json_extract_string(a.payload, '$.` + canonical.PayloadPairSource + `')
       AND b.transaction_external_id = json_extract_string(a.payload, '$.` + canonical.PayloadPairTransaction + `')
     WHERE starts_with(a.transaction_external_id, 'xfer:')
       AND starts_with(b.transaction_external_id, 'xfer:')
       AND json_extract_string(b.payload, '$.` + canonical.PayloadPairSource + `') = a.silver_source_id
       AND json_extract_string(b.payload, '$.` + canonical.PayloadPairTransaction + `') = a.transaction_external_id`

// writeStatedMoveLinks links both legs of every move one ledger row
// states, with measure in_kind. It runs after writeTransferLinks, in
// the same transaction. A link it writes replaces the one the matcher
// wrote for the same leg: the ledger states the pair, the matcher
// inferred one from the cash amounts. Where the matcher paired a
// stated leg with some other leg, that leg's link goes too, so no link
// names a partner that does not name it back. That leg reads as one
// the matcher never paired; the matcher does not run again for it.
func writeStatedMoveLinks(ctx context.Context, tx *sql.Tx) error {
	for _, st := range []struct{ what, q string }{
		{"clear the matcher's partners of stated legs", clearStatedPartners},
		{"clear stated move links", clearStatedLinks},
	} {
		if _, err := tx.ExecContext(ctx, st.q); err != nil {
			return fmt.Errorf("spending: %s: %w", st.what, err)
		}
	}
	if _, err := tx.ExecContext(ctx, insertStatedLinks, evidenceEquityLedger, gold.LinkMeasureInKind); err != nil {
		return fmt.Errorf("spending: write stated move links: %w", err)
	}
	return nil
}

// The three statements of writeStatedMoveLinks, in its order.
const (
	// clearStatedPartners drops every link that names a stated leg as
	// its partner: the matcher's other half of a pair the ledger
	// overrides. The stated legs' own rows go with clearStatedLinks.
	clearStatedPartners = `
        DELETE FROM transfer_links l
         USING (` + statedMoveLegs + `) s
         WHERE l.far_silver_source_id = s.silver_source_id
           AND l.far_transaction_external_id = s.transaction_external_id`
	clearStatedLinks = `
        DELETE FROM transfer_links l
         USING (` + statedMoveLegs + `) s
         WHERE l.silver_source_id = s.silver_source_id
           AND l.transaction_external_id = s.transaction_external_id`
	insertStatedLinks = `
        INSERT INTO transfer_links (silver_source_id, transaction_external_id,
                                    far_silver_source_id, far_account_external_id,
                                    far_transaction_external_id, far_class,
                                    evidence, measure)
        SELECT silver_source_id, transaction_external_id, far_silver_source_id,
               far_account_external_id, far_transaction_external_id, NULL, ?, ?
          FROM (` + statedMoveLegs + `)`
)
