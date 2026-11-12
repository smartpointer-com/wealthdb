package gold

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// lotSource is one source a pass replays.
type lotSource struct {
	id, kind string
	pol      lots.Policy
}

type lotKeyRef struct{ src, acct, inst string }

// lotKeyInfo is what the writer needs to know of a key.
type lotKeyInfo struct {
	src *lotSource
	// acct is the key's account: the account itself, or its portfolio
	// when the source pools at the portfolio grain.
	acct string
	inst string
	// hint marks an instrument only a hint names.
	hint     bool
	currency string
	// posCurrency marks a currency a position row set, which no trade
	// overrides.
	posCurrency bool
}

func (f *lotFeed) readSources(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT silver_source_id, silver_kind FROM silver_sources ORDER BY silver_source_id`)
	if err != nil {
		return fmt.Errorf("lots: read sources: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return err
		}
		p := f.cfg.PolicyFor(id, kind)
		switch p.Mode {
		case lots.Off:
			continue
		case lots.Stated:
			f.stated = append(f.stated, id)
			f.statedPol[id] = p
			continue
		}
		f.sources[id] = &lotSource{id: id, kind: kind, pol: p}
		f.ids = append(f.ids, id)
	}
	return rows.Err()
}

// inLotSources restricts col to the replayed sources; it binds f.ids.
func inLotSources(col string) string { return col + " IN (SELECT unnest(?::VARCHAR[]))" }

func (f *lotFeed) readPortfolios(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT src, acct, pid FROM portfolio_acct_map()`)
	if err != nil {
		return fmt.Errorf("lots: read portfolios: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src, acct, pid string
		if err := rows.Scan(&src, &acct, &pid); err != nil {
			return err
		}
		f.pid[[2]string{src, acct}] = pid
	}
	return rows.Err()
}

// readClasses marks the instruments that are never a key: a holding no
// cost basis describes (cash, a mortgage, an FX forward; GAINS.md §1),
// and one the source ever holds short, which a long-only book cannot
// carry.
func (f *lotFeed) readClasses(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id, holding_key(instrument_external_id, position_key),
       COALESCE(bool_or(quantity < 0), FALSE) AS short,
       bool_or(NOT basis_applies(asset_class, vehicle)) AS no_basis
  FROM positions WHERE `+inLotSources("silver_source_id")+` GROUP BY ALL`, f.ids)
	if err != nil {
		return fmt.Errorf("lots: read classes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src, inst string
		var short, noBasis bool
		if err := rows.Scan(&src, &inst, &short, &noBasis); err != nil {
			return err
		}
		if short || noBasis {
			f.never[[2]string{src, inst}] = true
		}
		if short && !noBasis {
			f.skipped[src]++
		}
	}
	return rows.Err()
}

// keyAccount is the account part of a key: the account, or its
// portfolio when the source pools.
func (f *lotFeed) keyAccount(s *lotSource, acct string) string {
	if s.pol.Grain == lots.GrainPortfolio {
		if pid := f.pid[[2]string{s.id, acct}]; pid != "" {
			return pid
		}
	}
	return acct
}

// key returns the key of an instrument in an account, opening it on
// first sight; -1 when the instrument is never a key.
func (f *lotFeed) key(s *lotSource, acct, inst string, hint bool) int32 {
	if inst == "" || f.never[[2]string{s.id, inst}] || (s.pol.Skip != nil && s.pol.Skip(inst)) {
		return -1
	}
	ka := f.keyAccount(s, acct)
	ref := lotKeyRef{s.id, ka, inst}
	if k, ok := f.keyOf[ref]; ok {
		return k
	}
	portfolio := f.pid[[2]string{s.id, acct}]
	account := acct
	if ka != acct {
		account = ""
	}
	k := int32(len(f.keys))
	f.keys = append(f.keys, lots.KeySpec{
		ID:     s.id + "\x00" + ka + "\x00" + inst,
		Method: f.cfg.MethodFor(s.id, portfolio, account, s.pol.Method),
		Fees:   s.pol.Fees,
		Basis:  f.cfg.BasisFor(s.id, ka, inst),
	})
	f.info = append(f.info, lotKeyInfo{src: s, acct: ka, inst: inst, hint: hint})
	f.keyOf[ref] = k
	return k
}

// setCurrency records the currency a key's amounts are in: the first
// position row's, else the first trade's. resolveValues converts every
// amount into it.
func (f *lotFeed) setCurrency(k int32, ccy string, fromPosition bool) {
	info := &f.info[k]
	if ccy == "" || info.posCurrency || (info.currency != "" && !fromPosition) {
		return
	}
	info.currency, info.posCurrency = ccy, fromPosition
}
