package runview

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The witness (#2637): a backend pinned by one entry and a provider by
// another — each singly valid at write time — must be refused at the
// launch fold, naming both entries and their levels. Driven through
// Service.Launch, the funnel every launch surface shares.
const splitPairBot = `
agent work:
  backend: "claude_code"
  model: "claude-opus-5"
  system: p
  tools: [read_file]

prompt p:
  Do the work.

workflow split:
  entry: work
  work -> done
`

func launchWithOverrides(t *testing.T, policy *store.RunLLMRoutePolicy, rows []ModelOverrideEntry) (*stubLaunchPublisher, error) {
	t.Helper()
	pub := &stubLaunchPublisher{}
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, launchErr := svc.Launch(context.Background(), LaunchSpec{
		FilePath:       "split.bot",
		Source:         splitPairBot,
		ModelOverrides: rows,
		LLMRoutePolicy: policy,
	})
	return pub, launchErr
}

// Two policy cells of ONE level — the snapshot shape the resolver
// produces: the split the write gate certified entry-wise.
func TestLaunchRefusesASplitPairAcrossTwoPolicyCells(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "work", Backend: "claw"},
			{Selector: "work", Provider: "zai"},
		},
	}
	pub, err := launchWithOverrides(t, pol, nil)
	if err == nil {
		t.Fatal("a split pair across two policy cells was admitted at launch")
	}
	if pub.lastSpec != nil {
		t.Fatal("the split-pair launch reached the publisher")
	}
	for _, want := range []string{"claw", "zai", `the routing policy's override cell "work"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "the launch's own override row") {
		t.Fatalf("error = %q, must not blame the launch for a policy-only split", err)
	}
}

// Two LEVELS: the policy pins the backend, the launch body carries the
// provider — the fold is where the pair first exists.
func TestLaunchRefusesASplitPairAcrossTwoLevels(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "work", Backend: "claw"},
		},
	}
	rows := []ModelOverrideEntry{{Selector: "work", Provider: "zai"}}
	_, err := launchWithOverrides(t, pol, rows)
	if err == nil {
		t.Fatal("a split pair across the policy and the launch was admitted")
	}
	for _, want := range []string{"claw", "zai", `the routing policy's override cell "work"`, `the launch's own override row "work"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
}

// A split the harness honours stays legal: claude_code walks any
// validated hint, so the same two-level shape launches.
func TestLaunchKeepsASplitPairTheHarnessHonours(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "work", Backend: "claude_code"},
		},
	}
	rows := []ModelOverrideEntry{{Selector: "work", Provider: "zai"}}
	pub, err := launchWithOverrides(t, pol, rows)
	if err != nil {
		t.Fatalf("an honoured split pair was refused: %v", err)
	}
	if pub.lastSpec == nil {
		t.Fatal("the honoured launch never reached the publisher")
	}
}

// The provenance half: the walk must name the entry each field came
// from, or the refusal blames nobody in particular.
func TestFoldSourcedOverridesCarriesTheOriginBesideEachEntry(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{{Selector: "work", Backend: "claw"}},
	}
	rows := []ModelOverrideEntry{{Selector: "work", Provider: "zai"}}
	got := foldSourcedOverrides(rows, pol)
	want := []sourcedOverrideEntry{
		{ModelOverrideEntry: ModelOverrideEntry{Selector: "work", Backend: "claw"}, origin: originPolicy},
		{ModelOverrideEntry: rows[0], origin: originLaunch},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sourced fold = %+v, want each entry carrying its origin", got)
	}
}

// Node order is sorted: the same launch must read the same refusal,
// whichever map iteration found it first.
func TestComposedPairFindingsWalkSortedNodeOrder(t *testing.T) {
	wf := &ir.Workflow{Name: "two", Nodes: map[string]ir.Node{}}
	for _, id := range []string{"b_work", "a_work"} {
		agent := &ir.AgentNode{}
		agent.ID = id
		wf.Nodes[id] = agent
	}
	sourced := foldSourcedOverrides(nil, &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "*", Backend: "claw"},
			{Selector: "*", Provider: "zai"},
		},
	})
	findings := composedPairFindings(wf, sourced)
	if len(findings) != 2 {
		t.Fatalf("findings = %d, want one per node", len(findings))
	}
	if findings[0].id != "a_work" || findings[1].id != "b_work" {
		t.Fatalf("order = [%s %s], want sorted [a_work b_work]", findings[0].id, findings[1].id)
	}
}

// The write-surface screen says so at save time (C1xx posture): the
// launch refusal above stays the gate.
func TestScreenPolicyOverridesWarnsOnTheComposedPair(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "work", Backend: "claw"},
			{Selector: "work", Provider: "zai"},
		},
	}
	path := filepath.Join(t.TempDir(), "split.bot")
	if err := os.WriteFile(path, []byte(splitPairBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	warnings, err := ScreenPolicyOverrides(path, splitPairBot, "", "", pol)
	if err != nil {
		t.Fatalf("screen: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{`composes agent "work" onto a pair the launch gate refuses`, "claw", "zai"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings = %v, want them to carry %q", warnings, want)
		}
	}
}

// The llm-mode router composes the same pair the executor spends, though
// it satisfies no LLMNode — the walk must include it or the split pin
// stays live for routers (#2637).
func TestComposedPairFindingsWalksTheLLMRouter(t *testing.T) {
	wf := &ir.Workflow{Name: "routed", Nodes: map[string]ir.Node{}}
	router := &ir.RouterNode{RouterMode: ir.RouterLLM}
	router.ID = "pick"
	wf.Nodes["pick"] = router
	sourced := foldSourcedOverrides(nil, &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{
			{Selector: "router", Backend: "claw"},
			{Selector: "router", Provider: "zai"},
		},
	})
	findings := composedPairFindings(wf, sourced)
	if len(findings) != 1 || findings[0].id != "pick" {
		t.Fatalf("findings = %+v, want the llm-mode router judged", findings)
	}
	// A deterministic router spends nothing: its overrides stay unjudged.
	deterministic := &ir.RouterNode{}
	deterministic.ID = "pick"
	wf.Nodes["pick"] = deterministic
	if findings := composedPairFindings(wf, sourced); len(findings) != 0 {
		t.Fatalf("a deterministic router was judged: %+v", findings)
	}
}

// The direction half: the same (selector, backend) written at two levels
// — the LATER rule wins the tie, so the refusal must blame the launch
// row, not the shadowed policy cell.
func TestLaunchRefusalBlamesTheLaterRuleOnADuplicateBackend(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{
		Overrides: []store.RunLLMRouteOverride{{Selector: "work", Backend: "claw"}},
	}
	rows := []ModelOverrideEntry{{Selector: "work", Backend: "claw", Provider: "zai"}}
	_, err := launchWithOverrides(t, pol, rows)
	if err == nil {
		t.Fatal("the split pair was admitted at launch")
	}
	if !strings.Contains(err.Error(), `the launch's own override row "work"`) {
		t.Fatalf("error = %q, want it to blame the launch row (the later rule)", err)
	}
	if strings.Contains(err.Error(), "the routing policy's override cell") {
		t.Fatalf("error = %q, must blame the later rule, not the shadowed policy cell", err)
	}
}
