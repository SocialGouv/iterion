package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The operator's launch-time model/backend pins must ride the cloud wire
// AND land on the run doc: the runner builds its own executor, so a pin
// persisted display-only is an override the studio shows but the
// delegates never honour. Falsified both ways: entries travel to both
// carriers; an override-less launch publishes byte-identical messages
// (nil field); a resume replays the run doc's pins.

func TestSubmitLaunchCarriesModelOverrides(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.LaunchSpec{
		FilePath: "wf.bot",
		Source:   "workflow wf:\n  start -> done\n",
		ModelOverrides: []runview.ModelOverrideEntry{
			{Selector: "agent", Backend: "claude_code", Model: "claude-fable-5"},
			{Selector: "judge", Model: "claude-opus-5"},
		},
	}
	if _, err := p.SubmitLaunch(ctx, "run-mo-1", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if published == nil {
		t.Fatal("nothing published")
	}
	if len(published.ModelOverrides) != 2 ||
		published.ModelOverrides[0] != (queue.ModelOverride{Selector: "agent", Backend: "claude_code", Model: "claude-fable-5"}) ||
		published.ModelOverrides[1] != (queue.ModelOverride{Selector: "judge", Model: "claude-opus-5"}) {
		t.Fatalf("message overrides = %+v, want the two launch pins verbatim", published.ModelOverrides)
	}
	r, err := st.LoadRun(ctx, "run-mo-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(r.ModelOverrides) != 2 || r.ModelOverrides[0].Model != "claude-fable-5" || r.ModelOverrides[1].Selector != "judge" {
		t.Fatalf("run doc overrides = %+v, want the two pins persisted for display + resume replay", r.ModelOverrides)
	}
}

func TestSubmitLaunchWithoutOverridesPublishesNone(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}
	if _, err := p.SubmitLaunch(ctx, "run-mo-2", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if published == nil || published.ModelOverrides != nil {
		t.Fatalf("override-less launch published %+v, want nil (older consumers stay byte-identical)", published.ModelOverrides)
	}
	if r, _ := st.LoadRun(ctx, "run-mo-2"); r != nil && r.ModelOverrides != nil {
		t.Fatalf("override-less launch persisted %+v, want none", r.ModelOverrides)
	}
}

func TestSubmitResumeReplaysRunDocOverrides(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	const runID = "run-mo-resume"
	if err := st.SaveRun(ctx, &store.Run{
		ID:       runID,
		TenantID: "team-a",
		OwnerID:  "u1",
		Status:   store.RunStatusFailedResumable,
		ModelOverrides: []store.RunModelOverride{
			{Selector: "agent", Backend: "claude_code", Model: "claude-fable-5"},
		},
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}
	if err := p.SubmitResume(ctx, spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if published == nil {
		t.Fatal("nothing published")
	}
	if len(published.ModelOverrides) != 1 ||
		published.ModelOverrides[0] != (queue.ModelOverride{Selector: "agent", Backend: "claude_code", Model: "claude-fable-5"}) {
		t.Fatalf("resume overrides = %+v, want the launch pin replayed from the run doc", published.ModelOverrides)
	}
}

// The pool's wants-derivation must read the launch-time model overrides,
// not only the DSL. #668: a bot with an agent + judge, both pinned to
// claw/openai by launch-time overrides, still had the pool tier walk on
// "anthropic" (the DSL value) because wantsFor read only the raw model
// field. A run that resolved no credential of its own then asked for a
// donation on the wrong wire, held a slot, and every retry re-picked
// the same wrong route.
//
// The fix is a shared helper (effectiveNodeTargeting) that applies
// overrides once and every node-walk in the file reads it. This test
// pins the helper's contract on the ONLY caller the pool tier has —
// wantsFor — because that's the site the incident traced back to.
func TestWantsFor_HonoursLaunchTimeModelOverrides(t *testing.T) {
	provider := func(w credpool.Credential) string {
		if w.Source == credpool.SourceAPIKey {
			return w.Ref
		}
		switch secrets.OAuthKind(w.Ref) {
		case secrets.OAuthKindClaudeCode:
			return "anthropic"
		case secrets.OAuthKindCodex:
			return "openai"
		}
		return ""
	}
	provs := func(ws []credpool.Credential) []string {
		out := make([]string, 0, len(ws))
		for _, w := range ws {
			if p := provider(w); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	contains := func(list []string, x string) bool {
		for _, s := range list {
			if s == x {
				return true
			}
		}
		return false
	}

	// The exact scenario from the ticket: two-node DAG, both pinned by
	// launch-time overrides to claw/openai. The DSL says "anthropic/…".
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"oracle_campaign": &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "oracle_campaign"},
			LLMFields: ir.LLMFields{Model: "anthropic/claude-opus-4"},
		},
		"mutants_adversary": &ir.JudgeNode{
			BaseNode:  ir.BaseNode{ID: "mutants_adversary"},
			LLMFields: ir.LLMFields{Model: "anthropic/claude-sonnet-4"},
		},
	}}

	t.Run("both nodes pinned to openai/claw by override → no anthropic want", func(t *testing.T) {
		var overrides model.ModelOverrides
		overrides.SetBackend("oracle_campaign", "claw")
		overrides.SetModel("oracle_campaign", "openai/gpt-5.6-sol")
		overrides.SetProvider("oracle_campaign", "openai")
		overrides.SetBackend("mutants_adversary", "claw")
		overrides.SetModel("mutants_adversary", "openai/gpt-5.6-sol")
		overrides.SetProvider("mutants_adversary", "openai")

		w, _ := wantsFor(wf, overrides, nil)
		got := provs(w)
		if contains(got, "anthropic") {
			t.Fatalf("wants still asks for anthropic: %v — the judge-kind override is not reaching wantsFor", got)
		}
		if !contains(got, "openai") {
			t.Fatalf("wants must ask for openai (both nodes pinned to it): got %v", got)
		}
	})

	t.Run("kind selector: 'judge' override still routes wants to openai", func(t *testing.T) {
		// The ticket's minimal shape: agent node's DSL is unchanged
		// (anthropic), but the JUDGE is pinned to openai via a kind
		// selector. Wants must reflect what each node actually runs on.
		var overrides model.ModelOverrides
		overrides.SetProvider("judge", "openai")
		overrides.SetModel("judge", "openai/gpt-5")

		w, _ := wantsFor(wf, overrides, nil)
		got := provs(w)
		// The agent still wants anthropic (unchanged), but the judge now
		// contributes openai — the mixed set the run actually needs.
		if !contains(got, "anthropic") || !contains(got, "openai") {
			t.Fatalf("wants = %v, want both anthropic (agent unchanged) and openai (judge overridden)", got)
		}
	})

	t.Run("empty overrides → identical to the pre-fix DSL walk", func(t *testing.T) {
		w, _ := wantsFor(wf, model.ModelOverrides{}, nil)
		got := provs(w)
		if !contains(got, "anthropic") {
			t.Fatalf("wants without overrides must still see the DSL pin: got %v", got)
		}
		if contains(got, "openai") {
			t.Fatalf("wants without overrides must NOT ask for openai: got %v", got)
		}
	})

	t.Run("wants=0 route: a pin that matches no known provider fails OPEN", func(t *testing.T) {
		// Round 1 G2: a pin the pool does not know (fake-provider,
		// unknown ${VAR}, "auto") must NOT narrow the wants to nothing.
		// The pre-round-1 code did narrow — a bot's provider-hint typo
		// silently skipped the pool tier — the exact regression the
		// review caught end-to-end on secured-renovacy. Widen instead:
		// EffectiveProviders returns narrowSafe=false, wantsFor falls
		// back to the full order.
		wfFake := &ir.Workflow{Nodes: map[string]ir.Node{
			"a": &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "a"},
				LLMFields: ir.LLMFields{Model: "fake-provider/gpt-x"},
			},
		}}
		got, _ := wantsFor(wfFake, model.ModelOverrides{}, nil)
		if len(got) != len(poolWantOrder) {
			t.Fatalf("unknown-provider pin narrowed wants to %d, want the full %d (fail open)", len(got), len(poolWantOrder))
		}
	})
}
