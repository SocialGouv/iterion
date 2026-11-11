package runner

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

// A cloud subbot child is a run of its own bot: the routing policy its root
// run froze at launch folds again for the CHILD's bot, through the launch's
// own fold and screens, and the child's executor dispatches with that fold.
// A cell scoped to the parent's bot does not reach the child, one scoped to
// the child's bot does, an unscoped cell and `*/x` still do; the parent's
// folded rows do not travel. The root run's own launch rows — the raw rows
// its document records — fold on top, for every child of the tree.
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
	return runSubbotFoldFixtureWithLaunchRows(t, childDir, childBot, extra, pol, nil, msgRows, seedParent)
}

// runSubbotFoldFixtureWithLaunchRows is runSubbotFoldFixture under a root
// run whose document also records launchRows, its raw launch rows.
func runSubbotFoldFixtureWithLaunchRows(t *testing.T, childDir, childBot string, extra map[string]string, pol *store.RunLLMRoutePolicy, launchRows []store.RunModelOverride, msgRows []queue.ModelOverride, seedParent bool) error {
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
		p.LaunchModelOverrides = launchRows
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

// dispatchedModel names the one model of models the child's error names —
// the model its agent dispatched on.
func dispatchedModel(t *testing.T, err error, models ...string) string {
	t.Helper()
	var named []string
	for _, m := range models {
		if strings.Contains(err.Error(), m) {
			named = append(named, m)
		}
	}
	if len(named) != 1 {
		t.Fatalf("precondition: the child's error names exactly one of %q, got %q: %v", models, named, err)
	}
	return named[0]
}

// The root run's raw launch rows — what its document records, never the
// folded list its message carries — fold for the child, bot-free: a row on
// `act` reaches the child's `act`. The message's rows here are a policy
// fold for the parent's bot (`parent/act` → gpt-5.5-mini); a child reading
// them would dispatch on that model instead.
func TestSubbotChildFoldsTheRootRunsLaunchRows(t *testing.T) {
	launch := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}
	parentPol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: "parent/act", Model: "openai/gpt-5.5-mini"}}}
	err := runSubbotFoldFixtureWithLaunchRows(t, "child", agentChildBot(""), nil, nil, launch, wireRows(nil, parentPol, "parent"), true)
	if got := dispatchedModel(t, err, "gpt-6-sol", "gpt-5.5-mini", "claude-sonnet-4-5"); got != "gpt-6-sol" {
		t.Fatalf("child dispatched on %s, want the root run's launch row gpt-6-sol (child error: %v)", got, err)
	}
}

// A launch row is the launch's own word: on the same selector it outranks
// the policy cell scoped to the child's bot, as it outranks a cell at the
// root.
func TestSubbotChildLaunchRowOutranksAChildCell(t *testing.T) {
	launch := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: "child/act", Model: "openai/gpt-5.5-mini"}}}
	err := runSubbotFoldFixtureWithLaunchRows(t, "child", agentChildBot(""), nil, pol, launch, nil, true)
	if got := dispatchedModel(t, err, "gpt-6-sol", "gpt-5.5-mini", "claude-sonnet-4-5"); got != "gpt-6-sol" {
		t.Fatalf("child dispatched on %s, want the launch row's gpt-6-sol over the child cell's gpt-5.5-mini (child error: %v)", got, err)
	}
}

// A policy cell folded for the PARENT's bot never travels as a launch row:
// the root's document holds no launch row, its message holds the parent's
// fold, and the child stays on its authored model.
func TestSubbotChildReadsNoParentFoldAsALaunchRow(t *testing.T) {
	parentPol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: "parent/act", Model: "openai/gpt-6-sol"}}}
	err := runSubbotFoldFixtureWithLaunchRows(t, "child", agentChildBot(""), nil, parentPol, nil, wireRows(nil, parentPol, "parent"), true)
	if reachedGPT(t, err) {
		t.Fatalf("a cell folded for the parent's bot reached the child (child error: %v)", err)
	}
}

// A grandchild folds the ROOT run's launch rows as a child does.
func TestSubbotGrandchildFoldsTheRootsLaunchRows(t *testing.T) {
	launch := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}
	err := runSubbotFoldFixtureWithLaunchRows(t, "mid", subbotFoldMidBot, map[string]string{"grandchild": agentChildBot("")}, nil, launch, nil, true)
	if !reachedGPT(t, err) {
		t.Fatalf("the root run's launch row did not reach the grandchild (error: %v)", err)
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

// A team_gateway root launched with a `*` row pinning every agent onto the
// team's gateway keeps its subbot child's content on that gateway: the row
// folds for the child, whose agent is authored on a vendor model. Both wires
// are local recorders, so the observable is which one the child's agent
// calls — never the network.
func TestSubbotChildUnderATeamGatewayLaunchRowStaysOnTheGateway(t *testing.T) {
	var mu sync.Mutex
	var gwHits, vendorHits []string
	recorder := func(hits *[]string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*hits = append(*hits, r.URL.Path)
			mu.Unlock()
			http.Error(w, "recorder", http.StatusTeapot)
		}))
	}
	gw, vendor := recorder(&gwHits), recorder(&vendorHits)
	defer gw.Close()
	defer vendor.Close()
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", vendor.URL)

	r, st := subbotTestRunner(t)
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "parent")
	writeSubbotFixture(t, parentDir, "main.bot", subbotFoldParentBot)
	writeSubbotFixture(t, filepath.Join(dir, "child"), "main.bot", "prompt usr:\n  Go.\n\n"+strings.Replace(agentChildBot("  user: usr\n"), `"claude-sonnet-4-5"`, `"openai/gpt-6-sol"`, 1))
	msg := &queue.RunMessage{RunID: "run-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent",
		ContentBoundary: &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, GatewayHost: strings.TrimPrefix(gw.URL, "http://")},
	}
	p := storetest.SeedParentRun(t, st, msg.RunID)
	p.LaunchModelOverrides = []store.RunModelOverride{{Selector: "*", Backend: "claw", Provider: "openai_compatible", Model: "openai_compatible/scaleway/deepseek-v4.1-flash"}}
	if err := st.SaveRun(store.WithIdentity(context.Background(), "_test", "_test"), p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = secrets.WithCredentials(ctx, secrets.Credentials{
		APIKeys: map[secrets.Provider]string{
			secrets.ProviderOpenAI:           "sk-test-vendor",
			secrets.ProviderOpenAICompatible: "sk-test-gateway",
		},
		APIBaseURLs:    map[secrets.Provider]string{secrets.ProviderOpenAICompatible: gw.URL},
		AmbientRefused: true,
	})
	_, cerr := r.subbotRunnerFor(msg, parentDir, dir, iterlog.Nop())(ctx, runtime.SubbotRequest{
		Source: "../child/main.bot", ParentRunID: msg.RunID, NodeID: "run_ticket", ReattachKey: "run_ticket",
	})
	if cerr == nil {
		t.Fatal("precondition: both recorders refuse, so the child fails")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gwHits) == 0 || len(vendorHits) != 0 {
		t.Fatalf("child calls: gateway %q, vendor %q — want the gateway only (child error: %v)", gwHits, vendorHits, cerr)
	}
}

// childLayout is one place a subbot child's .bot lives in its bundle: the
// bundle's directory (its slug), its manifest name, and the files beside
// it ("" is the agent child).
type childLayout struct {
	name, source, slug, manifest string
	files                        map[string]string
}

func childLayouts() []childLayout {
	return []childLayout{
		{"nested workflow", "../lib/sub/x.bot", "lib", "libbot", map[string]string{"lib/manifest.yaml": "name: libbot\n", "lib/main.bot": "", "lib/sub/x.bot": ""}},
		{"workflows export", "../kit/workflows/x.bot", "kit", "kitbot", map[string]string{"kit/manifest.yaml": "name: kitbot\n", "kit/main.bot": "", "kit/workflows/x.bot": ""}},
		{"main.bot its manifest renames", "../cdir/main.bot", "cdir", "childbot", map[string]string{"cdir/manifest.yaml": "name: childbot\n", "cdir/main.bot": ""}},
	}
}

// runSubbotLayoutFixture runs the parent's subbot node once on a child
// laid out as lay, under a root run whose document froze pol, and returns
// the child's error.
func runSubbotLayoutFixture(t *testing.T, lay childLayout, pol *store.RunLLMRoutePolicy, logger *iterlog.Logger) error {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "")
	r, st := subbotTestRunner(t)
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "parent")
	writeSubbotFixture(t, parentDir, "main.bot", strings.Replace(subbotTestParent, `source: "child.bot"`, `source: "`+lay.source+`"`, 1))
	for rel, body := range lay.files {
		if body == "" {
			body = agentChildBot("")
		}
		writeSubbotFixture(t, filepath.Join(dir, filepath.Dir(rel)), filepath.Base(rel), body)
	}
	msg := &queue.RunMessage{RunID: "run-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent"}
	p := storetest.SeedParentRun(t, st, msg.RunID)
	p.LLMRoutePolicy = pol
	if err := st.SaveRun(store.WithIdentity(context.Background(), "_test", "_test"), p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := r.subbotRunnerFor(msg, parentDir, dir, logger)(ctx, runtime.SubbotRequest{
		Source: lay.source, ParentRunID: msg.RunID, NodeID: "run_ticket", ReattachKey: "run_ticket",
	})
	if err == nil {
		t.Fatal("precondition: the child's claw leg fails offline (no credential)")
	}
	return err
}

// A cloud child is the bot a top-level launch of its bundle is — the
// bundle's slug and its manifest name — wherever its .bot sits in that
// bundle: a nested workflow, a workflows/ export, a main.bot its manifest
// renames. The in-process lane reads the same identity
// (runview.SubbotChildBotIdentity).
func TestSubbotChildFoldsForItsBundlesSlugAndManifestName(t *testing.T) {
	for _, lay := range childLayouts() {
		for _, tc := range []struct {
			sel   string
			reach bool
		}{
			{lay.manifest + "/act", true},
			{lay.slug + "/act", true},
			{"parent/act", false},
			{"act", true},
		} {
			t.Run(lay.name+"/"+tc.sel, func(t *testing.T) {
				pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{modelCell(tc.sel)}}
				err := runSubbotLayoutFixture(t, lay, pol, iterlog.Nop())
				if got := reachedGPT(t, err); got != tc.reach {
					t.Fatalf("cell %q reached the child bot = %v, want %v (child error: %v)", tc.sel, got, tc.reach, err)
				}
			})
		}
	}
}

// The zero-match warning names the child by the identity it folded for: a
// typo'd cell spelled with a nested child's manifest name is warned, and
// names the bundle's slug beside it.
func TestSubbotChildOfANestedBundleWorkflowIsWarnedByItsIdentity(t *testing.T) {
	var buf bytes.Buffer
	sel := "libbot/nosuch_" + strings.ReplaceAll(t.Name(), "/", "_")
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{modelCell(sel)}}
	_ = runSubbotLayoutFixture(t, childLayouts()[0], pol, iterlog.New(iterlog.LevelWarn, &buf))
	if got := buf.String(); !strings.Contains(got, `names bot "libbot" (the manifest name of "lib")`) {
		t.Fatalf("child log = %q, want the zero-match warning naming libbot, the manifest name of the bundle lib", got)
	}
}

// A child's document records what its root froze — the routing-policy
// snapshot and the raw launch rows — so a resume of the child on its own
// (after a human gate) replays the root's providers allowlist, and the
// child's own children fold the root's snapshot and rows when the resumed
// child is the top of their tree.
func TestSubbotChildDocumentRecordsTheRootsRouting(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	r, st := subbotTestRunner(t)
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "parent")
	writeSubbotFixture(t, parentDir, "main.bot", subbotFoldParentBot)
	writeSubbotFixture(t, filepath.Join(dir, "child"), "main.bot", agentChildBot(""))
	pol := &store.RunLLMRoutePolicy{Providers: []string{"openai"}, Overrides: []store.RunLLMRouteOverride{modelCell("child/act")}}
	launch := []store.RunModelOverride{{Selector: "*", Effort: "low"}}
	msg := &queue.RunMessage{RunID: "run-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent", LLMProviders: []string{"openai"}}
	idCtx := store.WithIdentity(context.Background(), "_test", "_test")
	p := storetest.SeedParentRun(t, st, msg.RunID)
	p.LLMRoutePolicy = pol
	p.LaunchModelOverrides = launch
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
	child, err := st.LoadRun(idCtx, parent.SubbotChildren["run_ticket"])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(child.LLMRoutePolicy.ProviderAllowlist(), []string{"openai"}) || child.LLMRoutePolicy == nil || !reflect.DeepEqual(child.LLMRoutePolicy.Overrides, pol.Overrides) {
		t.Fatalf("child document policy = %+v, want the root's snapshot %+v (a resume of the child replays its allowlist)", child.LLMRoutePolicy, pol)
	}
	if !reflect.DeepEqual(child.LaunchModelOverrides, launch) {
		t.Fatalf("child document launch rows = %+v, want the root's %+v", child.LaunchModelOverrides, launch)
	}
}
