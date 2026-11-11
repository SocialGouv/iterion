package runner

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

// A cloud subbot child is a run of its own bot: the routing policy its root
// run froze at launch folds again for the CHILD's bot, through the launch's
// own fold and screens, and the child's executor dispatches with that fold.
// A cell scoped to the parent's bot does not reach the child, one scoped to
// the child's bot does, an unscoped cell and `*/x` still do; the parent's
// folded rows and its own launch rows do not travel.
//
// The observable is the child's dispatch: its agent `act` runs claw on
// claude-sonnet-4-5, and a cell pinning it to openai/gpt-6-sol makes the
// offline claw leg refuse on gpt-6-sol instead (no credential, no network).

// subbotFoldParentBot invokes the child bundle beside it.
var subbotFoldParentBot = strings.Replace(subbotTestParent, `source: "child.bot"`, `source: "../child/main.bot"`, 1)

// subbotFoldMidBot is a child that itself invokes a grandchild bundle.
const subbotFoldMidBot = `schema vout:
  validated: bool
  echoed: string

subbot run_gc:
  source: "../grandchild/main.bot"
  output: vout

workflow mid:
  entry: run_gc
  run_gc -> done when validated
  run_gc -> fail
`

// modelCell pins the agent the selector reaches onto openai/gpt-6-sol.
func modelCell(sel string) store.RunLLMRouteOverride {
	return store.RunLLMRouteOverride{Selector: sel, Model: "openai/gpt-6-sol"}
}

// wireRows is what the publisher puts on the parent's message for this
// policy: the launch fold for the parent's bot.
func wireRows(launchRows []runview.ModelOverrideEntry, pol *store.RunLLMRoutePolicy, parentBot string) []queue.ModelOverride {
	var rows []queue.ModelOverride
	for _, e := range runview.EffectiveModelOverrides(launchRows, pol, llmroute.BotIdentity{ID: parentBot}) {
		rows = append(rows, queue.ModelOverride{Selector: e.Selector, Backend: e.Backend, Model: e.Model, Provider: e.Provider, Effort: e.Effort})
	}
	return rows
}

// runSubbotFoldFixture runs the parent's subbot node once and returns the
// child's error. seedParent=false leaves the root run's document absent.
func runSubbotFoldFixture(t *testing.T, childDir, childBot string, extra map[string]string, pol *store.RunLLMRoutePolicy, msgRows []queue.ModelOverride, seedParent bool) error {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "")
	r, _ := subbotTestRunner(t)
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "parent")
	writeSubbotFixture(t, parentDir, "main.bot", subbotFoldParentBot)
	writeSubbotFixture(t, filepath.Join(dir, childDir), "main.bot", childBot)
	for sub, body := range extra {
		writeSubbotFixture(t, filepath.Join(dir, sub), "main.bot", body)
	}
	msg := &queue.RunMessage{RunID: "run-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent", ModelOverrides: msgRows}
	if seedParent {
		p := storetest.SeedParentRun(t, r.cfg.Store, msg.RunID)
		p.LLMRoutePolicy = pol
		if err := r.cfg.Store.SaveRun(store.WithIdentity(context.Background(), "_test", "_test"), p); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := r.subbotRunnerFor(msg, parentDir, dir, iterlog.Nop())(ctx, runtime.SubbotRequest{
		Source: "../" + childDir + "/main.bot", ParentRunID: msg.RunID, NodeID: "run_ticket", ReattachKey: "run_ticket",
	})
	if err == nil {
		t.Fatal("precondition: the child's claw leg fails offline (no credential)")
	}
	return err
}

// reachedGPT reports whether the child's agent dispatched on the cell's
// model rather than its authored one.
func reachedGPT(t *testing.T, err error) bool {
	t.Helper()
	gpt, sonnet := strings.Contains(err.Error(), "gpt-6-sol"), strings.Contains(err.Error(), "claude-sonnet-4-5")
	if gpt == sonnet {
		t.Fatalf("precondition: the child's error names exactly one of gpt-6-sol and claude-sonnet-4-5: %v", err)
	}
	return gpt
}

func TestSubbotChildFoldsTheRoutingPolicyForItsOwnBot(t *testing.T) {
	for _, tc := range []struct {
		sel   string
		reach bool
	}{
		{"parent/act", false},
		{"child/act", true},
		{"act", true},
		{"*/act", true},
	} {
		t.Run(tc.sel, func(t *testing.T) {
			pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{modelCell(tc.sel)}}
			err := runSubbotFoldFixture(t, "child", agentChildBot(""), nil, pol, wireRows(nil, pol, "parent"), true)
			if got := reachedGPT(t, err); got != tc.reach {
				t.Fatalf("cell %q reached the child bot = %v, want %v (child error: %v)", tc.sel, got, tc.reach, err)
			}
		})
	}
}

// A grandchild folds the ROOT run's snapshot for its own bot: the middle
// child's document carries no policy, so a recursion that re-read the
// immediate parent would hand the grandchild nothing.
func TestSubbotGrandchildFoldsTheRootRunsPolicy(t *testing.T) {
	grandchild := map[string]string{"grandchild": agentChildBot("")}
	for _, tc := range []struct {
		sel   string
		reach bool
	}{
		{"grandchild/act", true},
		{"mid/act", false},
		{"parent/act", false},
	} {
		t.Run(tc.sel, func(t *testing.T) {
			pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{modelCell(tc.sel)}}
			err := runSubbotFoldFixture(t, "mid", subbotFoldMidBot, grandchild, pol, wireRows(nil, pol, "parent"), true)
			if got := reachedGPT(t, err); got != tc.reach {
				t.Fatalf("cell %q reached the grandchild bot = %v, want %v (error: %v)", tc.sel, got, tc.reach, err)
			}
		})
	}
}

// The parent's own launch rows apply to the run they launched: a child
// folds the policy alone.
func TestSubbotChildDoesNotInheritTheParentsLaunchRows(t *testing.T) {
	launch := []runview.ModelOverrideEntry{{Selector: "act", Model: "openai/gpt-6-sol"}}
	err := runSubbotFoldFixture(t, "child", agentChildBot(""), nil, nil, wireRows(launch, nil, "parent"), true)
	if reachedGPT(t, err) {
		t.Fatalf("the parent's launch row reached the child bot (child error: %v)", err)
	}
}

// A root run whose document cannot be read refuses the child, naming the
// run and the child's bot — never a fallback to the parent's folded rows.
func TestSubbotChildRefusesAnUnreadableRootPolicy(t *testing.T) {
	err := runSubbotFoldFixture(t, "child", agentChildBot(""), nil, nil, []queue.ModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}, false)
	msg := err.Error()
	if !strings.Contains(msg, "routing policy") || !strings.Contains(msg, `"run-parent"`) || !strings.Contains(msg, `"child"`) {
		t.Fatalf("child error = %v, want a refusal naming the root run's routing policy and the child bot", err)
	}
}

// The child's fold meets the launch's screens: a backend and a provider
// pinned by two cells compose a pair the composed-pair check refuses, and
// the refusal names the policy cells — a check the executor build alone
// never makes.
func TestSubbotChildFoldMeetsTheLaunchScreens(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{
		{Selector: "child/act", Backend: "claw"},
		{Selector: "*/act", Provider: "zai"},
	}}
	err := runSubbotFoldFixture(t, "child", agentChildBot(""), nil, pol, nil, true)
	if !strings.Contains(err.Error(), "the routing policy's override cell") || !strings.Contains(err.Error(), `"child"`) {
		t.Fatalf("child error = %v, want the composed-pair refusal naming the policy cells and the child bot", err)
	}
}

// The child's document stamps the rows its executor runs with, so the
// studio shows them and a resume of the child replays them.
func TestSubbotChildDocumentStampsItsFoldedRows(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	r, st := subbotTestRunner(t)
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "parent")
	writeSubbotFixture(t, parentDir, "main.bot", subbotFoldParentBot)
	writeSubbotFixture(t, filepath.Join(dir, "child"), "main.bot", agentChildBot(""))
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{modelCell("child/act")}}
	msg := &queue.RunMessage{RunID: "run-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent", ModelOverrides: wireRows(nil, pol, "parent")}
	idCtx := store.WithIdentity(context.Background(), "_test", "_test")
	p := storetest.SeedParentRun(t, st, msg.RunID)
	p.LLMRoutePolicy = pol
	if err := st.SaveRun(idCtx, p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.subbotRunnerFor(msg, parentDir, dir, iterlog.Nop())(ctx, runtime.SubbotRequest{
		Source: "../child/main.bot", ParentRunID: msg.RunID, NodeID: "run_ticket", ReattachKey: "run_ticket",
	}); err == nil {
		t.Fatal("precondition: the child's claw leg fails offline (no credential)")
	}
	parent, err := st.LoadRun(idCtx, msg.RunID)
	if err != nil {
		t.Fatal(err)
	}
	childID := parent.SubbotChildren["run_ticket"]
	if childID == "" {
		t.Fatal("precondition: the parent records its child")
	}
	child, err := st.LoadRun(idCtx, childID)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}
	if !reflect.DeepEqual(child.ModelOverrides, want) {
		t.Fatalf("child document rows = %+v, want its fold %+v", child.ModelOverrides, want)
	}
}
