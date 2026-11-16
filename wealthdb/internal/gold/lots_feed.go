package gold

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// The lot engine's feed: gold rows into lots.Events (docs/LOTS.md §3).
// One generic reader serves every source. A transaction reads by the
// action its adapter states, else by its kind (lotClassify); the source
// kind's registered lots.Policy holds the rest of what the engine knows
// of its feed.

type lotFeed struct {
	cfg     lots.Config
	sources map[string]*lotSource
	ids     []string
	pid     map[[2]string]string
	never   map[[2]string]bool
	rates   *lotRates

	keys  []lots.KeySpec
	info  []lotKeyInfo
	keyOf map[lotKeyRef]int32
	// trades lists each key account's transactions, in time order.
	trades map[[2]string][]lotTrade
	// walletTrades lists each pooled key's trades per account, in time
	// order.
	walletTrades map[lotWalletRef][]lotWalletTrade

	events []lots.Event
	aux    []lotAux
	// transits numbers the paired transfers (lots.Event.Transit).
	transits int32
	legs     []lotLeg
	// links names each leg's partner where a transfer_links row of
	// measure in_kind states it, keyed by (source, transaction).
	links map[[2]string][2]string
	corp  []lotCorpLeg

	// targets are the rows each observation writes to.
	targets []lotObs
	// documented marks a transaction in a year its account's documents
	// cover: the engine's realized lots for it are not primary.
	documented map[[2]string]bool
	// findings are the feed's own, made outside the replay.
	findings []lots.Finding
	skipped  map[string]int
	// stated lists the sources in stated mode, which the replay leaves
	// alone: their sales realize against the stated basis in SQL
	// (lotWriter.writeStated).
	stated    []string
	statedPol map[string]lots.Policy
}

// newLotFeed reads which sources a pass replays: every source whose
// policy is fill or shadow. A move links sources, so a pass replays them
// all together. A stated source is noted for the writer.
func newLotFeed(ctx context.Context, db *sql.DB, cfg lots.Config) (*lotFeed, error) {
	f := &lotFeed{
		cfg:          cfg,
		sources:      map[string]*lotSource{},
		pid:          map[[2]string]string{},
		never:        map[[2]string]bool{},
		keyOf:        map[lotKeyRef]int32{},
		trades:       map[[2]string][]lotTrade{},
		walletTrades: map[lotWalletRef][]lotWalletTrade{},
		documented:   map[[2]string]bool{},
		skipped:      map[string]int{},
		statedPol:    map[string]lots.Policy{},
		links:        map[[2]string][2]string{},
	}
	return f, f.readSources(ctx, db)
}

// all lists the replayed and the stated sources: every source a pass
// writes a run for.
func (f *lotFeed) all() []string { return append(append([]string{}, f.ids...), f.stated...) }

// policy is the policy a pass applies to source id.
func (f *lotFeed) policy(id string) lots.Policy {
	if s, ok := f.sources[id]; ok {
		return s.pol
	}
	return f.statedPol[id]
}

// read reads the replayed sources' rows into events.
func (f *lotFeed) read(ctx context.Context, db *sql.DB) error {
	if len(f.ids) == 0 {
		return nil
	}
	for _, step := range []func(context.Context, *sql.DB) error{
		f.readPortfolios, f.readClasses, f.readRates, f.readTransactions, f.readLinks,
		f.readRealized, f.readDocumented, f.readPositions, f.readAnchors,
	} {
		if err := step(ctx, db); err != nil {
			return err
		}
	}
	f.pairTransfers()
	f.corporateActions()
	f.resolveValues()
	return nil
}

func (f *lotFeed) add(ev lots.Event, aux lotAux) {
	f.events = append(f.events, ev)
	f.aux = append(f.aux, aux)
}
