package gold

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// ClassRefiner reads a lookup's type of a security over the pair an
// adapter gave its instrument (silver.RefineClassFunc): it returns the
// (asset_class, vehicle) of an instrument named name, which the adapter
// classed (ac, v) and OpenFIGI types securityType. It is an alias so
// that silver.ClassRefiners' map is the pass's map.
type ClassRefiner = func(securityType string, ac canonical.AssetClass, v canonical.Vehicle, name string) (canonical.AssetClass, canonical.Vehicle)

// ClassCounts is what the class pass rewrote: the instrument rows and
// the position rows whose pair it changed.
type ClassCounts struct {
	Instruments, Positions int64
}

// ClassPin is a pair the config pins on an instrument
// (instrument_overrides): the instrument and every position row of it
// read it, whatever the adapter or a lookup says.
type ClassPin struct {
	AssetClass canonical.AssetClass
	Vehicle    canonical.Vehicle
}

// RefineClasses is the class pass. pins names, per source, the
// instruments the config pins. Each of them, and every position row of
// it, takes the pinned pair, whatever its source's kind. For each source
// whose kind has a refiner (refiners, by kind), the pass reads
// OpenFIGI's type of every other instrument's security over the pair
// the load gave the instrument and every position row of it.
//
// It runs after the per-source loads, so a type a later refdata load
// brings reaches every source, and a pin reaches rows an earlier load
// wrote. It reads every row each time: a load writes an instrument's
// pair as the adapter reads it, and the pass reads the pin or the type
// over it again. A refiner reads its own output as it is, so a second
// pass changes nothing. One transaction.
func RefineClasses(ctx context.Context, db *sql.DB, refiners map[string]ClassRefiner,
	pins map[string]map[string]ClassPin) (ClassCounts, error) {
	var n ClassCounts
	if len(refiners) == 0 && len(pins) == 0 {
		return n, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return n, fmt.Errorf("class pass: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit

	bySource := map[string]ClassRefiner{}
	types := map[string]string{}
	typed := map[[2]string]typedInstrument{}
	var changes []classChange
	if err := runSteps("class pass", []step{
		{"sources", func() error {
			return scanAll(ctx, tx, `SELECT silver_source_id, silver_kind FROM silver_sources`, func(rows *sql.Rows) error {
				var id, kind string
				if err := rows.Scan(&id, &kind); err != nil {
					return err
				}
				if r := refiners[kind]; r != nil {
					bySource[id] = r
				}
				return nil
			})
		}},
		{"types", func() error {
			return scanAll(ctx, tx, figiTypesSQL, func(rows *sql.Rows) error {
				var key, typ string
				if err := rows.Scan(&key, &typ); err != nil {
					return err
				}
				types[key] = typ
				return nil
			})
		}},
		{"instruments", func() error {
			return scanAll(ctx, tx, `
SELECT silver_source_id, instrument_external_id, COALESCE(isin, ''), COALESCE(cusip, ''), COALESCE(symbol, ''),
       COALESCE(name, '')
  FROM instruments`, func(rows *sql.Rows) error {
				var src, iid, isin, cusip, symbol, name string
				if err := rows.Scan(&src, &iid, &isin, &cusip, &symbol, &name); err != nil {
					return err
				}
				if _, pinned := pins[src][iid]; pinned || bySource[src] == nil {
					return nil
				}
				if t := figiType(types, isin, cusip, symbol); t != "" {
					typed[[2]string{src, iid}] = typedInstrument{securityType: t, name: name}
				}
				return nil
			})
		}},
		{"pairs", func() error {
			return scanAll(ctx, tx, `
SELECT silver_source_id, instrument_external_id, asset_class, COALESCE(vehicle, '') FROM instruments
UNION
SELECT silver_source_id, instrument_external_id, asset_class, COALESCE(vehicle, '') FROM positions
 WHERE instrument_external_id IS NOT NULL`, func(rows *sql.Rows) error {
				var src, iid, class, vehicle string
				if err := rows.Scan(&src, &iid, &class, &vehicle); err != nil {
					return err
				}
				c := classChange{src: src, iid: iid, fromClass: canonical.AssetClass(class),
					fromVehicle: canonical.Vehicle(vehicle)}
				if pin, ok := pins[c.src][c.iid]; ok {
					c.toClass, c.toVehicle = pin.AssetClass, pin.Vehicle
				} else if t, ok := typed[[2]string{c.src, c.iid}]; ok {
					c.toClass, c.toVehicle = bySource[c.src](t.securityType, c.fromClass, c.fromVehicle, t.name)
				} else {
					return nil
				}
				if c.toClass != c.fromClass || c.toVehicle != c.fromVehicle {
					changes = append(changes, c)
				}
				return nil
			})
		}},
		{"rewrite", func() error {
			for _, c := range changes {
				for _, t := range []struct {
					table string
					count *int64
				}{{"instruments", &n.Instruments}, {"positions", &n.Positions}} {
					res, err := tx.ExecContext(ctx, `UPDATE `+t.table+` SET asset_class = ?, vehicle = ?
 WHERE silver_source_id = ? AND instrument_external_id = ? AND asset_class = ? AND COALESCE(vehicle, '') = ?`,
						string(c.toClass), string(c.toVehicle), c.src, c.iid, string(c.fromClass), string(c.fromVehicle))
					if err != nil {
						return fmt.Errorf("%s %s/%s: %w", t.table, c.src, c.iid, err)
					}
					k, err := res.RowsAffected()
					if err != nil {
						return err
					}
					*t.count += k
				}
			}
			return nil
		}},
	}); err != nil {
		return ClassCounts{}, err
	}
	if err := tx.Commit(); err != nil {
		return ClassCounts{}, fmt.Errorf("class pass: commit: %w", err)
	}
	return n, nil
}

// typedInstrument is an instrument OpenFIGI types, with its name.
type typedInstrument struct{ securityType, name string }

// classChange is a pair the pass rewrites: an instrument's pair on its
// instrument row and on each position row of it that holds the pair.
type classChange struct {
	src, iid               string
	fromClass, toClass     canonical.AssetClass
	fromVehicle, toVehicle canonical.Vehicle
}

// figiTypesSQL is OpenFIGI's latest type of each security it found.
const figiTypesSQL = `
SELECT security_key, security_type FROM security_identities
 WHERE provider = 'openfigi' AND trim(COALESCE(security_type, '')) <> ''
QUALIFY ROW_NUMBER() OVER (PARTITION BY security_key ORDER BY as_of DESC) = 1`

// figiType is OpenFIGI's type of an instrument's security, under the
// first key it has one for: the instrument's ISIN, the US ISIN its CUSIP
// stands for, its CUSIP, its symbol. The wanted list asks by the ISIN a
// CUSIP aliases to where it knows one, else by the CUSIP or the symbol.
// The keys come from the instrument alone, so the pass reads the same
// type whether or not an exposure pass has aliased the keys yet.
func figiType(types map[string]string, isin, cusip, symbol string) string {
	for _, k := range []string{
		canonical.SecurityKey(isin, "", "", "", ""),
		canonical.SecurityKey(canonical.ISINFromCUSIP(cusip), "", "", "", ""),
		canonical.SecurityKey("", cusip, "", "", ""),
		canonical.SecurityKey("", "", "", symbol, ""),
	} {
		if t := types[k]; k != "" && t != "" {
			return t
		}
	}
	return ""
}
