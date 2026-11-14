package spending

import (
	"context"
	"database/sql"
	"fmt"
)

// ---- phase 4: the signature-version re-key ---------------------------------

// rekeyStore carries paid-for verdicts across a normalisation change (a
// SignatureVersion bump), for whichever family's store it is given, so the
// model tier does not buy the same answers twice.
//
// A verdict is carried forward when all five hold: the row's signature
// actually moved, EVERY row that carried the old signature now carries the
// same new one, the store holds a verdict at the OLD signature, that verdict
// predates the current SignatureVersion, and the NEW signature has no
// verdict yet. An old key whose rows split across several new keys was a
// shape several creditors shared, not a merchant, so its verdict stays
// behind and is counted; an existing verdict at a new key is never
// overwritten; model_name is preserved verbatim. docs/SPENDING.md §4 argues
// each condition. The rows arrive sorted, so when two old signatures
// collapse onto one new one the first wins, whatever the map order.
func rekeyStore(
	ctx context.Context,
	tx *sql.Tx,
	fam family,
	rows []enrichmentRow,
	old map[txKey]signatureAt,
	verdicts map[string]storedVerdict,
	now int64,
) (carried, split int, err error) {
	// Where each old signature's rows landed under the current rules.
	// An old signature with more than one destination has split, and
	// nothing is carried from it. An empty destination counts: a row
	// the current rules cannot sign at all did not land with the rest.
	landed := map[string]map[string]bool{}
	for _, r := range rows {
		if prev, ok := old[r.key]; ok {
			if landed[prev.signature] == nil {
				landed[prev.signature] = map[string]bool{}
			}
			landed[prev.signature][r.signature] = true
		}
	}
	for sig, dests := range landed {
		if len(dests) < 2 {
			continue
		}
		if verdict, ok := verdicts[sig]; ok && verdict.version < SignatureVersion {
			split++
		}
	}

	type carry struct {
		signature string
		verdict   storedVerdict
	}
	var (
		plan    []carry
		claimed = map[string]bool{}
	)
	for _, r := range rows {
		prev, ok := old[r.key]
		if !ok || r.signature == "" || prev.signature == r.signature {
			continue
		}
		if len(landed[prev.signature]) > 1 {
			continue // a split: the old key was an artefact, see above
		}
		if _, exists := verdicts[r.signature]; exists || claimed[r.signature] {
			continue
		}
		verdict, ok := verdicts[prev.signature]
		if !ok || verdict.version >= SignatureVersion {
			continue
		}
		claimed[r.signature] = true
		plan = append(plan, carry{r.signature, verdict})
	}
	if len(plan) == 0 {
		return 0, split, nil
	}

	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO `+fam.storeTable+` (
            `+fam.storeSignatureCol+`, `+fam.storeNameCol+`, `+fam.storeDetailedCol+`,
            signature_version, assigned_at, model_name
        ) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: prepare verdict re-key: %w", fam.name, err)
	}
	defer stmt.Close()
	for _, p := range plan {
		if _, err := stmt.ExecContext(ctx, p.signature, p.verdict.name, p.verdict.detailed,
			SignatureVersion, now, p.verdict.modelName); err != nil {
			return 0, 0, fmt.Errorf("%s: re-key verdict onto %q: %w", fam.name, p.signature, err)
		}
	}
	return len(plan), split, nil
}
