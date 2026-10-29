package discovery

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Corpus aggregates several runs into the numbers a report states, plus
// the coverage that says how much of the corpus those numbers actually
// saw. Every ratio here has its denominator next to it on purpose: a
// share computed over three classified calls is not a finding.
type Corpus struct {
	Runs            int `json:"runs"`
	RunsWithTooling int `json:"runs_with_tooling"`
	Nodes           int `json:"nodes"`

	Calls   int           `json:"calls"`
	ByClass map[Class]int `json:"by_class"`
	// BeforeMutation counts only the calls that ran before their node's
	// first mutating call — the orientation phase, where the stream
	// supports a boundary.
	BeforeMutation      map[Class]int `json:"before_mutation"`
	CallsBeforeMutation int           `json:"calls_before_mutation"`

	// PureDiscoveryNodes never mutated anything: their whole token spend
	// is orientation cost, attributed with no imputation. MutatingNodes
	// wrote at least once, so their spend is NOT split here.
	PureDiscoveryNodes  int `json:"pure_discovery_nodes"`
	MutatingNodes       int `json:"mutating_nodes"`
	IdleNodes           int `json:"idle_nodes"`
	PureDiscoveryTokens int `json:"pure_discovery_tokens"`
	MutatingTokens      int `json:"mutating_tokens"`
	NodesWithoutTokens  int `json:"nodes_without_tokens"`
	// The token columns and the node columns have DIFFERENT populations:
	// most pure-orientation nodes are `tool` nodes, which spend nothing.
	// Dividing a token total by a node count that includes them reads
	// nine times too low, in the direction that would kill the feature
	// this measurement exists to arbitrate — so both counts are carried.
	PureDiscoveryNodesWithTokens int `json:"pure_discovery_nodes_with_tokens"`
	MutatingNodesWithTokens      int `json:"mutating_nodes_with_tokens"`
	// IdleTokens is spend recorded on a node that called no tool at all.
	// It belongs to neither side of the boundary and is stated rather
	// than dropped.
	IdleTokens int `json:"idle_tokens"`
	// The within-node split (#1481), over the MUTATING nodes: the spend
	// their turns recorded before the node's first mutating call
	// (orientation) and from it on (work). MutatingSplitKnownNodes counts
	// the mutating nodes whose turns carried usage at all — the split's
	// coverage.
	SplitBeforeTokens       int `json:"split_before_tokens"`
	SplitAfterTokens        int `json:"split_after_tokens"`
	MutatingSplitKnownNodes int `json:"mutating_split_known_nodes"`
	// StartedNotFinished counts calls that opened and never completed,
	// across the corpus. Excluded from every other count.
	StartedNotFinished int `json:"started_not_finished"`

	InputBytes  int   `json:"input_bytes"`
	OutputBytes int   `json:"output_bytes"`
	DurationMs  int64 `json:"duration_ms"`
	ElapsedMs   int64 `json:"elapsed_ms"`
	// Every sum above carries the count of calls that contributed one.
	// Without them, `DurationMs` reads as the corpus's tool time when it
	// is the tool time of the 6 % of calls a backend bothered to time —
	// a figure this report published, wrong by 6.7×.
	CallsWithInput    int `json:"calls_with_input"`
	CallsWithOutput   int `json:"calls_with_output"`
	CallsWithDuration int `json:"calls_with_duration"`
	CallsWithElapsed  int `json:"calls_with_elapsed"`
	// CompletionsWithoutStart is what the per-node dedup discarded. It is
	// the engine's duplicate emissions while every start is present, and
	// silently-dropped real calls the moment one is not.
	CompletionsWithoutStart int `json:"completions_without_start"`

	// TopVerbs ranks the shell verbs seen; UnknownVerbs ranks the subset
	// the table could not name, which is the list that says what the
	// measurement is still guessing at.
	TopVerbs     map[string]int `json:"top_verbs,omitempty"`
	UnknownVerbs map[string]int `json:"unknown_verbs,omitempty"`

	// Reconciliation is the corpus's view of UsageFeeds. Every other
	// number in this report describes a corpus and moves with it; this
	// one states an invariant, which is why it is the part of the
	// measurement a gate can hold.
	Reconciliation Reconciliation `json:"reconciliation"`
}

// Reconciliation aggregates the per-node comparison of the two usage
// recordings (see UsageFeeds).
type Reconciliation struct {
	Steps                int `json:"steps"`
	StepsWithoutUsage    int `json:"steps_without_usage"`
	StepsUsageUnreported int `json:"steps_usage_unreported"`
	StepsOutsideANode    int `json:"steps_outside_a_node"`
	NodesWithSteps       int `json:"nodes_with_steps"`
	// ReconcilableNodes is the comparison's coverage: nodes where both
	// recordings carry usage and the checkpoints reported a real split.
	// UnsplitAggregateNodes are excluded from it and stated apart — a
	// backend that could not split its total leaves no field-by-field
	// baseline, which is a limit of the recording, not a gap.
	ReconcilableNodes     int `json:"reconcilable_nodes"`
	ReconciledNodes       int `json:"reconciled_nodes"`
	NodesWithGap          int `json:"nodes_with_gap"`
	UnsplitAggregateNodes int `json:"unsplit_aggregate_nodes"`
	// The gap totals sum MAGNITUDES, so a node short by 500 and a node
	// over by 500 do not cancel into "the corpus reconciles".
	GapInputTokens  int `json:"gap_input_tokens"`
	GapOutputTokens int `json:"gap_output_tokens"`
	// The cache fields the feed carries and the checkpoint has no column
	// for: reported, never reconciled.
	StepCacheReadTokens  int `json:"step_cache_read_tokens"`
	StepCacheWriteTokens int `json:"step_cache_write_tokens"`
}

// fold adds one node's feeds to the corpus comparison.
func (r *Reconciliation) fold(u UsageFeeds) {
	r.Steps += u.Steps
	r.StepsWithoutUsage += u.Steps - u.StepsWithUsage
	r.StepsUsageUnreported += u.StepsUsageUnreported
	r.StepCacheReadTokens += u.StepCacheReadTokens
	r.StepCacheWriteTokens += u.StepCacheWriteTokens
	if u.Steps > 0 {
		r.NodesWithSteps++
	}
	if u.StepsWithUsage > 0 && u.TurnAggregateTokens > 0 {
		r.UnsplitAggregateNodes++
	}
	if !u.Reconcilable {
		return
	}
	r.ReconcilableNodes++
	if u.Reconciled() {
		r.ReconciledNodes++
		return
	}
	r.NodesWithGap++
	r.GapInputTokens += abs(u.GapInputTokens)
	r.GapOutputTokens += abs(u.GapOutputTokens)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Aggregate folds run profiles into a corpus.
func Aggregate(profiles []*RunProfile) Corpus {
	c := Corpus{
		ByClass:        map[Class]int{},
		BeforeMutation: map[Class]int{},
		TopVerbs:       map[string]int{},
		UnknownVerbs:   map[string]int{},
	}
	for _, p := range profiles {
		if p == nil {
			continue
		}
		c.Runs++
		c.StartedNotFinished += p.StartedNotFinished
		c.CompletionsWithoutStart += p.CompletionsWithoutStart
		c.Reconciliation.StepsOutsideANode += p.StepsOutsideANode
		if len(p.Nodes) > 0 {
			hadCalls := false
			for _, n := range p.Nodes {
				if n.Calls > 0 {
					hadCalls = true
					break
				}
			}
			if hadCalls {
				c.RunsWithTooling++
			}
		}
		for _, n := range p.Nodes {
			c.Nodes++
			c.Reconciliation.fold(n.Usage)
			c.Calls += n.Calls
			c.CallsBeforeMutation += n.CallsBeforeMutation
			c.InputBytes += n.InputBytes
			c.OutputBytes += n.OutputBytes
			c.DurationMs += n.DurationMs
			c.ElapsedMs += n.ElapsedMs
			c.CallsWithInput += n.CallsWithInput
			c.CallsWithOutput += n.CallsWithOutput
			c.CallsWithDuration += n.CallsWithDuration
			c.CallsWithElapsed += n.CallsWithElapsed
			for k, v := range n.ByClass {
				c.ByClass[k] += v
			}
			for k, v := range n.BeforeMutation {
				c.BeforeMutation[k] += v
			}
			for k, v := range n.Verbs {
				c.TopVerbs[k] += v
			}
			for k, v := range n.UnknownVerbs {
				c.UnknownVerbs[k] += v
			}
			switch {
			case n.Mutated:
				c.MutatingNodes++
				if n.TokensKnown {
					c.MutatingTokens += n.Tokens
					c.MutatingNodesWithTokens++
				}
				if n.TokensSplitKnown {
					c.SplitBeforeTokens += n.TokensBeforeMutation
					c.SplitAfterTokens += n.TokensAfterMutation
					c.MutatingSplitKnownNodes++
				}
			case n.Calls > 0:
				c.PureDiscoveryNodes++
				if n.TokensKnown {
					c.PureDiscoveryTokens += n.Tokens
					c.PureDiscoveryNodesWithTokens++
				}
			default:
				c.IdleNodes++
				if n.TokensKnown {
					c.IdleTokens += n.Tokens
				}
			}
			if !n.TokensKnown {
				c.NodesWithoutTokens++
			}
		}
	}
	return c
}

// AttributableTokens is the spend this corpus can place on one side of
// the boundary or the other. Ratios must use this, never a total that
// includes nodes whose spend nobody recorded.
func (c Corpus) AttributableTokens() int { return c.PureDiscoveryTokens + c.MutatingTokens }

// ClassifiedShare is the fraction of calls the table could name. It is
// the honesty dial of the whole report: a low share means the other
// numbers describe a minority of the activity.
func (c Corpus) ClassifiedShare() float64 {
	if c.Calls == 0 {
		return 0
	}
	return float64(c.Calls-c.ByClass[ClassUnknown]) / float64(c.Calls)
}

// RenderOptions parameterises the markdown rendering.
type RenderOptions struct {
	Title       string
	GeneratedAt time.Time
	StoreLabel  string
	TopN        int // per-node rows to show; 0 → 15
}

// RenderMarkdown writes the corpus as a report. Coverage comes FIRST:
// a reader who stops after the first table must still know what the
// numbers below it cannot see.
func RenderMarkdown(c Corpus, profiles []*RunProfile, opts RenderOptions) string {
	title := opts.Title
	if title == "" {
		title = "Discovery cost"
	}
	topN := opts.TopN
	if topN <= 0 {
		topN = 15
	}
	var b strings.Builder

	fmt.Fprintf(&b, "# %s\n\n", title)
	if !opts.GeneratedAt.IsZero() {
		fmt.Fprintf(&b, "Generated %s", opts.GeneratedAt.UTC().Format(time.RFC3339))
		if opts.StoreLabel != "" {
			fmt.Fprintf(&b, " from `%s`", opts.StoreLabel)
		}
		b.WriteString(".\n\n")
	}

	b.WriteString("## What this can and cannot see\n\n")
	b.WriteString("| Coverage | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Runs read | %d |\n", c.Runs)
	fmt.Fprintf(&b, "| Runs that called a tool | %d |\n", c.RunsWithTooling)
	fmt.Fprintf(&b, "| Nodes | %d (%d never called a tool) |\n", c.Nodes, c.IdleNodes)
	fmt.Fprintf(&b, "| Tool calls classified | %d of %d (%.0f%%) |\n",
		c.Calls-c.ByClass[ClassUnknown], c.Calls, 100*c.ClassifiedShare())
	fmt.Fprintf(&b, "| Nodes with no recorded token spend | %d of %d |\n", c.NodesWithoutTokens, c.Nodes)
	fmt.Fprintf(&b, "| Calls that opened and never completed | %d (excluded from every count below) |\n", c.StartedNotFinished)
	b.WriteString("\n")
	// The within-node split (#1481): turn-granular, mutating nodes only.
	fmt.Fprintf(&b, "## Within-node split (mutating nodes, turn-granular)\n\n")
	if c.MutatingSplitKnownNodes > 0 {
		fmt.Fprintf(&b, "| Phase | Tokens |\n|---|---:|\n")
		fmt.Fprintf(&b, "| Orientation (before the node's first write) | %d |\n", c.SplitBeforeTokens)
		fmt.Fprintf(&b, "| Work (the first write's turn and after) | %d |\n", c.SplitAfterTokens)
		share := 0.0
		if total := c.SplitBeforeTokens + c.SplitAfterTokens; total > 0 {
			share = 100 * float64(c.SplitBeforeTokens) / float64(total)
		}
		fmt.Fprintf(&b, "\nOrientation is %.0f%% of the split spend across %d mutating nodes with recorded turns. A turn mixing reads and a write counts whole to work — turn granularity is the finest the checkpoints support.\n", share, c.MutatingSplitKnownNodes)
	} else {
		b.WriteString("No mutating node recorded turn usage: the split has nothing to attribute.\n")
	}
	b.WriteString("\n")
	renderReconciliation(&b, c, profiles, topN)

	b.WriteString("## Tool calls\n\n")
	b.WriteString("| Class | All calls | Before the node's first write |\n|---|---:|---:|\n")
	for _, k := range []Class{ClassDiscovery, ClassMutation, ClassOther, ClassUnknown} {
		fmt.Fprintf(&b, "| %s | %d | %d |\n", k, c.ByClass[k], c.BeforeMutation[k])
	}
	fmt.Fprintf(&b, "| **total** | **%d** | **%d** |\n\n", c.Calls, c.CallsBeforeMutation)
	cov := func(n int) string {
		if c.Calls == 0 {
			return "no calls"
		}
		return fmt.Sprintf("%d of %d calls", n, c.Calls)
	}
	dur := func(ms int64) string {
		return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
	}
	fmt.Fprintf(&b, "Bytes pulled in by tool inputs: %s — %s · returned by outputs: %s — %s.\n\n",
		humanBytes(c.InputBytes), cov(c.CallsWithInput),
		humanBytes(c.OutputBytes), cov(c.CallsWithOutput))

	// Two instruments, two lines, never one number. The backend times
	// its own `tool` nodes and nothing else — the streaming path builds
	// its call info without a duration at all — while the event stream
	// times every call from its own two timestamps. Adding them would
	// sum a measurement to an absence, and publishing only the first
	// labelled "tool wall time" understated this corpus by 6.7×.
	fmt.Fprintf(&b, "Tool time, backend-measured: %s — %s · event-stream elapsed: %s — %s.\n\n",
		dur(c.DurationMs), cov(c.CallsWithDuration),
		dur(c.ElapsedMs), cov(c.CallsWithElapsed))

	if c.CompletionsWithoutStart > 0 {
		fmt.Fprintf(&b, "Completions with no open start: %d — the engine emits two per `tool` "+
			"node and the second is dropped. A number larger than the `tool`-node call count "+
			"would mean starts went missing and real calls were discarded with them.\n\n",
			c.CompletionsWithoutStart)
	}

	b.WriteString("## Tokens, where they can be attributed\n\n")
	att := c.AttributableTokens()
	b.WriteString("| Node kind | Nodes | of which carry a spend | Tokens |\n|---|---:|---:|---:|\n")
	fmt.Fprintf(&b, "| never mutated (pure orientation) | %d | %d | %d |\n",
		c.PureDiscoveryNodes, c.PureDiscoveryNodesWithTokens, c.PureDiscoveryTokens)
	fmt.Fprintf(&b, "| mutated at least once (not split) | %d | %d | %d |\n",
		c.MutatingNodes, c.MutatingNodesWithTokens, c.MutatingTokens)
	fmt.Fprintf(&b, "| **attributable total** | | | **%d** |\n\n", att)
	b.WriteString("The middle column is the one to divide by: most pure-orientation " +
		"nodes are `tool` nodes, which call a command and spend no tokens at all. " +
		"Dividing by the first column reads nine times too low.\n\n")
	if c.IdleTokens > 0 {
		fmt.Fprintf(&b, "A further %d tokens sit on %d node(s) that called no tool — "+
			"neither orientation nor change, and excluded from the total above.\n\n",
			c.IdleTokens, c.IdleNodes)
	}
	if att > 0 {
		fmt.Fprintf(&b, "**%.1f%% of the attributable tokens were spent by nodes that never wrote a byte.**\n\n",
			100*float64(c.PureDiscoveryTokens)/float64(att))
	} else {
		b.WriteString("No node in this corpus carried a recorded token spend — " +
			"the share is undefined, not zero.\n\n")
	}

	if len(c.TopVerbs) > 0 {
		b.WriteString("## Shell verbs seen\n\n")
		writeVerbList(&b, c.TopVerbs, topN)
	}
	if len(c.UnknownVerbs) > 0 {
		b.WriteString("## Verbs the classifier could not name\n\n")
		b.WriteString("Every call below sits in the `unknown` row above. " +
			"The list is the table's own to-do — and its length is how much " +
			"of this measurement is still guesswork.\n\n")
		writeVerbList(&b, c.UnknownVerbs, topN)
	}

	b.WriteString("## Busiest nodes\n\n")
	b.WriteString("| Run | Node | Kind | Calls | Discovery | Before 1st write | Tokens |\n|---|---|---|---:|---:|---:|---:|\n")
	type row struct {
		run string
		n   NodeProfile
	}
	var rows []row
	for _, p := range profiles {
		if p == nil {
			continue
		}
		for _, n := range p.Nodes {
			if n.Calls > 0 {
				rows = append(rows, row{p.RunID, n})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].n.Calls > rows[j].n.Calls })
	if len(rows) > topN {
		rows = rows[:topN]
	}
	for _, r := range rows {
		tok := "—"
		if r.n.TokensKnown {
			tok = fmt.Sprintf("%d", r.n.Tokens)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %d | %d | %d | %s |\n",
			shortID(r.run), r.n.NodeID, dash(r.n.Kind), r.n.Calls,
			r.n.ByClass[ClassDiscovery], r.n.CallsBeforeMutation, tok)
	}
	b.WriteString("\n")
	return b.String()
}

// writeVerbList renders a verb ranking, ties broken alphabetically so
// two runs of the same corpus produce byte-identical reports.
func writeVerbList(b *strings.Builder, verbs map[string]int, topN int) {
	type kv struct {
		k string
		v int
	}
	rows := make([]kv, 0, len(verbs))
	for k, v := range verbs {
		rows = append(rows, kv{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].v != rows[j].v {
			return rows[i].v > rows[j].v
		}
		return rows[i].k < rows[j].k
	})
	if len(rows) > topN {
		rows = rows[:topN]
	}
	for _, r := range rows {
		fmt.Fprintf(b, "- `%s` — %d\n", r.k, r.v)
	}
	b.WriteString("\n")
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// renderReconciliation writes the comparison of the two usage recordings
// and names every node whose recordings disagree. A corpus-level "they
// reconcile" would hide the one node that does not, which is the only
// thing this section exists to find.
func renderReconciliation(b *strings.Builder, c Corpus, profiles []*RunProfile, topN int) {
	r := c.Reconciliation
	b.WriteString("## Token reconciliation: the per-step feed against the turn checkpoints\n\n")
	if r.Steps == 0 {
		b.WriteString("No run in this corpus recorded a per-step usage feed: there is nothing to reconcile, and no claim is made that the producing sites are honoured.\n\n")
		return
	}
	b.WriteString("Two independent recordings of the same spend: one `llm_step_finished` event per model message, against the turn checkpoints the node writes. A producing site that stops emitting — or that emits a zero-valued usage struct — shows up as a gap on its node, where counting events stays green. A gap is the CHECKPOINT minus the FEED, so a positive one means the feed is short and a negative one means it recorded the same spend twice. A node whose backend writes only one of the two recordings is not comparable and claims nothing.\n\n")
	b.WriteString("| Reconciliation | Value |\n|---|---|\n")
	fmt.Fprintf(b, "| Steps recorded | %d, on %d of the %d nodes |\n", r.Steps, r.NodesWithSteps, c.Nodes)
	fmt.Fprintf(b, "| Steps carrying no usage field at all | %d of %d |\n", r.StepsWithoutUsage, r.Steps)
	fmt.Fprintf(b, "| Steps whose producer reported no usage (`usage_unreported`) | %d of %d |\n", r.StepsUsageUnreported, r.Steps)
	fmt.Fprintf(b, "| Steps naming no node (a supervised session has none) | %d |\n", r.StepsOutsideANode)
	fmt.Fprintf(b, "| Nodes where both recordings carry a split usage | %d of %d |\n", r.ReconcilableNodes, c.Nodes)
	fmt.Fprintf(b, "| Nodes that reconcile field by field | %d of %d comparable |\n", r.ReconciledNodes, r.ReconcilableNodes)
	fmt.Fprintf(b, "| Nodes whose recordings disagree | %d (input %d, output %d tokens apart, magnitudes) |\n",
		r.NodesWithGap, r.GapInputTokens, r.GapOutputTokens)
	fmt.Fprintf(b, "| Nodes with a feed whose checkpoints carry an unsplit total | %d (no field-by-field baseline) |\n", r.UnsplitAggregateNodes)
	fmt.Fprintf(b, "\nThe feed's cache fields are carried and never reconciled — the checkpoint has no column for them: %d read, %d written.\n\n",
		r.StepCacheReadTokens, r.StepCacheWriteTokens)

	if r.NodesWithGap == 0 {
		return
	}
	b.WriteString("### Nodes whose two recordings disagree\n\n")
	b.WriteString("| Run | Node | Steps | Turns | Input gap | Output gap |\n|---|---|---:|---:|---:|---:|\n")
	shown := 0
	for _, p := range profiles {
		if p == nil {
			continue
		}
		for _, n := range p.Nodes {
			if n.Usage.Reconciled() || !n.Usage.Reconcilable {
				continue
			}
			if shown == topN {
				fmt.Fprintf(b, "\n%d more node(s) disagree; raise `--top` to see them.\n", r.NodesWithGap-shown)
				b.WriteString("\n")
				return
			}
			fmt.Fprintf(b, "| `%s` | %s | %d | %d | %+d | %+d |\n",
				shortID(p.RunID), n.NodeID, n.Usage.Steps, n.Usage.Turns,
				n.Usage.GapInputTokens, n.Usage.GapOutputTokens)
			shown++
		}
	}
	b.WriteString("\n")
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
