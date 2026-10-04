package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

func routeResolveServer(t *testing.T, rec *platformcfg.PlatformCredentials) *Server {
	t.Helper()
	return New(Config{
		SkipProjectRegistration:     true,
		PlatformCredentialsSettings: platformcfg.NewMemoryStore[platformcfg.PlatformCredentials](),
		PlatformCredentialsResolver: platformcfg.NewResolver(rec0(rec), nil),
	}, iterlog.New(iterlog.LevelError, nil))
}

func rec0(rec *platformcfg.PlatformCredentials) platformcfg.Store[platformcfg.PlatformCredentials] {
	st := platformcfg.NewMemoryStore[platformcfg.PlatformCredentials]()
	if rec != nil {
		_ = st.Put(context.Background(), *rec)
	}
	return st
}

// The snapshot answers with the platform record's values and names it: a
// run doc that cannot say WHY it routes is an unauditable one.
func TestResolveRunLLMRoutePolicy_PlatformRecordWinsAndNamesItself(t *testing.T) {
	park := llmroute.RefusedPinnedPark
	s := routeResolveServer(t, &platformcfg.PlatformCredentials{Routing: &llmroute.Policy{
		PairOrder:        []string{llmroute.Pair(llmroute.HarnessClaw, "anthropic_key")},
		RefusedPinnedKey: park,
	}})
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == nil {
		t.Fatal("snapshot = nil")
	}
	if len(got.PairOrder) != 1 || got.PairOrder[0] != "claw+anthropic_key" {
		t.Fatalf("pair_order = %v, want the record's", got.PairOrder)
	}
	if got.RefusedPinnedKey != park {
		t.Fatalf("refused_pinned_key = %q, want the record's park", got.RefusedPinnedKey)
	}
	if got.Sources["refused_pinned_key"] != llmroute.SourcePlatform {
		t.Fatalf("refused_pinned_key provenance = %q, want platform", got.Sources["refused_pinned_key"])
	}
	if got.Sources["triggers"] != llmroute.SourceDefault {
		t.Fatalf("triggers provenance = %q, want default", got.Sources["triggers"])
	}
	if len(got.Triggers) != len(llmroute.Triggers) {
		t.Fatalf("triggers = %v, want the whole vocabulary", got.Triggers)
	}
}

// The env dial answers where the record is silent, and its provenance says
// env — "was it the DB record or the deployment env?" must be answerable
// from the snapshot alone.
func TestResolveRunLLMRoutePolicy_EnvDialAnswersWithItsOwnProvenance(t *testing.T) {
	t.Setenv(llmroute.EnvRefusedPinnedKey, llmroute.RefusedPinnedPark)
	s := routeResolveServer(t, nil)
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.RefusedPinnedKey != llmroute.RefusedPinnedPark {
		t.Fatalf("refused_pinned_key = %q, want the env dial's park", got.RefusedPinnedKey)
	}
	if got.Sources["refused_pinned_key"] != llmroute.SourceEnv {
		t.Fatalf("refused_pinned_key provenance = %q, want env", got.Sources["refused_pinned_key"])
	}
}

// A launch-site layer (the webhook's own policy, slice 3's bot/run
// levels) outranks the platform record — the `higher` parameter is the
// shape slices 2–3 extend.
func TestResolveRunLLMRoutePolicy_HigherLayerOutranksThePlatform(t *testing.T) {
	park := llmroute.RefusedPinnedPark
	s := routeResolveServer(t, &platformcfg.PlatformCredentials{Routing: &llmroute.Policy{
		RefusedPinnedKey: park,
	}})
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "", llmroute.Layer{
		Source: llmroute.SourceBot,
		Policy: llmroute.Policy{PairOrder: []string{llmroute.Pair(llmroute.HarnessCodex, llmroute.CredChatGPTForfait)}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got.PairOrder) != 1 || got.PairOrder[0] != "codex+chatgpt_forfait" {
		t.Fatalf("pair_order = %v, want the bot layer's", got.PairOrder)
	}
	if got.Sources["pair_order"] != llmroute.SourceBot {
		t.Fatalf("pair_order provenance = %q, want bot", got.Sources["pair_order"])
	}
	if got.RefusedPinnedKey != park {
		t.Fatalf("refused_pinned_key = %q, want the platform's park (the bot layer set nothing)", got.RefusedPinnedKey)
	}
}

// The snapshot shape never loses an empty trigger answer: the ceiling's
// "never switch" must survive the store round-trip (triggers is not
// omitempty on the snapshot — a JSON round-trip keeps the []).
func TestRunLLMRoutePolicy_EmptyTriggersSurviveJSON(t *testing.T) {
	in := store.RunLLMRoutePolicy{Triggers: []string{}, RefusedPinnedKey: llmroute.RefusedPinnedForfait}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out store.RunLLMRoutePolicy
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Triggers == nil {
		t.Fatalf("triggers = nil after a JSON round-trip of %s — omitempty erased the empty answer", b)
	}
}

// A launcher built without a resolver (tests, embedders) leaves the field
// nil: the snapshot is simply absent and the consumer applies the package
// defaults — the same tolerance retryPolicyFor states.
func TestServiceLauncher_routePolicyForToleratesNoResolver(t *testing.T) {
	l := &serviceLauncher{}
	if got, err := l.routePolicyFor(context.Background(), trigger.LaunchPlan{}); got != nil || err != nil {
		t.Fatalf("routePolicyFor = %+v, %v — want nil, nil without a resolver", got, err)
	}
	want := &store.RunLLMRoutePolicy{RefusedPinnedKey: llmroute.RefusedPinnedPark}
	l2 := &serviceLauncher{resolveRoute: func(context.Context, string, string, ...llmroute.Layer) (*store.RunLLMRoutePolicy, error) {
		return want, nil
	}}
	if got, err := l2.routePolicyFor(context.Background(), trigger.LaunchPlan{TenantID: "t1", BotID: "b1"}); err != nil || got != want {
		t.Fatalf("routePolicyFor = %+v, %v — want the resolver's snapshot", got, err)
	}
}

// The RUN level (ADR-121 §0): the launcher's own policy, folded as the
// HEAD layer — it outranks the platform record, and its explicit lock is
// the one seat that reopens strict (the marker slice 2 reserved).
func TestResolveRunLLMRoutePolicy_RunLevelOutranksAndReopens(t *testing.T) {
	yes := true
	rec := &platformcfg.PlatformCredentials{Routing: &llmroute.Policy{Strict: &yes}}
	s := routeResolveServer(t, rec)
	no := false
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "", llmroute.Layer{
		Source:   llmroute.SourceRun,
		Launcher: true,
		Policy:   llmroute.Policy{Strict: &no, Locks: []string{llmroute.FieldStrict}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Strict {
		t.Fatalf("strict = true, want false — the launcher's explicit lock is the one unset path, for their own run")
	}
	if got.Sources["strict"] != llmroute.SourceRun {
		t.Fatalf("strict provenance = %q, want run", got.Sources["strict"])
	}

	// WITHOUT the lock the author's true wins over the launcher's false:
	// the launcher steers, the author's requirement stands.
	got, err = s.resolveRunLLMRoutePolicy(context.Background(), "", "", llmroute.Layer{
		Source:   llmroute.SourceRun,
		Launcher: true,
		Policy:   llmroute.Policy{Strict: &no},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.Strict {
		t.Fatalf("strict = false, want true — no lock, no unset: the platform's true beats the run's false")
	}
	if got.Sources["strict"] != llmroute.SourcePlatform {
		t.Fatalf("strict provenance = %q, want platform", got.Sources["strict"])
	}
}
