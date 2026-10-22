package fidelity

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// A statement row the silver's cross-walk left keyless names its
// security by the statement's own description: the cross-walk matches
// only a live position of the same account with the very same text, so a
// statement parsed before any live snapshot, or one whose description a
// page footer ("… - 3 of 9") or the column's width cut, keeps no key.
// The rest of the silver names the same security by its key: a live
// position, a trade, a keyed statement row. So the adapter links such a
// row by what the statement states, in order:
//
//   - the ticker it prints in parentheses ("… (EXMP) -"), where the silver
//     keys a security by it;
//   - its description, where it has at least histLinkMinWords words and
//     exactly one key carries it as a name;
//   - else where it starts such a name, or that name starts it (the
//     statement and the trade file cut a long name at their columns'
//     width), and the keys carrying one are one security: a single key,
//     or the keys one chain of corporate actions moved its shares
//     through. Both names have at least histLinkMinWords words, so a
//     short pool name ("Growth") never links. A row that prints a ticker
//     the silver keys nothing by was not cut, so it takes no such road:
//     a longer name is another share class, not its own;
//   - from a corporate action's day on, the key the action moved a
//     name-linked key's shares to: a statement goes on printing the old
//     name after a merger, and the new key's positions may carry it too.
//
// A row no road answers keeps its synthetic key, and so does one whose
// link would name a key its account's statement already holds that day.
// What the silver states is read whole, not by the load window, so a
// row's link never depends on which window a load reads.

// histLinkMinWords is the fewest words a name must have to link by name:
// the statement's own, by either road, and a name it starts or that
// starts it.
const histLinkMinWords = 3

var (
	// histFooterRe is a statement's page footer read into a holding's
	// description, with the dash before it.
	histFooterRe = regexp.MustCompile(`\s+-\s+\d+\s+of\s+\d+\s*$`)
	// histTickerRe is a ticker printed in parentheses.
	histTickerRe = regexp.MustCompile(`\(([A-Z][A-Z0-9./]{0,9})\)`)
	// histFromRe is the old key a corporate action's in-leg names
	// ("MERGER MER FROM 000000001#REOR …").
	histFromRe = regexp.MustCompile(`\bFROM\s+([0-9A-Z]{3,12})\b`)
)

// historicalDescription is a statement row's description without the
// page footer, the dangling dash or the ellipsis a cut line ends in: the
// name its instrument carries.
func historicalDescription(desc string) string {
	d := histFooterRe.ReplaceAllString(strings.TrimSpace(desc), "")
	for {
		t := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(d, "-"), "..."))
		if t == d {
			return strings.Join(strings.Fields(d), " ")
		}
		d = t
	}
}

// linkedClass is a linked row's class and vehicle: classifyHistoricalPair
// on its key and cleaned name, as a keyed row reads, except that a
// CUSIP key other than a plan code says nothing, since its shape reads
// as a bond's.
func linkedClass(key, name string) (canonical.AssetClass, canonical.Vehicle) {
	if silver.StmtCUSIPRe.MatchString(key) && !histPlanKeyRe.MatchString(key) {
		key = ""
	}
	return classifyHistoricalPair(key, name)
}

// histSuccessor is the key a corporate action moved a key's shares to,
// and its day.
type histSuccessor struct {
	key string
	at  int64
}

// historicalLinks is what the silver states of the securities its
// statement rows name: every key a live position, a trade or a keyed
// statement row names, the names they carry them under, the corporate
// actions that replaced a key, and the keys each account's statement
// holds on each day.
type historicalLinks struct {
	keys       map[string]bool
	names      map[string]map[string]bool
	successors map[string]histSuccessor
	held       map[histHolding]bool
}

// histHolding is a key an account's statement holds on a day.
type histHolding struct {
	at        int64
	acct, key string
}

// historicalLinks reads the whole silver once. A trade's name and its
// corporate action are read through parseTxPayload, as the transactions
// path reads them.
func (c *Connection) historicalLinks(ctx context.Context) (*historicalLinks, error) {
	l := &historicalLinks{keys: map[string]bool{}, names: map[string]map[string]bool{},
		successors: map[string]histSuccessor{}, held: map[histHolding]bool{}}
	add := func(key, name string) {
		l.keys[key] = true
		if n := strings.ToUpper(historicalDescription(name)); n != "" {
			if l.names[n] == nil {
				l.names[n] = map[string]bool{}
			}
			l.names[n][key] = true
		}
	}
	if err := scanEach(ctx, c.db, `
SELECT instrument_key, COALESCE(description, '') FROM positions
UNION SELECT instrument_key, description FROM historical_position_snapshots WHERE instrument_key IS NOT NULL`,
		func(s *sql.Rows) error {
			var key, name string
			if err := s.Scan(&key, &name); err != nil {
				return err
			}
			add(key, name)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("historicalLinks names: %w", err)
	}
	// Oldest first, so a key replaced twice keeps its first successor and
	// the chain is walked from there.
	if err := scanEach(ctx, c.db, `
SELECT instrument_key, timestamp, kind, payload FROM transactions
 WHERE instrument_key IS NOT NULL ORDER BY timestamp, instrument_key`,
		func(s *sql.Rows) error {
			var (
				key, kind, payload string
				at                 int64
			)
			if err := s.Scan(&key, &at, &kind, &payload); err != nil {
				return err
			}
			p := parseTxPayload(payload)
			add(key, p.Description)
			if !successorKinds[kind] {
				return nil
			}
			m := histFromRe.FindStringSubmatch(strings.ToUpper(p.Action))
			if m != nil && m[1] != key {
				if _, seen := l.successors[m[1]]; !seen {
					l.successors[m[1]] = histSuccessor{key: key, at: at}
				}
			}
			return nil
		}); err != nil {
		return nil, fmt.Errorf("historicalLinks trades: %w", err)
	}
	if err := scanEach(ctx, c.db, `
SELECT as_of_date, account_external_id, instrument_key
  FROM historical_position_snapshots WHERE instrument_key IS NOT NULL`,
		func(s *sql.Rows) error {
			var h histHolding
			if err := s.Scan(&h.at, &h.acct, &h.key); err != nil {
				return err
			}
			l.held[h] = true
			return nil
		}); err != nil {
		return nil, fmt.Errorf("historicalLinks held: %w", err)
	}
	return l, nil
}

// scanEach runs q and hands each row to fn.
func scanEach(ctx context.Context, db *sql.DB, q string, fn func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// successorKinds are the corporate actions whose in-leg names the key
// whose shares it moved.
var successorKinds = map[string]bool{"MERGER": true, "CONVERSION": true, "NAME_CHANGE": true, "REVERSE_SPLIT": true}

// link is the key a keyless row of acct's statement of day at names, or
// "" for none. A link is recorded as held, so a second row of that day
// that names the same key keeps its own.
func (l *historicalLinks) link(at int64, acct, desc string) string {
	ticker := printedTicker(desc)
	key := ""
	if l.keys[ticker] {
		key = ticker
	} else if key = l.byName(strings.ToUpper(historicalDescription(desc)), ticker != ""); key != "" {
		key = l.successorOn(key, at)
	}
	if key == "" || l.held[histHolding{at, acct, key}] {
		return ""
	}
	l.held[histHolding{at, acct, key}] = true
	return key
}

// printedTicker is the last ticker desc prints in parentheses, "" for
// none.
func printedTicker(desc string) string {
	m := histTickerRe.FindAllStringSubmatch(desc, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

// byName is the key n names (link's second and third roads), "" where
// none or more than one security answers. exact keeps to the second.
func (l *historicalLinks) byName(n string, exact bool) string {
	if len(strings.Fields(n)) < histLinkMinWords {
		return ""
	}
	if keys := l.names[n]; len(keys) == 1 {
		for k := range keys {
			return k
		}
	}
	if exact {
		return ""
	}
	found := map[string]bool{}
	for name, keys := range l.names {
		if len(strings.Fields(name)) < histLinkMinWords ||
			!(strings.HasPrefix(name, n) || strings.HasPrefix(n, name)) {
			continue
		}
		for k := range keys {
			found[k] = true
		}
	}
	return l.chainHead(found)
}

// chainHead is the one key of keys from which a chain of corporate
// actions reaches every other, "" where there is none.
func (l *historicalLinks) chainHead(keys map[string]bool) string {
	for _, head := range slices.Sorted(maps.Keys(keys)) {
		reached := map[string]bool{head: true}
		for k := head; ; {
			s, ok := l.successors[k]
			if !ok || reached[s.key] {
				break
			}
			k = s.key
			reached[k] = true
		}
		all := true
		for k := range keys {
			all = all && reached[k]
		}
		if all {
			return head
		}
	}
	return ""
}

// successorOn follows key's corporate actions dated on or before at.
func (l *historicalLinks) successorOn(key string, at int64) string {
	for seen := map[string]bool{key: true}; ; {
		s, ok := l.successors[key]
		if !ok || s.at > at || seen[s.key] {
			return key
		}
		key = s.key
		seen[key] = true
	}
}
