package discovery

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// step seeds one `llm_step_finished` event as a producing site writes it.
// usage nil means the site emitted the event and no usage at all.
func step(t *testing.T, s store.RunStore, runID, nodeID string, usage map[string]any) {
	t.Helper()
	data := map[string]any{"step": 1, "finish_reason": "stop"}
	for k, v := range usage {
		data[k] = v
	}
	appendEvent(t, s, runID, store.Event{
		Type: store.EventLLMStepFinished, NodeID: nodeID, Data: data,
	})
}

// turn seeds one checkpoint with the split usage both backend paths write
// at a turn's end — the baseline the per-step feed reconciles against.
func turn(t *testing.T, s store.RunStore, runID, nodeID string, idx, in, out int) {
	t.Helper()
	ts := store.AsTurnStore(s)
	if ts == nil {
		t.Fatal("the filesystem store no longer satisfies TurnStore")
	}
	if err := ts.WriteTurn(context.Background(), &store.TurnCheckpoint{
		RunID: runID, NodeID: nodeID, LoopIter: 0, TurnIndex: idx,
		Backend: "claude_code",
		Usage:   store.TurnUsage{InputTokens: in, OutputTokens: out},
	}); err != nil {
		t.Fatalf("WriteTurn(%d): %v", idx, err)
	}
}

func nodeStarted(t *testing.T, s store.RunStore, runID, nodeID string) {
	t.Helper()
	appendEvent(t, s, runID, store.Event{
		Type: store.EventNodeStarted, NodeID: nodeID,
		Data: map[string]any{"kind": "agent", "iteration": 0},
	})
}

// The reconciliation the #1481 review asked for: the per-step feed must
// equal the node's checkpoints FIELD BY FIELD. Measured on this
// machine's own store (run live-feat-gateway-1793219201, node `gw`): two
// steps of 1279/60 and 1316/9 against two checkpoints of the same
// values — 2595 input and 69 output on both sides.
func TestTheTwoUsageRecordingsReconcileFieldByField(t *testing.T) {
	s := tmpStore(t)
	runID := newRun(t, s)
	nodeStarted(t, s, runID, "gw")
	step(t, s, runID, "gw", map[string]any{"input_tokens": 1279, "output_tokens": 60})
	step(t, s, runID, "gw", map[string]any{"input_tokens": 1316, "output_tokens": 9})
	turn(t, s, runID, "gw", 0, 1279, 60)
	turn(t, s, runID, "gw", 1, 1316, 9)

	prof, err := ParseRun(context.Background(), s, runID)
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	u := prof.Nodes[0].Usage
	if u.StepInputTokens != 2595 || u.StepOutputTokens != 69 {
		t.Fatalf("the step feed sums to %d/%d, want 2595/69", u.StepInputTokens, u.StepOutputTokens)
	}
	if u.TurnInputTokens != 2595 || u.TurnOutputTokens != 69 {
		t.Fatalf("the checkpoints sum to %d/%d, want 2595/69", u.TurnInputTokens, u.TurnOutputTokens)
	}
	if !u.Reconcilable {
		t.Fatalf("a node with both recordings is not comparable: %+v", u)
	}
	if !u.Reconciled() {
		t.Fatalf("the two recordings agree and the comparison says they do not: input gap %+d, output gap %+d",
			u.GapInputTokens, u.GapOutputTokens)
	}
	c := Aggregate([]*RunProfile{prof})
	r := c.Reconciliation
	if r.ReconcilableNodes != 1 || r.ReconciledNodes != 1 || r.NodesWithGap != 0 {
		t.Fatalf("the corpus comparison is wrong: %+v", r)
	}
	if r.Steps != 2 || r.StepsWithoutUsage != 0 {
		t.Fatalf("steps = %d (%d without usage), want 2 (0)", r.Steps, r.StepsWithoutUsage)
	}
}

// A producing site that stops emitting its usage leaves the event in
// place: the node's step COUNT is unchanged and only the sum moves. The
// gap is what finds it, and the report names the node.
func TestAProducingSiteThatStopsEmittingShowsUpAsAGap(t *testing.T) {
	s := tmpStore(t)
	runID := newRun(t, s)
	nodeStarted(t, s, runID, "dev")
	step(t, s, runID, "dev", map[string]any{"input_tokens": 1279, "output_tokens": 60})
	step(t, s, runID, "dev", nil) // the muted site: the event, no usage
	turn(t, s, runID, "dev", 0, 1279, 60)
	turn(t, s, runID, "dev", 1, 1316, 9)

	prof, err := ParseRun(context.Background(), s, runID)
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	u := prof.Nodes[0].Usage
	if u.Steps != 2 || u.StepsWithUsage != 1 {
		t.Fatalf("steps = %d (%d with usage), want 2 (1)", u.Steps, u.StepsWithUsage)
	}
	if u.GapInputTokens != 1316 || u.GapOutputTokens != 9 {
		t.Fatalf("the muted site's spend is unaccounted and the gap says %+d/%+d, want +1316/+9",
			u.GapInputTokens, u.GapOutputTokens)
	}
	if u.Reconciled() {
		t.Fatal("a node missing a site's emission reports as reconciled")
	}
	c := Aggregate([]*RunProfile{prof})
	if c.Reconciliation.NodesWithGap != 1 || c.Reconciliation.GapInputTokens != 1316 {
		t.Fatalf("the corpus comparison is wrong: %+v", c.Reconciliation)
	}
	md := RenderMarkdown(c, []*RunProfile{prof}, RenderOptions{})
	if !strings.Contains(md, "Nodes whose two recordings disagree") || !strings.Contains(md, "| dev |") {
		t.Fatalf("the report does not name the node whose recordings disagree:\n%s", md)
	}
}

// The case a coverage percentage cannot see: a site that emits a
// zero-valued usage struct. Every step carries the fields, so coverage
// is 100 %, and only the reconciliation reddens. `usage_unreported` is
// the producer's own account of why — it explains the gap, it does not
// excuse it.
func TestAZeroValuedUsageStructReddensWhereCoverageStaysGreen(t *testing.T) {
	s := tmpStore(t)
	runID := newRun(t, s)
	nodeStarted(t, s, runID, "dev")
	step(t, s, runID, "dev", map[string]any{"input_tokens": 1279, "output_tokens": 60})
	step(t, s, runID, "dev", map[string]any{
		"input_tokens": 0, "output_tokens": 0, "usage_unreported": true,
	})
	turn(t, s, runID, "dev", 0, 1279, 60)
	turn(t, s, runID, "dev", 1, 1316, 9)

	prof, err := ParseRun(context.Background(), s, runID)
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	u := prof.Nodes[0].Usage
	if u.StepsWithUsage != u.Steps {
		t.Fatalf("a zero-valued usage struct did not count as present: %d of %d steps", u.StepsWithUsage, u.Steps)
	}
	if u.StepsUsageUnreported != 1 {
		t.Fatalf("steps flagged usage_unreported = %d, want 1", u.StepsUsageUnreported)
	}
	if u.Reconciled() {
		t.Fatal("a zero-valued usage struct passed the reconciliation")
	}
	if u.GapInputTokens != 1316 || u.GapOutputTokens != 9 {
		t.Fatalf("gap = %+d/%+d, want +1316/+9", u.GapInputTokens, u.GapOutputTokens)
	}
	c := Aggregate([]*RunProfile{prof})
	if c.Reconciliation.StepsWithoutUsage != 0 {
		t.Fatalf("coverage reports %d steps without usage: the zero-valued struct must read as present",
			c.Reconciliation.StepsWithoutUsage)
	}
	if c.Reconciliation.NodesWithGap != 1 {
		t.Fatalf("the corpus found no gap: %+v", c.Reconciliation)
	}
}

// A backend that could not split its total writes AggregateTokens, which
// leaves no field-by-field baseline. That is a limit of the recording,
// not a gap: the node is stated apart and claims nothing.
//
// Two conditions refuse the comparison and each has its own scenario,
// because either one alone would make the other look tested: a node
// whose checkpoints are ALL aggregates has no input/output baseline at
// all, and a node where ONE turn could not be split has a baseline that
// is real but incomplete — comparing it would invent a gap the size of
// the unsplit turn.
func TestAnUnsplitAggregateIsStatedApartNotCountedAsAGap(t *testing.T) {
	ctx := context.Background()
	aggregateTurn := func(t *testing.T, s store.RunStore, runID string, idx, total int) {
		t.Helper()
		if err := store.AsTurnStore(s).WriteTurn(ctx, &store.TurnCheckpoint{
			RunID: runID, NodeID: "dev", LoopIter: 0, TurnIndex: idx,
			Backend: "claw", Usage: store.TurnUsage{AggregateTokens: total},
		}); err != nil {
			t.Fatalf("WriteTurn(%d): %v", idx, err)
		}
	}
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, s store.RunStore, runID string)
		turns int
	}{
		{"every checkpoint carries an unsplit total", func(t *testing.T, s store.RunStore, runID string) {
			aggregateTurn(t, s, runID, 0, 1339)
		}, 1},
		{"one checkpoint of several could not be split", func(t *testing.T, s store.RunStore, runID string) {
			turn(t, s, runID, "dev", 0, 1279, 60)
			aggregateTurn(t, s, runID, 1, 1325)
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			runID := newRun(t, s)
			nodeStarted(t, s, runID, "dev")
			step(t, s, runID, "dev", map[string]any{"input_tokens": 1279, "output_tokens": 60})
			tc.seed(t, s, runID)

			prof, err := ParseRun(ctx, s, runID)
			if err != nil {
				t.Fatalf("ParseRun: %v", err)
			}
			u := prof.Nodes[0].Usage
			if u.Turns != tc.turns {
				t.Fatalf("turns read = %d, want %d", u.Turns, tc.turns)
			}
			if u.Reconcilable {
				t.Fatalf("an unsplit aggregate was compared field by field: %+v", u)
			}
			if u.GapInputTokens != 0 || u.GapOutputTokens != 0 {
				t.Fatalf("a node with no baseline reports a gap: %+d/%+d", u.GapInputTokens, u.GapOutputTokens)
			}
			c := Aggregate([]*RunProfile{prof})
			r := c.Reconciliation
			if r.UnsplitAggregateNodes != 1 || r.NodesWithGap != 0 || r.ReconcilableNodes != 0 {
				t.Fatalf("the corpus comparison is wrong: %+v", r)
			}
			md := RenderMarkdown(c, []*RunProfile{prof}, RenderOptions{})
			if !strings.Contains(md, "no field-by-field baseline") {
				t.Fatalf("the report does not say the aggregate has no baseline:\n%s", md)
			}
		})
	}
}

// The report's own sentence, executed: "a node whose backend writes only
// one of the two recordings is not comparable and claims nothing".
// Comparing one recording against a missing one would invent a gap the
// size of whichever side exists, and call a node nobody metered a
// defect. Each shape of a missing side gets its own case.
func TestOneRecordingAloneIsAnAbsenceNotAGap(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name           string
		seed           func(t *testing.T, s store.RunStore, runID string)
		turns          int
		nodesWithSteps int
	}{
		{"checkpoints that recorded no usage", func(t *testing.T, s store.RunStore, runID string) {
			step(t, s, runID, "dev", map[string]any{"input_tokens": 1279, "output_tokens": 60})
			if err := store.AsTurnStore(s).WriteTurn(ctx, &store.TurnCheckpoint{
				RunID: runID, NodeID: "dev", LoopIter: 0, TurnIndex: 0,
				Backend: "claude_code", // no Usage: the backend reported none
			}); err != nil {
				t.Fatalf("WriteTurn: %v", err)
			}
		}, 1, 1},
		{"a feed with no checkpoint at all", func(t *testing.T, s store.RunStore, runID string) {
			step(t, s, runID, "dev", map[string]any{"input_tokens": 1279, "output_tokens": 60})
		}, 0, 1},
		{"checkpoints with no feed at all", func(t *testing.T, s store.RunStore, runID string) {
			turn(t, s, runID, "dev", 0, 1279, 60)
		}, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			runID := newRun(t, s)
			nodeStarted(t, s, runID, "dev")
			tc.seed(t, s, runID)

			prof, err := ParseRun(ctx, s, runID)
			if err != nil {
				t.Fatalf("ParseRun: %v", err)
			}
			n := prof.Nodes[0]
			if n.Usage.Turns != tc.turns {
				t.Fatalf("turns read = %d, want %d", n.Usage.Turns, tc.turns)
			}
			if n.Usage.Reconcilable {
				t.Fatalf("a node with one recording was compared: %+v", n.Usage)
			}
			if n.Usage.GapInputTokens != 0 || n.Usage.GapOutputTokens != 0 {
				t.Fatalf("a node with one recording reports a gap: %+d/%+d",
					n.Usage.GapInputTokens, n.Usage.GapOutputTokens)
			}
			c := Aggregate([]*RunProfile{prof})
			r := c.Reconciliation
			if r.NodesWithGap != 0 || r.ReconcilableNodes != 0 {
				t.Fatalf("the corpus read an absence as a gap: %+v", r)
			}
			if r.NodesWithSteps != tc.nodesWithSteps {
				t.Fatalf("nodes with a feed = %d, want %d", r.NodesWithSteps, tc.nodesWithSteps)
			}
		})
	}
}

// Opposite gaps must not cancel: a feed that emits twice is as much a
// defect as one that stopped, and a corpus that summed them signed would
// report a clean total over two broken nodes.
func TestTheCorpusGapSumsMagnitudesSoOppositeGapsDoNotCancel(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	runID := newRun(t, s)

	// `short` lost a site's emission: the checkpoints carry 1316/9 more.
	nodeStarted(t, s, runID, "short")
	step(t, s, runID, "short", map[string]any{"input_tokens": 1279, "output_tokens": 60})
	turn(t, s, runID, "short", 0, 1279, 60)
	turn(t, s, runID, "short", 1, 1316, 9)

	// `double` emitted the same step twice: the feed carries 1316/9 more.
	nodeStarted(t, s, runID, "double")
	step(t, s, runID, "double", map[string]any{"input_tokens": 1279, "output_tokens": 60})
	step(t, s, runID, "double", map[string]any{"input_tokens": 1316, "output_tokens": 9})
	turn(t, s, runID, "double", 0, 1279, 60)

	prof, err := ParseRun(ctx, s, runID)
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	gaps := map[string][2]int{}
	for _, n := range prof.Nodes {
		gaps[n.NodeID] = [2]int{n.Usage.GapInputTokens, n.Usage.GapOutputTokens}
	}
	if gaps["short"] != [2]int{1316, 9} {
		t.Fatalf("the short node's gap = %v, want [1316 9]", gaps["short"])
	}
	if gaps["double"] != [2]int{-1316, -9} {
		t.Fatalf("the double-emitting node's gap = %v, want [-1316 -9]", gaps["double"])
	}
	r := Aggregate([]*RunProfile{prof}).Reconciliation
	if r.NodesWithGap != 2 {
		t.Fatalf("nodes with a gap = %d, want 2", r.NodesWithGap)
	}
	if r.GapInputTokens != 2632 || r.GapOutputTokens != 18 {
		t.Fatalf("the corpus gap = %d/%d, want 2632/18 (magnitudes, not a signed sum)",
			r.GapInputTokens, r.GapOutputTokens)
	}
}

// A supervised raw session has no nodes (pkg/supervise observes a
// transcript, not a workflow), so its steps name none. Folding them into
// a phantom node would move every per-node denominator in the report:
// they are counted apart.
func TestStepsThatNameNoNodeAreCountedApart(t *testing.T) {
	s := tmpStore(t)
	runID := newRun(t, s)
	nodeStarted(t, s, runID, "dev")
	step(t, s, runID, "dev", map[string]any{"input_tokens": 100, "output_tokens": 10})
	turn(t, s, runID, "dev", 0, 100, 10)
	step(t, s, runID, "", map[string]any{"input_tokens": 900, "output_tokens": 90})

	prof, err := ParseRun(context.Background(), s, runID)
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	if len(prof.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1: a step naming no node created one", len(prof.Nodes))
	}
	if prof.StepsOutsideANode != 1 {
		t.Fatalf("steps outside a node = %d, want 1", prof.StepsOutsideANode)
	}
	u := prof.Nodes[0].Usage
	if u.StepInputTokens != 100 || !u.Reconciled() {
		t.Fatalf("the nodeless step leaked into the node: %+v", u)
	}
	c := Aggregate([]*RunProfile{prof})
	if c.Reconciliation.StepsOutsideANode != 1 || c.Reconciliation.NodesWithGap != 0 {
		t.Fatalf("the corpus comparison is wrong: %+v", c.Reconciliation)
	}
}

// #2806 — the run the reconciliation caught, replayed:
// live-feat-budget-resume-1786088951, node `writer`: a first attempt
// died on budget_exceeded, the resumed run re-executed the node, and
// BOTH attempts emitted their step-1 usage (23, then 31 output tokens).
// The checkpoint coordinate derives from the step, not the attempt, so
// the retried attempt wrote where the first had written — and a store
// that overwrites there leaves the baseline at 31 where the feed
// carries 54. Both attempts must stay in the baseline the comparison
// reads.
func TestARetriedNodeKeepsBothAttemptsInTheBaseline(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	runID := newRun(t, s)
	nodeStarted(t, s, runID, "writer")
	step(t, s, runID, "writer", map[string]any{"input_tokens": 1200, "output_tokens": 23})
	step(t, s, runID, "writer", map[string]any{"input_tokens": 2100, "output_tokens": 31})
	// Two attempts, one coordinate: exactly what the retried node wrote.
	for _, u := range [][2]int{{1200, 23}, {2100, 31}} {
		if err := store.AsTurnStore(s).WriteTurn(ctx, &store.TurnCheckpoint{
			RunID: runID, NodeID: "writer", LoopIter: 0, TurnIndex: 0,
			Backend: "claude_code",
			Usage:   store.TurnUsage{InputTokens: u[0], OutputTokens: u[1]},
		}); err != nil {
			t.Fatalf("WriteTurn: %v", err)
		}
	}

	prof, err := ParseRun(ctx, s, runID)
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	u := prof.Nodes[0].Usage
	if u.Turns != 2 {
		t.Fatalf("turns read = %d, want 2 — the retried attempt erased the first attempt's checkpoint", u.Turns)
	}
	if u.TurnInputTokens != 3300 || u.TurnOutputTokens != 54 {
		t.Fatalf("the checkpoints sum to %d/%d, want 3300/54 (both attempts)", u.TurnInputTokens, u.TurnOutputTokens)
	}
	if !u.Reconcilable || !u.Reconciled() {
		t.Fatalf("a retried node must reconcile once both attempts are kept: %+v", u)
	}
	if r := Aggregate([]*RunProfile{prof}).Reconciliation; r.NodesWithGap != 0 {
		t.Fatalf("the corpus still reports a gap on a retried node: %+v", r)
	}
}
