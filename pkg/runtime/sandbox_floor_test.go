package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #1959 — the operator isolation floor: a `sandbox: none` declared by a bot
// source the operator does not control is neutralized (loudly), while an
// operator-origin source keeps its opt-out.

// floorOptOutWorkflow is a bot whose only sandbox declaration is the opt-out
// the floor exists for.
func floorOptOutWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:    "floor_opt_out",
		Entry:   "done",
		Nodes:   map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
		Sandbox: &ir.SandboxSpec{Mode: "none"},
	}
}

// floorTestEngine builds an engine whose sandbox default is `auto` against a
// working (fake) driver — the cloud posture ADR-082 phase 3 installs — with
// the host dirs pointed at throwaway ones so host_state touches nothing real.
func floorTestEngine(t *testing.T, s store.RunStore, opts ...EngineOption) *Engine {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_HOME", t.TempDir())
	base := []EngineOption{
		WithLogger(iterlog.Nop()),
		WithWorkDir(t.TempDir()),
		WithSandboxDefault("auto"),
		WithSandboxDrivers(workingHostRegistry(t)),
	}
	return New(floorOptOutWorkflow(), s, newStubExecutor(), append(base, opts...)...)
}

func floorEvents(t *testing.T, s store.RunStore, runID string) []*store.Event {
	t.Helper()
	evs, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// The ticket's core witness: a team-authored bot's `sandbox: none` does NOT
// reach the host — the run executes inside the sandbox the operator's tiers
// resolve, and the neutralization is on the run's event stream. Fails
// without the fix: the run settles unsandboxed and no event exists.
func TestSandboxFloorNeutralizesTeamSourceOptOut(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	eng := floorTestEngine(t, s, WithBotSourceTier(store.BotSourceTierTeam))
	if err := eng.Run(ctx, "floor-team", nil); err != nil {
		t.Fatal(err)
	}
	evs := floorEvents(t, s, "floor-team")
	if !hasEventType(evs, store.EventSandboxFloorApplied) {
		t.Fatalf("no sandbox_floor_applied event — the neutralization must be loud; events: %v", evs)
	}
	if !hasEventType(evs, store.EventSandboxStarted) {
		t.Fatalf("no sandbox_started event — the floored run must execute inside a container; events: %v", evs)
	}
	for _, ev := range evs {
		if ev.Type != store.EventSandboxFloorApplied {
			continue
		}
		if ev.Data["tier"] != store.BotSourceTierTeam {
			t.Errorf("floor event tier = %v, want %q", ev.Data["tier"], store.BotSourceTierTeam)
		}
		if ev.Data["overridden_mode"] != "none" {
			t.Errorf("floor event overridden_mode = %v, want none", ev.Data["overridden_mode"])
		}
	}
	// The wire-carried tier is stamped onto the document (the subbot trap
	// WithTrust's docstring names): a later resume re-reads it from there.
	got, err := s.LoadRun(ctx, "floor-team")
	if err != nil {
		t.Fatal(err)
	}
	if got.BotSourceTier != store.BotSourceTierTeam {
		t.Errorf("run BotSourceTier = %q, want %q", got.BotSourceTier, store.BotSourceTierTeam)
	}
}

// The catalog's deliberate opt-out keeps working: bots/copilot resolves to
// the BAKED tier, which the operator shipped — the floor does not speak.
// Same for the platform tier (the operator's own override) and the empty
// tier (the operator's own local launch).
func TestSandboxFloorKeepsOperatorSourceOptOut(t *testing.T) {
	for _, tier := range []string{store.BotSourceTierBaked, store.BotSourceTierPlatform, ""} {
		t.Run("tier="+tier, func(t *testing.T) {
			ctx := context.Background()
			s := tmpStore(t)
			eng := floorTestEngine(t, s, WithBotSourceTier(tier))
			if err := eng.Run(ctx, "floor-operator", nil); err != nil {
				t.Fatal(err)
			}
			evs := floorEvents(t, s, "floor-operator")
			if hasEventType(evs, store.EventSandboxFloorApplied) {
				t.Errorf("tier %q is operator-controlled — the floor must not speak", tier)
			}
			if hasEventType(evs, store.EventSandboxStarted) {
				t.Errorf("tier %q declared sandbox: none — the run must stay unsandboxed", tier)
			}
		})
	}
}

// A tier the vocabulary does not recognise — a hand-edited row, a newer
// replica's value mid-rollout — is NOT the operator's: the floor fails
// closed on it, the same direction the trust floor (#1773) takes for an
// unknown trust class.
func TestSandboxFloorUnknownTierFloors(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	eng := floorTestEngine(t, s, WithBotSourceTier("vendored"))
	if err := eng.Run(ctx, "floor-unknown", nil); err != nil {
		t.Fatal(err)
	}
	if !hasEventType(floorEvents(t, s, "floor-unknown"), store.EventSandboxFloorApplied) {
		t.Fatal("an unrecognized tier must be floored too")
	}
}

// The floor's second carrier: a message too old to carry the tier (pre-v28)
// is backstopped by the run document the publisher already stamps. The
// engine option absent, the document's stamp alone floors the run.
func TestSandboxFloorReadsTheRunDocument(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	eng := floorTestEngine(t, s) // no WithBotSourceTier
	// Pre-create the document the way the cloud publisher does, so Run takes
	// the pickup path and the floor reads the tier from the document.
	if _, err := s.CreateRun(ctx, "floor-doc", "floor_opt_out", nil); err != nil {
		t.Fatal(err)
	}
	run, err := s.LoadRun(ctx, "floor-doc")
	if err != nil {
		t.Fatal(err)
	}
	run.BotSourceTier = store.BotSourceTierTeam
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := eng.Run(ctx, "floor-doc", nil); err != nil {
		t.Fatal(err)
	}
	if !hasEventType(floorEvents(t, s, "floor-doc"), store.EventSandboxFloorApplied) {
		t.Fatal("the document-stamped tier alone must floor the run")
	}
}

// A resume re-applies the floor: the workflow is recompiled from the CURRENT
// bot text, which may still carry the opt-out, and the tier replays from the
// run document. A resume that skipped the floor would rehabilitate the
// opt-out on the second attempt.
func TestSandboxFloorAppliesOnResume(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "floor-resume", "floor_opt_out", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.BotSourceTier = store.BotSourceTierTeam
	run.Status = store.RunStatusFailedResumable
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	eng := floorTestEngine(t, s)
	if err := eng.Resume(ctx, "floor-resume", nil); err != nil {
		t.Fatal(err)
	}
	evs := floorEvents(t, s, "floor-resume")
	if !hasEventType(evs, store.EventSandboxFloorApplied) {
		t.Fatal("a resumed attempt must re-apply the floor")
	}
	if !hasEventType(evs, store.EventSandboxStarted) {
		t.Fatal("the resumed attempt must execute inside a container")
	}
}

// The operator's explicit CLI-strength choice is never silently replaced:
// with the override set, the workflow's opt-out is already dead — and an
// override of none is the operator's own posture, floored by nobody.
func TestSandboxFloorNeverOverridesTheCLIStrengthChoice(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	eng := floorTestEngine(t, s,
		WithBotSourceTier(store.BotSourceTierTeam),
		WithSandboxOverride("none"),
		WithSandboxDrivers(driverlessHostRegistry()))
	if err := eng.Run(ctx, "floor-override", nil); err != nil {
		t.Fatal(err)
	}
	evs := floorEvents(t, s, "floor-override")
	if hasEventType(evs, store.EventSandboxFloorApplied) {
		t.Error("the override is the operator's explicit choice — the floor must stay silent")
	}
	if hasEventType(evs, store.EventSandboxStarted) {
		t.Error("ITERION_SANDBOX_OVERRIDE=none means none, whatever the bot's source")
	}
}

// The escape hatch philosophy §1 requires for a load-bearing limit:
// ITERION_SANDBOX_FLOOR=off restores the pre-floor behaviour for an
// operator who trusts their team tier.
func TestSandboxFloorEscapeHatch(t *testing.T) {
	t.Setenv(SandboxFloorEnvVar, "off")
	ctx := context.Background()
	s := tmpStore(t)
	eng := floorTestEngine(t, s, WithBotSourceTier(store.BotSourceTierTeam))
	if err := eng.Run(ctx, "floor-hatch", nil); err != nil {
		t.Fatal(err)
	}
	evs := floorEvents(t, s, "floor-hatch")
	if hasEventType(evs, store.EventSandboxFloorApplied) {
		t.Error("the floor is off — no neutralization")
	}
	if hasEventType(evs, store.EventSandboxStarted) {
		t.Error("the floor is off — the opt-out is honoured")
	}
}

// The pure decision, pinned so a mutation in any clause fails a test.
func TestApplySandboxFloorDecisionTable(t *testing.T) {
	optOut := func() *ir.Workflow {
		return &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "none"}}
	}
	cases := []struct {
		name     string
		wf       *ir.Workflow
		tier     string
		cli      string
		wantNeut bool
	}{
		{"team opt-out neutralized", optOut(), store.BotSourceTierTeam, "", true},
		{"baked keeps opt-out", optOut(), store.BotSourceTierBaked, "", false},
		{"platform keeps opt-out", optOut(), store.BotSourceTierPlatform, "", false},
		{"empty tier is the operator", optOut(), "", "", false},
		{"unknown tier floors", optOut(), "future-tier", "", true},
		{"cli override is the operator's word", optOut(), store.BotSourceTierTeam, "none", false},
		{"no opt-out, nothing to do", &ir.Workflow{}, store.BotSourceTierTeam, "", false},
		{"sandbox auto is not an opt-out", &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "auto"}}, store.BotSourceTierTeam, "", false},
		{"nil workflow", nil, store.BotSourceTierTeam, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, neut := ApplySandboxFloor(tc.wf, tc.tier, tc.cli)
			if neut != tc.wantNeut {
				t.Fatalf("neutralized = %v, want %v", neut, tc.wantNeut)
			}
			if !neut {
				if tc.wf != nil && got != tc.wf {
					t.Error("no neutralization must return the input workflow, not a copy")
				}
				return
			}
			if got == tc.wf {
				t.Error("a neutralized workflow must be a copy — the launch's compiled IR is not mutated")
			}
			if got.Sandbox.Mode != "" {
				t.Errorf("neutralized block mode = %q, want empty so pickMode falls through", got.Sandbox.Mode)
			}
			if tc.wf.Sandbox.Mode != "none" {
				t.Error("the input workflow was mutated")
			}
			// The neutralized copy resolves from the operator's tiers: with a
			// global default installed, pickMode lands there.
			mode, _ := pickMode(got, "", "auto")
			if mode != "auto" {
				t.Errorf("pickMode after neutralization = %q, want the global default", mode)
			}
		})
	}
}

// The floor composes with the sandbox resolution exactly where the ticket
// names: pickMode's caller. Pinned against a re-resolution from the
// unfloored workflow.
func TestApplySandboxFloorComposesWithResolve(t *testing.T) {
	wf := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "none"}}
	floored, neut := ApplySandboxFloor(wf, store.BotSourceTierTeam, "")
	if !neut {
		t.Fatal("expected neutralization")
	}
	spec, _, _, err := resolveSandboxSpec(floored, t.TempDir(), "", "auto", "example.invalid/sandbox:test")
	if err != nil {
		t.Fatal(err)
	}
	if spec == nil || !spec.Mode.IsActive() {
		t.Fatal("the floored workflow must resolve an active sandbox from the operator's default")
	}
	spec, _, _, err = resolveSandboxSpec(wf, t.TempDir(), "", "auto", "example.invalid/sandbox:test")
	if err != nil {
		t.Fatal(err)
	}
	if spec != nil {
		t.Fatal("the declared opt-out must still resolve to no sandbox — only the floored copy changes")
	}
}
