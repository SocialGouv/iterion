package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// blankAnthropicWireEnv keeps the pod's own anthropic-wire credentials out of
// a route's resolution: the delegate reads them as the ambient fallback.
func blankAnthropicWireEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR",
		"ZAI_API_KEY", "MOONSHOT_API_KEY", "MOONSHOT_BASE_URL",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	} {
		t.Setenv(name, "")
	}
}

// forfaitBesidePinnedKeys is the bundle the platform tier seals under the
// facade policy's `auto` default: its Claude forfait on the anthropic wire, and
// the z.ai and Anthropic keys only for the routes that name them.
func forfaitBesidePinnedKeys() context.Context {
	return secrets.WithCredentials(context.Background(), secrets.Credentials{
		PinnedAPIKeys: map[secrets.Provider]string{
			secrets.ProviderZAI:       "zai-pinned",
			secrets.ProviderAnthropic: "ant-pinned",
		},
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): "/forfait"},
		PlatformSourced: map[string]bool{
			string(secrets.OAuthKindClaudeCode): true,
			string(secrets.ProviderZAI):         true,
			string(secrets.ProviderAnthropic):   true,
		},
		Fingerprints: map[string]string{
			string(secrets.OAuthKindClaudeCode): "fp-forfait",
			string(secrets.ProviderZAI):         "fp-zai",
			string(secrets.ProviderAnthropic):   "fp-ant",
		},
	})
}

func platformKey(fp string) string {
	return usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, fp)
}

// weekCapped records a fresh weekly reading over the test policy's 75% cap.
func weekCapped(t *testing.T, caps usagecap.Store, key string, resets time.Time) {
	t.Helper()
	if err := caps.Record(context.Background(), key, usagecap.Reading{
		Window:      usagecap.WindowSevenDay,
		Utilization: 0.92,
		Status:      usagecap.StatusWarning,
		ResetsAt:    resets,
		ObservedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func agentRoute(id, backend, provider, model string) *ir.AgentNode {
	return &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, LLMFields: ir.LLMFields{Backend: backend, Provider: provider, Model: model}}
}

// chainWorkflow runs its nodes one after the other, entry first: every node is
// on every path.
func chainWorkflow(nodes ...ir.Node) *ir.Workflow {
	wf := &ir.Workflow{Name: "chain", Entry: nodes[0].NodeID(), Nodes: map[string]ir.Node{
		"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
	}}
	for i, n := range nodes {
		wf.Nodes[n.NodeID()] = n
		to := "done"
		if i+1 < len(nodes) {
			to = nodes[i+1].NodeID()
		}
		wf.Edges = append(wf.Edges, &ir.Edge{From: n.NodeID(), To: to})
	}
	return wf
}

// branchWorkflow lets a condition router send the run down ONE of its nodes:
// each node is on some path, none on all of them.
func branchWorkflow(nodes ...ir.Node) *ir.Workflow {
	wf := &ir.Workflow{Name: "branch", Entry: "pick", Nodes: map[string]ir.Node{
		"pick": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "pick"}, RouterMode: ir.RouterCondition},
		"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
	}}
	for _, n := range nodes {
		wf.Nodes[n.NodeID()] = n
		wf.Edges = append(wf.Edges,
			&ir.Edge{From: "pick", To: n.NodeID()},
			&ir.Edge{From: n.NodeID(), To: "done"})
	}
	return wf
}

func preflightFor(t *testing.T, caps usagecap.Store, ctx context.Context, wf *ir.Workflow) error {
	t.Helper()
	r := capRunner(capTestPolicy(), caps, &capStatusStore{})
	return r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-routes"}, iterlog.Nop())
}

func parkedUntil(t *testing.T, err error) time.Time {
	t.Helper()
	var rl *delegate.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("pre-flight = %v, want the run parked on the cap", err)
	}
	return rl.ResetAt
}

// A run's GLM routes spend the z.ai key pinned for them, whatever the forfait
// holding the wire says: a closed forfait does not park a run that never
// spends it. Judged on the run's default, it did — the default precedence
// skips a pinned key and lands on the forfait.
func TestUsageCapPreflight_AClosedForfaitDoesNotParkRoutesOnAPinnedKey(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := usagecap.NewMemStore()
	weekCapped(t, caps, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))

	glm := chainWorkflow(agentRoute("review", delegate.BackendClaudeCode, "", "glm-5.3"))
	if err := preflightFor(t, caps, forfaitBesidePinnedKeys(), glm); err != nil {
		t.Errorf("GLM-only run parked on the forfait's cap: %v — its route spends the z.ai key", err)
	}
	pinnedClaw := chainWorkflow(agentRoute("review", delegate.BackendClaw, "", "anthropic/claude-opus-5-5"))
	if err := preflightFor(t, caps, forfaitBesidePinnedKeys(), pinnedClaw); err != nil {
		t.Errorf("claw anthropic run parked on the forfait's cap: %v — its route spends the Anthropic key pinned for it", err)
	}
	// The route that does spend the forfait still parks.
	opus := chainWorkflow(agentRoute("review", delegate.BackendClaudeCode, "", "claude-opus-5-5"))
	if err := preflightFor(t, caps, forfaitBesidePinnedKeys(), opus); err == nil {
		t.Error("a claude_code run on the capped forfait started")
	}
}

// The reverse: a run whose default has room but whose every route spends a
// walled key parks on THAT key, and comes back when it reopens.
func TestUsageCapPreflight_AWalledPinnedKeyParksTheRoutesThatSpendIt(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := usagecap.NewMemStore()
	resets := time.Now().UTC().Add(50 * time.Hour).Truncate(time.Second)
	weekCapped(t, caps, platformKey("fp-zai"), resets)

	glm := chainWorkflow(agentRoute("review", delegate.BackendClaudeCode, "", "glm-5.3"))
	if at := parkedUntil(t, preflightFor(t, caps, forfaitBesidePinnedKeys(), glm)); !at.Equal(resets) {
		t.Errorf("parked until %v, want the z.ai window's reopening %v", at, resets)
	}
}

// Parking is decided on PATHS: a capped route every path crosses parks the
// run even beside a route with room, and a capped route some path avoids does
// not — the mid-run guard stops a capped call if the run takes it.
func TestUsageCapPreflight_ParksOnlyWhenEveryPathCrossesACappedRoute(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := usagecap.NewMemStore()
	weekCapped(t, caps, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	opus := func() ir.Node { return agentRoute("opus", delegate.BackendClaudeCode, "", "claude-opus-5-5") }
	glm := func() ir.Node { return agentRoute("glm", delegate.BackendClaudeCode, "", "glm-5.3") }

	if err := preflightFor(t, caps, forfaitBesidePinnedKeys(), chainWorkflow(opus(), glm())); err == nil {
		t.Error("started a run whose every path crosses the capped forfait")
	}
	if err := preflightFor(t, caps, forfaitBesidePinnedKeys(), branchWorkflow(opus(), glm())); err != nil {
		t.Errorf("parked a run with a path around the capped forfait: %v", err)
	}
}

// The retry is armed for the earliest reopening that frees a PATH: on
// alternative branches the first route to reopen is enough; in sequence the
// run needs both.
func TestUsageCapPreflight_ParksUntilAPathReopens(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := usagecap.NewMemStore()
	soon := time.Now().UTC().Add(5 * time.Hour).Truncate(time.Second)
	late := time.Now().UTC().Add(60 * time.Hour).Truncate(time.Second)
	weekCapped(t, caps, platformKey("fp-forfait"), late)
	weekCapped(t, caps, platformKey("fp-zai"), soon)
	opus := func() ir.Node { return agentRoute("opus", delegate.BackendClaudeCode, "", "claude-opus-5-5") }
	glm := func() ir.Node { return agentRoute("glm", delegate.BackendClaudeCode, "", "glm-5.3") }

	if at := parkedUntil(t, preflightFor(t, caps, forfaitBesidePinnedKeys(), branchWorkflow(opus(), glm()))); !at.Equal(soon) {
		t.Errorf("branches: parked until %v, want the first reopening %v", at, soon)
	}
	if at := parkedUntil(t, preflightFor(t, caps, forfaitBesidePinnedKeys(), chainWorkflow(opus(), glm()))); !at.Equal(late) {
		t.Errorf("sequence: parked until %v, want the last reopening %v", at, late)
	}
}

// A route the walk cannot read keeps the run's default credential — the
// pre-flight's reading before it read routes — and a route the delegate
// refuses before spawning is not the cap's to park.
func TestUsageCapPreflight_UnreadableAndRefusedRoutes(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := usagecap.NewMemStore()
	weekCapped(t, caps, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))

	templated := chainWorkflow(agentRoute("review", "{{vars.backend}}", "", "glm-5.3"))
	if err := preflightFor(t, caps, forfaitBesidePinnedKeys(), templated); err == nil {
		t.Error("a route resolved at dispatch escaped the capped default credential")
	}
	// No Moonshot key anywhere: the delegate refuses the node by name.
	refused := chainWorkflow(agentRoute("review", delegate.BackendClaudeCode, "moonshot", "kimi-k2"))
	if err := preflightFor(t, caps, forfaitBesidePinnedKeys(), refused); err != nil {
		t.Errorf("parked a route the delegate refuses before spawning: %v — no reset funds it", err)
	}
}

// One capped credential's store read failing does not decide for the run: the
// route it answers for counts as room, the others are still judged.
func TestUsageCapPreflight_FailsOpenPerCredential(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := &oneKeyFailingStore{Store: usagecap.NewMemStore(), failing: platformKey("fp-forfait")}
	weekCapped(t, caps.Store, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	weekCapped(t, caps.Store, platformKey("fp-zai"), time.Now().UTC().Add(30*time.Hour))
	opus := agentRoute("opus", delegate.BackendClaudeCode, "", "claude-opus-5-5")
	glm := agentRoute("glm", delegate.BackendClaudeCode, "", "glm-5.3")

	if err := preflightFor(t, caps, forfaitBesidePinnedKeys(), chainWorkflow(opus)); err != nil {
		t.Errorf("parked on a credential the store could not read: %v", err)
	}
	if err := preflightFor(t, caps, forfaitBesidePinnedKeys(), chainWorkflow(opus, glm)); err == nil {
		t.Error("an unreadable credential beside a walled one waved the run through")
	}
}

type oneKeyFailingStore struct {
	usagecap.Store
	failing string
}

func (s *oneKeyFailingStore) Latest(ctx context.Context, key string) ([]usagecap.Reading, error) {
	if key == s.failing {
		return nil, errors.New("store unavailable")
	}
	return s.Store.Latest(ctx, key)
}

// The decision does not depend on the order the routes were read in: equal
// reopenings resolve by node id, and the set of walls is order-free.
func TestParkDecision_IsDeterministic(t *testing.T) {
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	capped := map[string]usagecap.Decision{
		"b": {Blocked: true, ResetsAt: at, Reason: "b"},
		"a": {Blocked: true, ResetsAt: at, Reason: "a"},
	}
	wf := chainWorkflow(agentRoute("b", "", "", ""), agentRoute("a", "", "", ""))
	for range 20 {
		d, blocked := parkDecision(wf, capped)
		if !blocked || d.Reason[:1] != "a" {
			t.Fatalf("parkDecision = %+v, %v; want blocked on node a's decision", d, blocked)
		}
	}
}

// fanOutWorkflow runs its nodes in parallel, then joins.
func fanOutWorkflow(nodes ...ir.Node) *ir.Workflow {
	wf := &ir.Workflow{Name: "fan", Entry: "split", Nodes: map[string]ir.Node{
		"split": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "split"}, RouterMode: ir.RouterFanOutAll},
		"join":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "join"}, Command: "true"},
		"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
	}}
	for _, n := range nodes {
		wf.Nodes[n.NodeID()] = n
		wf.Edges = append(wf.Edges, &ir.Edge{From: "split", To: n.NodeID()}, &ir.Edge{From: n.NodeID(), To: "join"})
	}
	wf.Edges = append(wf.Edges, &ir.Edge{From: "join", To: "done"})
	return wf
}

// A fan-out runs every branch, so a capped branch is on every execution; and
// a SOFT cap stops nothing in flight, so a soft-capped route the run may reach
// parks it even when a path goes around — only a hard cap earns that relief.
func TestUsageCapPreflight_FanOutAndSoftCapsPark(t *testing.T) {
	blankAnthropicWireEnv(t)
	opus := func() ir.Node { return agentRoute("opus", delegate.BackendClaudeCode, "", "claude-opus-5-5") }
	glm := func() ir.Node { return agentRoute("glm", delegate.BackendClaudeCode, "", "glm-5.3") }

	hard := usagecap.NewMemStore()
	weekCapped(t, hard, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	if err := preflightFor(t, hard, forfaitBesidePinnedKeys(), fanOutWorkflow(opus(), glm())); err == nil {
		t.Error("a fan-out with a capped branch started — every execution runs that branch")
	}

	soft := usagecap.NewMemStore()
	if err := soft.Record(context.Background(), platformKey("fp-forfait"), usagecap.Reading{
		Window: usagecap.WindowFiveHour, Utilization: 0.95, Status: usagecap.StatusWarning,
		ResetsAt: time.Now().UTC().Add(2 * time.Hour), ObservedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := preflightFor(t, soft, forfaitBesidePinnedKeys(), branchWorkflow(opus(), glm())); err == nil {
		t.Error("a run that may reach a soft-capped route started — a soft cap would let it spend that route uninterrupted")
	}
	if err := preflightFor(t, soft, forfaitBesidePinnedKeys(), chainWorkflow(glm())); err != nil {
		t.Errorf("a run that cannot reach the soft-capped route parked: %v", err)
	}
}
