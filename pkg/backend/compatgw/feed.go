package compatgw

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The gateway price feed's read side (ADR-163): the server folds the
// effective table for a run — fresh feed entries first, the operator
// floor below — and the table travels with the run, planted on its
// environment under ONE name this package reads between the host IPC
// record and the operator table. A fresh feed entry answers before the
// operator table for the ids it carries; a stale one steps down with a
// warning, never a refusal; and the record the run is stamped with
// names the feed and its age.

// FeedTableEnv carries the run's effective price-feed table. The server
// seals it in the run bundle; the runner plants it on the run's
// environment (credroute overlay); the sandbox crossing forwards it
// explicitly beside the IPC record. Read through catalogEnv like the
// other three names: a value a project `.env` planted prices nothing.
const FeedTableEnv = "ITERION_GATEWAY_PRICE_FEED"

// SourceFeedPrefix marks a provenance string that answered from a named
// price feed — `feed:<name>` — so the `_gateway_spec` record says WHICH
// feed priced the run, not just WHICH table kind.
const SourceFeedPrefix = "feed:"

// SourceFeed builds a feed's provenance string.
func SourceFeed(name string) string { return SourceFeedPrefix + name }

// FeedSource splits a feed provenance string; ok reports the prefix.
func FeedSource(source string) (name string, ok bool) {
	name, ok = strings.CutPrefix(source, SourceFeedPrefix)
	return name, ok && name != ""
}

// feedTableEntry is one parsed feed table, memoised by the raw value it
// was parsed from — the operator table's discipline: parse once per
// value, whatever the reader's provenance. Nothing else rides the entry:
// the table's ANSWER is its two maps, ENTRIES and EFFORTS. Freshness was
// judged once, at the FOLD, by the server (ADR-163 §1: "the server
// resolves the effective table (feed entry if fresh, else the operator
// floor)") — the entries that travel are the entries that answered, and
// every reader of the value (dispatch stamp, meter, crossing, a re-parse
// after eviction) reads the same answer with no clock of its own. That is
// ADR-122's "the pricing tables are never read again in mid-flight" held
// at the feed rung: no reader can re-judge, so no eviction, no queue
// delay and no per-runner clock can change one invocation's price (the
// loop's rounds and the gate's verdict converged here).
type feedTableEntry struct {
	table *pricefeed.Table
}

// feedTableMemoBound caps the memoised PARSES: every launch folds a
// fresh value (refreshed_at moves), so an unbounded memo is one parsed
// table retained per run a long-lived runner has served. Eviction is
// harmless — a re-parse answers the same entries (presence, not
// clock), so the bound only bounds memory.
const feedTableMemoBound = 32

var (
	feedTableMu   sync.Mutex
	feedTables    = map[string]*feedTableEntry{}
	feedTablesSeq []string
)

// feedTableLookup answers one gateway id (already normalized) from the
// run's feed table (feedTable). PRESENCE IS THE VERDICT: the fold sent
// these entries because they were fresh when the server folded; the
// invocation answers from them until it ends, and no reader consults a
// clock.
func feedTableLookup(get func(string) string, id string) *pricefeed.Entry {
	table := feedTable(get)
	if table == nil {
		return nil
	}
	if entry, here := table.Entries[id]; here {
		return &entry
	}
	return nil
}

// feedTable is the run's parsed feed table — memoised per raw value,
// planted values refused (catalogEnv), a wrong-shaped value silently no
// table. The price rung (feedTableLookup) and the effort rung
// (AcceptedEfforts) read through it.
func feedTable(get func(string) string) *pricefeed.Table {
	raw := catalogEnv(get, FeedTableEnv)
	if raw == "" {
		return nil
	}
	feedTableMu.Lock()
	e, ok := feedTables[raw]
	if !ok {
		e = &feedTableEntry{table: pricefeed.ParseTable(raw)}
		feedTables[raw] = e
		feedTablesSeq = append(feedTablesSeq, raw)
		if len(feedTablesSeq) > feedTableMemoBound {
			oldest := feedTablesSeq[0]
			feedTablesSeq = feedTablesSeq[1:]
			delete(feedTables, oldest)
		}
	}
	feedTableMu.Unlock()
	return e.table
}

// ResetFeedTableCache drops the memoised feed tables — tests only; a
// production process reads its run's table once per value and the memo
// is bounded.
func ResetFeedTableCache() {
	feedTableMu.Lock()
	feedTables = map[string]*feedTableEntry{}
	feedTablesSeq = nil
	feedTableMu.Unlock()
}

// OperatorTable reads the operator's table through the ONE parser — the
// server's drift report reads the same OPENAI_COMPATIBLE_MODELS the
// resolution answers from, planted filter included, so a `.env` table
// drifts against nothing rather than against a table nobody serves.
func OperatorTable(get func(string) string) (map[string]modelspecs.Spec, error) {
	return pricefeed.ParseEntries(catalogEnv(get, CatalogModelsEnv), CatalogModelsEnv)
}

// OverlayFeedTable plants a feed table's wire form on a context (the
// runner's bundle → context step). A malformed payload answers the
// context unchanged: the run keeps the local sources rather than a
// table this process cannot read. This is the ONE bundle-to-context
// adapter, shared by the runner and the suites, so the sealed wire shape
// and the planted one cannot drift apart.
func OverlayFeedTable(ctx context.Context, raw []byte) context.Context {
	if len(raw) == 0 {
		return ctx
	}
	if pricefeed.ParseTable(string(raw)) == nil {
		return ctx
	}
	return credroute.WithEnvOverlay(ctx, FeedTableEnv, string(raw))
}

// FeedTableJSON answers the run's planted table as its wire bytes — the
// crossing's forward and the suites' assertions read the same value the
// catalog answers from.
func FeedTableJSON(get func(string) string) json.RawMessage {
	return json.RawMessage(catalogEnv(get, FeedTableEnv))
}
