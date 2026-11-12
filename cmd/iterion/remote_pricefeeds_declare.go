package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
	"github.com/spf13/cobra"
)

// `iterion remote price-feeds declare` — writes ONE declared feed of a
// scope's list: the table it is given, judged here by the parser the
// server applies, upserted into the list read from the server and written
// back by a CAS on that read, with the per-id diff printed.

var (
	remotePriceFeedsDeclareFile        string
	remotePriceFeedsDeclareModelsFile  string
	remotePriceFeedsDeclareEffortsFile string
	remotePriceFeedsDeclareFirst       bool
	remotePriceFeedsDeclareDryRun      bool
)

var remotePriceFeedsDeclareCmd = &cobra.Command{
	Use:   "declare <name> (--file F | --models-file M [--efforts-file E])",
	Short: "Declare one feed's table — prices, windows, accepted efforts — in the scope's list",
	Long: `Declare one feed of type declared in the scope's list: a table the
operator states — per gateway id, the prices, window, output cap, flags and
accepted reasoning efforts — read from the feed's config at every fold,
never fetched and never stale.

The table comes from either
  --file F          a feed-entry document (- reads stdin): the operator-table
                    JSON per id plus "reasoning_efforts", e.g.
                    {"glm-5.3":{"context_window":256000,"max_output_tokens":16384,
                      "input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15},
                     "scaleway/deepseek-v4.1-flash":{"reasoning_efforts":["low","high","xhigh","max"]}}
  --models-file M   a runner pool's OPENAI_COMPATIBLE_MODELS table, as written,
  --efforts-file E  and/or its OPENAI_COMPATIBLE_REASONING_EFFORTS table: each
                    id's efforts land beside its prices (the two tables'
                    spellings of an id match case-insensitively), and an id
                    only E names is declared by its efforts alone.

An entry states the price pair, or only reasoning_efforts: a window, an
output cap or a flag without the prices would never answer, and is refused.
The table becomes the feed's whole document.

declare reads the scope's list, replaces the named feed's document — its
place, switch and cadence kept — or appends a new enabled feed (--first puts
it ahead of the others: within one level, the first enabled feed stating an
id answers it), prints the per-id diff, and writes the list back on the stamp
it read: a concurrent write answers 409, and a re-run shows the new diff.
It refuses a feed of another type under the name, and another feed with a
param the server's view redacts (a token, a credentialed URL — written back,
the redaction would replace the value): replace the whole list with
` + "`iterion remote price-feeds set`" + ` there.

--dry-run prints the diff and the document, and writes nothing.`,
	Args: cobra.ExactArgs(1),
	RunE: remoteRunE(runRemotePriceFeedsDeclare),
}

// priceFeedDeclareResult is declare's answer: what it wrote, or would.
type priceFeedDeclareResult struct {
	Scope     string                  `json:"scope"`
	Feed      string                  `json:"feed"`
	Placement string                  `json:"placement"`
	Order     []string                `json:"order"`
	DryRun    bool                    `json:"dry_run"`
	Changes   []pricefeed.EntryChange `json:"changes,omitempty"`
	Unchanged int                     `json:"unchanged"`
	// StoredUnreadable is the refusal the replaced document met: its ids
	// read as absent in Changes.
	StoredUnreadable string          `json:"stored_unreadable,omitempty"`
	Document         json.RawMessage `json:"document"`
}

func runRemotePriceFeedsDeclare(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
	doc, err := priceFeedDeclaredDocument()
	if err != nil {
		return fmt.Errorf("price-feeds declare: %w", err)
	}
	declared := pricefeed.FeedConfig{
		Name: args[0], Type: pricefeed.TypeDeclared, Enabled: true,
		Interval: pricefeed.D(pricefeed.MaxInterval), Timeout: pricefeed.D(pricefeed.MinTimeout),
		Params: map[string]string{pricefeed.DeclaredParamEntries: doc},
	}
	if err := pricefeed.ValidateFeed(declared); err != nil {
		return fmt.Errorf("price-feeds declare: %w", err)
	}
	base, err := priceFeedsBase(cmd, c)
	if err != nil {
		return err
	}
	var view priceFeedsCLIView
	if _, err := c.Call(cmd.Context(), "GET", base, nil, &view); err != nil {
		return err
	}
	plan, err := planPriceFeedDeclaration(view, declared, remotePriceFeedsDeclareFirst)
	if err != nil {
		return fmt.Errorf("price-feeds declare: %w", err)
	}
	result := priceFeedDeclareResult{
		Scope: view.Scope, Feed: declared.Name, Placement: plan.placement, DryRun: remotePriceFeedsDeclareDryRun,
		Document: json.RawMessage(doc),
	}
	for _, f := range plan.feeds {
		result.Order = append(result.Order, f.Name)
	}
	if plan.oldErr != nil {
		result.StoredUnreadable = plan.oldErr.Error()
	}
	result.Changes = pricefeed.DiffEntries(plan.old, plan.next)
	result.Unchanged = len(plan.next) - countChanged(result.Changes, plan.next)

	if !remotePriceFeedsDeclareDryRun {
		stamp := time.Time{}
		if view.UpdatedAt != nil {
			stamp = *view.UpdatedAt
		}
		payload := struct {
			Feeds             []pricefeed.FeedConfig `json:"feeds"`
			ExpectedUpdatedAt time.Time              `json:"expected_updated_at"`
		}{plan.feeds, stamp}
		if _, err := c.Call(cmd.Context(), "PUT", base, payload, nil); err != nil {
			return err
		}
	}
	printPriceFeedDeclaration(p, result, plan.disabled)
	return nil
}

// countChanged counts the changes that touch an id of next — the ids a
// declaration states that moved or arrived.
func countChanged(changes []pricefeed.EntryChange, next map[string]modelspecs.Spec) int {
	n := 0
	for _, ch := range changes {
		if _, ok := next[ch.ID]; ok {
			n++
		}
	}
	return n
}

func printPriceFeedDeclaration(p *cli.Printer, r priceFeedDeclareResult, disabled bool) {
	if p.Format == cli.OutputJSON {
		p.JSON(r)
		return
	}
	p.Header(fmt.Sprintf("declare %s — %s", r.Feed, r.Scope))
	p.KV("feed", fmt.Sprintf("%s; order: %s", r.Placement, strings.Join(r.Order, ", ")))
	if disabled {
		p.KV("note", "the feed is disabled at this level: its declaration answers once it is enabled (price-feeds set)")
	}
	if r.StoredUnreadable != "" {
		p.KV("stored", "the replaced document does not parse ("+r.StoredUnreadable+"): its ids read as absent")
	}
	for _, ch := range r.Changes {
		p.KV("  "+ch.ID, fmt.Sprintf("%s → %s", priceEntrySummary(ch.Old), priceEntrySummary(ch.New)))
	}
	p.KV("unchanged", fmt.Sprintf("%d ids", r.Unchanged))
	if r.DryRun {
		p.Header("document (params." + pricefeed.DeclaredParamEntries + ")")
		p.Line("%s", string(r.Document))
		p.Line("dry run: nothing written")
		return
	}
	p.Line("price feeds: %s declared (scope %s)", r.Feed, r.Scope)
}

// priceFeedDeclaration is declare's plan: the list to write, the replaced
// document's entries (nil when the feed is new), the new ones, and where
// the declaration lands.
type priceFeedDeclaration struct {
	feeds     []pricefeed.FeedConfig
	old       map[string]modelspecs.Spec
	oldErr    error
	next      map[string]modelspecs.Spec
	placement string
	disabled  bool
}

// planPriceFeedDeclaration upserts declared into the scope's list as the
// view rendered it. The other feeds are written back as read, so a list
// holding a param the view redacts is refused: the redaction would be
// stored over the value.
func planPriceFeedDeclaration(view priceFeedsCLIView, declared pricefeed.FeedConfig, first bool) (priceFeedDeclaration, error) {
	var plan priceFeedDeclaration
	static, ok := staticSource(pricefeed.TypeDeclared)
	if !ok {
		return plan, fmt.Errorf("no %s source is registered in this binary", pricefeed.TypeDeclared)
	}
	next, err := static.Entries(declared.Params)
	if err != nil {
		return plan, err
	}
	plan.next = next
	at := -1
	for i, f := range view.Feeds {
		if f.Feed.Name == declared.Name {
			at = i
			continue
		}
		if len(f.RedactedParams) > 0 {
			return plan, fmt.Errorf("feed %q's params %s render redacted in the scope's view, and declare writes the other feeds back as read — the redaction would replace the stored value: replace the whole list with `iterion remote price-feeds set --file` instead",
				f.Feed.Name, strings.Join(f.RedactedParams, ", "))
		}
	}
	feeds := make([]pricefeed.FeedConfig, 0, len(view.Feeds)+1)
	for i, f := range view.Feeds {
		if i != at {
			feeds = append(feeds, f.Feed)
		}
	}
	if at >= 0 {
		stored := view.Feeds[at].Feed
		if stored.Type != pricefeed.TypeDeclared {
			return plan, fmt.Errorf("feed %q at this level is a %s feed, not a declaration — declare a new name, or replace the list with `iterion remote price-feeds set --file`", stored.Name, stored.Type)
		}
		plan.old, plan.oldErr = static.Entries(stored.Params)
		declared.Enabled, declared.Interval, declared.Timeout = stored.Enabled, stored.Interval, stored.Timeout
		plan.disabled = !stored.Enabled
	}
	switch {
	case first:
		feeds = slices.Insert(feeds, 0, declared)
		plan.placement = "moved first"
		if at < 0 {
			plan.placement = "new, first"
		}
	case at >= 0:
		feeds = slices.Insert(feeds, at, declared)
		plan.placement = "replaced in place"
	default:
		feeds = append(feeds, declared)
		plan.placement = "new, last"
	}
	plan.feeds = feeds
	return plan, nil
}

// staticSource answers the registered static source of a type.
func staticSource(typ string) (pricefeed.StaticSource, bool) {
	src, ok := pricefeed.SourceFor(typ)
	if !ok {
		return nil, false
	}
	static, ok := src.(pricefeed.StaticSource)
	return static, ok
}

// priceFeedDeclaredDocument builds the declared document from the input
// flags: --file's document, compacted, or the pool tables merged.
func priceFeedDeclaredDocument() (string, error) {
	if remotePriceFeedsDeclareFile != "" {
		var raw []byte
		var err error
		if remotePriceFeedsDeclareFile == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(remotePriceFeedsDeclareFile)
		}
		if err != nil {
			return "", err
		}
		what := "--file " + remotePriceFeedsDeclareFile
		if len(bytes.TrimSpace(raw)) == 0 {
			return "", fmt.Errorf("no document read from %s", what)
		}
		if _, err := pricefeed.ParseFeedEntries(string(raw), what); err != nil {
			return "", err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, bytes.TrimSpace(raw)); err != nil {
			return "", fmt.Errorf("%s: %w", what, err)
		}
		return compact.String(), nil
	}
	var models, efforts []byte
	var err error
	if remotePriceFeedsDeclareModelsFile != "" {
		if models, err = os.ReadFile(remotePriceFeedsDeclareModelsFile); err != nil {
			return "", err
		}
	}
	if remotePriceFeedsDeclareEffortsFile != "" {
		if efforts, err = os.ReadFile(remotePriceFeedsDeclareEffortsFile); err != nil {
			return "", err
		}
	}
	return poolTablesDocument(models, efforts, "--models-file "+remotePriceFeedsDeclareModelsFile, "--efforts-file "+remotePriceFeedsDeclareEffortsFile)
}

// poolTablesDocument merges a runner pool's two env tables into one
// declared document. models is the OPENAI_COMPATIBLE_MODELS table, judged
// by the parser the runner decodes it with (pricefeed.ParseEntries);
// efforts is the OPENAI_COMPATIBLE_REASONING_EFFORTS table, each list read
// through the effort vocabulary — strictly here, where the runner skips a
// bad entry: a write states what it means. Every entry keeps its JSON as
// written, an id's efforts landing in its models entry when both tables
// spell the id alike once normalized.
func poolTablesDocument(models, efforts []byte, modelsWhat, effortsWhat string) (string, error) {
	merged := map[string]map[string]json.RawMessage{}
	keyOf := map[string]string{} // normalized id → its key in merged
	if len(bytes.TrimSpace(models)) > 0 {
		if _, err := pricefeed.ParseEntries(string(models), modelsWhat); err != nil {
			return "", err
		}
		var table map[string]map[string]json.RawMessage
		if err := json.Unmarshal(models, &table); err != nil {
			return "", fmt.Errorf("%s does not parse: %w", modelsWhat, err)
		}
		for id, fields := range table {
			if fields == nil {
				fields = map[string]json.RawMessage{}
			}
			merged[id] = fields
			keyOf[pricefeed.NormalizeID(id)] = id
		}
	}
	if len(bytes.TrimSpace(efforts)) > 0 {
		table, err := parsePoolEfforts(efforts, effortsWhat)
		if err != nil {
			return "", err
		}
		ids := make([]string, 0, len(table))
		for id := range table {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			key, known := keyOf[pricefeed.NormalizeID(id)]
			if !known {
				merged[id] = map[string]json.RawMessage{}
				keyOf[pricefeed.NormalizeID(id)] = id
				key = id
			}
			if _, dup := merged[key]["reasoning_efforts"]; dup {
				return "", fmt.Errorf("%s: id %q collides with another spelling of %q after normalization — keep one spelling", effortsWhat, id, pricefeed.NormalizeID(id))
			}
			merged[key]["reasoning_efforts"] = table[id]
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parsePoolEfforts decodes an OPENAI_COMPATIBLE_REASONING_EFFORTS table —
// one JSON object, id → effort list — refusing an empty id and a list the
// effort vocabulary refuses, each named.
func parsePoolEfforts(raw []byte, what string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var table map[string]json.RawMessage
	if err := dec.Decode(&table); err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", what, err)
	}
	if table == nil {
		return nil, fmt.Errorf("%s must be a JSON object of effort lists", what)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%s carries content after the table", what)
	}
	ids := make([]string, 0, len(table))
	for id := range table {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%s: an empty model id", what)
		}
		if _, err := modelspecs.ParseEffortLevels(table[id]); err != nil {
			return nil, fmt.Errorf("%s[%q]: %w", what, id, err)
		}
	}
	return table, nil
}

func init() {
	f := remotePriceFeedsDeclareCmd.Flags()
	f.StringVar(&remotePriceFeedsDeclareFile, "file", "", "Path to the declared document (feed-entry JSON; - for stdin)")
	f.StringVar(&remotePriceFeedsDeclareModelsFile, "models-file", "", "Path to a pool's OPENAI_COMPATIBLE_MODELS table")
	f.StringVar(&remotePriceFeedsDeclareEffortsFile, "efforts-file", "", "Path to a pool's OPENAI_COMPATIBLE_REASONING_EFFORTS table")
	f.BoolVar(&remotePriceFeedsDeclareFirst, "first", false, "Place the declaration ahead of the level's other feeds")
	f.BoolVar(&remotePriceFeedsDeclareDryRun, "dry-run", false, "Print the diff and the document; write nothing")
	remotePriceFeedsDeclareCmd.MarkFlagsMutuallyExclusive("file", "models-file")
	remotePriceFeedsDeclareCmd.MarkFlagsMutuallyExclusive("file", "efforts-file")
	remotePriceFeedsDeclareCmd.MarkFlagsOneRequired("file", "models-file", "efforts-file")
	addPriceFeedsScopeFlags(remotePriceFeedsDeclareCmd)
	remotePriceFeedsCmd.AddCommand(remotePriceFeedsDeclareCmd)
}
