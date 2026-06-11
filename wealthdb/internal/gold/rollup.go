package gold

import (
	"context"
	"database/sql"

	"github.com/ptu/wealthdb/internal/canonical"
)

// srcKey identifies an aggregation bucket within a silver source:
// the account_external_id for the accounts rollup, the
// portfolio_external_id ("" = the per-source sentinel) for the
// portfolios rollup. Both rollups bucket their position + cash
// lines the same way, so they share this key.
type srcKey struct{ src, id string }

// lines collects the position + cash line items that roll into one
// bucket, plus the latest snapshot_at observed across them.
type lines struct {
	positions []lineItem
	cash      []lineItem
	maxSnap   int64
}

// addLine routes one position/cash value into its bucket. It always
// advances sourceMaxSnap[src] — even for a nil/unparseable value —
// so a bucket with no contributing lines can still report its
// source's latest snapshot rather than "unknown". A nil or garbage
// valueStr updates only that snapshot bookkeeping and adds no line.
func addLine(byKey map[srcKey]*lines, sourceMaxSnap map[string]int64,
	src, id, ccy string, valueStr *string, snap int64, isCash bool) {
	if snap > sourceMaxSnap[src] {
		sourceMaxSnap[src] = snap
	}
	if valueStr == nil {
		return
	}
	v, err := canonical.NewDecimalFromString(*valueStr)
	if err != nil {
		return
	}
	l, ok := byKey[srcKey{src, id}]
	if !ok {
		l = &lines{}
		byKey[srcKey{src, id}] = l
	}
	item := lineItem{currency: ccy, amount: v, snapshotAt: snap}
	if isCash {
		l.cash = append(l.cash, item)
	} else {
		l.positions = append(l.positions, item)
	}
	if snap > l.maxSnap {
		l.maxSnap = snap
	}
}

// valueColumns holds the six money columns both rollups compute:
// positions / cash / total in the bucket's own base currency (all
// nil when the base currency is unknown) and the same three in the
// requested output currency.
type valueColumns struct {
	positionsBase, cashBase, totalBase *string
	positionsOut, cashOut, totalOut    *string
}

// computeValueColumns sums the position + cash lines into the base
// and output currencies. Each sumConverted returns nil when no line
// converted, so a bucket with no FX path keeps a blank cell rather
// than a misleading 0. The base trio stays nil when baseCurrency is
// unknown (nil / empty).
func computeValueColumns(ctx context.Context, db *sql.DB,
	pos, cash []lineItem, baseCurrency *string, outCcy string,
	mode canonical.FxMode) valueColumns {
	var vc valueColumns
	if baseCurrency != nil && *baseCurrency != "" {
		base := *baseCurrency
		pSum := sumConverted(ctx, db, pos, base, mode)
		cSum := sumConverted(ctx, db, cash, base, mode)
		vc.positionsBase = decimalPtrString(pSum)
		vc.cashBase = decimalPtrString(cSum)
		vc.totalBase = decimalPtrString(addOptional(pSum, cSum))
	}
	pSum := sumConverted(ctx, db, pos, outCcy, mode)
	cSum := sumConverted(ctx, db, cash, outCcy, mode)
	vc.positionsOut = decimalPtrString(pSum)
	vc.cashOut = decimalPtrString(cSum)
	vc.totalOut = decimalPtrString(addOptional(pSum, cSum))
	return vc
}
