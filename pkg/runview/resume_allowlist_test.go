package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The local lanes' twin of the cloud resume's providers walk (#3220): a
// resume of a run whose document froze a non-empty providers list reads the
// same gate the launch read — on the dispatch view over the run's own
// frozen overrides, chain and inputs — before anything moves the run. A
// source edited between park and resume cannot add a shape (a subbot, a
// forbidden primary, an unreadable route) the launch's gate never walked.

// resumeAllowlistGateBot is the source the seeded run froze: the human gate
// is the entry, and every edited variant keeps it as entry and puts the
// offending node behind its approval — with no answers passed, a mutant run
// that slips past the gate parks there and dispatches nothing.
const resumeAllowlistGateBot = `schema gate_out:
  approved: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow resume_allowlist:
  entry: gate
  gate -> done when approved
  gate -> fail when not approved
`

// The edited sources: the offending node behind the gate (gate -> X when
// approved), so the refusal must come from the resume gate, not from a node
// that ran.
const resumeAllowlistSubbotBot = `schema gate_out:
  approved: bool

schema vout:
  ok: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

subbot run_child:
  source: "../child/main.bot"
  output: vout

workflow resume_allowlist:
  entry: gate
  gate -> run_child when approved
  run_child -> done
  gate -> fail when not approved
`

const resumeAllowlistOpenAIBot = `schema gate_out:
  approved: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

agent work:
  backend: "claw"
  model: "openai/gpt-5"
  system: p

prompt p:
  Do the work.

workflow resume_allowlist:
  entry: gate
  gate -> work when approved
  work -> done
  gate -> fail when not approved
`

// A prefix-less model on an agent with no backend: the walk cannot say what
// the route spends, and under a non-empty list a lock that cannot be
// checked must not pass.
const resumeAllowlistPrefixlessBot = `schema gate_out:
  approved: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

agent work:
  model: "claude-opus-5"

workflow resume_allowlist:
  entry: gate
  gate -> work when approved
  work -> done
  gate -> fail when not approved
`

// seedAllowlistRun seeds runID as failed_resumable on botPath, its
// WorkflowHash frozen on the gate-only source, plus pol — the shape
// seedChangedWorkflowRun seeds, with the routing policy beside it.
func seedAllowlistRun(t *testing.T, svc *Service, runID, botPath string, pol *store.RunLLMRoutePolicy) {
	t.Helper()
	if _, err := svc.store.CreateRun(context.Background(), runID, "resume_allowlist", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := svc.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	_, hash, err := CompileWorkflowFromSource(botPath, resumeAllowlistGateBot, "", store.RunTrustDefault)
	if err != nil {
		t.Fatalf("compile the frozen source: %v", err)
	}
	r.Status = store.RunStatusFailedResumable
	r.FilePath = botPath
	r.WorkflowHash = hash
	r.LLMRoutePolicy = pol
	if err := svc.store.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

// newAllowlistService builds a service whose runs work in a repository the
// test owns, and pins the environment a mutant run must stay inside: blank
// provider keys, an orphan reconciler that never fires, its own HOME/TMPDIR.
func newAllowlistService(t *testing.T, dir string) *Service {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "0")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	svc, err := NewService(filepath.Join(dir, "store"), WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)), WithSandboxDefault("none"))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { stopService(t, svc) })
	return svc
}

// allowlistDetachedLane flips the resume onto the detached spawn, its
// runner a shell script that records its args into argsFile — the mutant
// signal for "a subprocess was spawned".
func allowlistDetachedLane(t *testing.T, dir string) (argsFile string) {
	t.Helper()
	t.Setenv(envDetached, "1")
	binary := filepath.Join(dir, "iterion")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ITERION_TEST_ARGS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	argsFile = filepath.Join(dir, "args")
	t.Setenv("ITERION_BIN", binary)
	t.Setenv("ITERION_TEST_ARGS", argsFile)
	return argsFile
}

// TestLocalResumeRefusesOutsideTheFrozenProvidersList walks the three
// unreadable-or-forbidden shapes through both local lanes. Mutants: the
// gate call dropped from Service.Resume → every refusal row reddens on its
// typed assertion; the run's inputs not fed to the view →
// TestRefuseResumeOutsideAllowlistReadsTheRunsInputs reddens; its chain
// not fed → TestRefuseResumeOutsideAllowlistReadsTheRunsChain reddens.
func TestLocalResumeRefusesOutsideTheFrozenProvidersList(t *testing.T) {
	for _, lane := range []struct {
		name     string
		detached bool
	}{
		{name: "in process"},
		{name: "detached", detached: true},
	} {
		for _, tc := range []struct {
			name   string
			edited string
			check  func(t *testing.T, err error)
		}{
			{
				name:   "added subbot",
				edited: resumeAllowlistSubbotBot,
				check: func(t *testing.T, err error) {
					t.Helper()
					var unreadable *model.RouteUnreadableError
					if !errors.As(err, &unreadable) {
						t.Fatalf("resume refused err = %v, want the fieldless RouteUnreadableError", err)
					}
					if unreadable.Site != "run_child" || !strings.Contains(unreadable.Error(), "no field exposes") {
						t.Fatalf("refusal = site %q (%v), want the subbot node named as spending a model no field exposes", unreadable.Site, unreadable)
					}
				},
			},
			{
				name:   "primary route outside the list",
				edited: resumeAllowlistOpenAIBot,
				check: func(t *testing.T, err error) {
					t.Helper()
					var outside *model.ProviderOutsideError
					if !errors.As(err, &outside) {
						t.Fatalf("resume refused err = %v, want ProviderOutsideError", err)
					}
					if outside.Site != "work" || outside.Provider != "openai" || outside.Level != "team" {
						t.Fatalf("refusal = site %q provider %q level %q, want work/openai/team", outside.Site, outside.Provider, outside.Level)
					}
				},
			},
			{
				name:   "route the list cannot read",
				edited: resumeAllowlistPrefixlessBot,
				check: func(t *testing.T, err error) {
					t.Helper()
					var unreadable *model.RouteUnreadableError
					if !errors.As(err, &unreadable) {
						t.Fatalf("resume refused err = %v, want the unreadable-route refusal", err)
					}
					if !errors.Is(err, model.ErrRouteUnreadableUnderAllowlist) || unreadable.Field != "model" {
						t.Fatalf("refusal = field %q (%v), want the model field unreadable under the list", unreadable.Field, unreadable)
					}
				},
			},
			{
				// The same forced resume with no policy on the document
				// holds no lock: the gate passes and the run moves — in the
				// detached lane, far enough to spawn its subprocess.
				name:   "no policy on the document",
				edited: resumeAllowlistOpenAIBot,
				check: func(t *testing.T, err error) {
					t.Helper()
					if err != nil {
						t.Fatalf("the gate refused a run with no policy on its document: %v", err)
					}
				},
			},
		} {
			t.Run(lane.name+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				parentPath := filepath.Join(dir, "parent", "main.bot")
				writeFile(t, filepath.Join(dir, "parent"), "main.bot", tc.edited)
				writeFile(t, filepath.Join(dir, "child"), "main.bot", subbotFoldAgent)
				svc := newAllowlistService(t, dir)
				argsFile := ""
				if lane.detached {
					argsFile = allowlistDetachedLane(t, dir)
				} else {
					t.Setenv(envDetached, "0")
				}
				ctx := context.Background()
				const runID = "run-allowlist"
				seedAllowlistRun(t, svc, runID, parentPath, func() *store.RunLLMRoutePolicy {
					if tc.name == "no policy on the document" {
						return nil
					}
					return teamProviders("anthropic")
				}())

				res, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: parentPath, Force: true})
				tc.check(t, err)

				// A refusal is synchronous and moves nothing; the control
				// row moves the run instead.
				if err != nil {
					if res != nil {
						t.Fatalf("a refused resume returned a result (%#v), want nil", res)
					}
					if svc.manager.Active(runID) {
						t.Fatal("a refused resume registered an active run")
					}
					if r, err := svc.store.LoadRun(ctx, runID); err != nil || r.Status != store.RunStatusFailedResumable {
						t.Fatalf("a refused resume moved the run (status %+v, err %v), want failed_resumable unchanged", r, err)
					}
					if lane.detached {
						if _, statErr := os.Stat(argsFile); !os.IsNotExist(statErr) {
							t.Fatalf("a refused detached resume spawned its subprocess (args file: %v, stat err %v)", argsFile, statErr)
						}
					}
					return
				}
				// Control row: the resume moved the run — in the detached
				// lane, far enough to spawn the runner.
				if res == nil {
					t.Fatal("the admitted resume returned no result")
				}
				awaitRunCompletion(t, res.Done, tc.name)
				if lane.detached {
					if _, statErr := os.Stat(argsFile); statErr != nil {
						t.Fatalf("the admitted detached resume spawned no subprocess (args file: %v)", statErr)
					}
				} else if r, err := svc.store.LoadRun(ctx, runID); err != nil || r.Status == store.RunStatusFailedResumable {
					t.Fatalf("the admitted resume left the run failed_resumable (status %+v, err %v)", r, err)
				}
			})
		}
	}
}

// TestPreflightResumeRefusesOutsideTheFrozenProvidersList: the preflight
// answers the same refusal as the resume it mirrors, so a resume the gate
// refuses never consumes the uploads the caller staged. Mutant: the gate
// call dropped from PreflightResume → the first half reddens.
func TestPreflightResumeRefusesOutsideTheFrozenProvidersList(t *testing.T) {
	for _, tc := range []struct {
		name string
		pol  *store.RunLLMRoutePolicy
	}{
		{name: "frozen list", pol: teamProviders("anthropic")},
		{name: "no policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			parentPath := filepath.Join(dir, "parent", "main.bot")
			writeFile(t, filepath.Join(dir, "parent"), "main.bot", resumeAllowlistOpenAIBot)
			writeFile(t, filepath.Join(dir, "child"), "main.bot", subbotFoldAgent)
			svc := newAllowlistService(t, dir)
			t.Setenv(envDetached, "0")
			ctx := context.Background()
			const runID = "run-preflight-allowlist"
			seedAllowlistRun(t, svc, runID, parentPath, tc.pol)
			err := svc.PreflightResume(ctx, ResumeSpec{RunID: runID, FilePath: parentPath, Force: true})
			if tc.pol == nil {
				if err != nil {
					t.Fatalf("preflight refused a run with no policy on its document: %v", err)
				}
				return
			}
			var outside *model.ProviderOutsideError
			if !errors.As(err, &outside) {
				t.Fatalf("preflight err = %v, want the same refusal the resume answers", err)
			}
			if outside.Site != "work" || outside.Provider != "openai" || outside.Level != "team" {
				t.Fatalf("refusal = site %q provider %q level %q, want work/openai/team", outside.Site, outside.Provider, outside.Level)
			}
		})
	}
}

// TestRefuseResumeOutsideAllowlistReadsTheRunsInputs: the gate reads the
// run's OWN inputs the way dispatch does — a declared var answered by the
// run decides the route — so a run the launch admitted (its input names an
// allowed provider) resumes, and one whose input names a forbidden one
// refuses. Mutant: the view built without r.Inputs → the forbidden-input
// half reddens (the admitted half's input equals the declared default).
func TestRefuseResumeOutsideAllowlistReadsTheRunsInputs(t *testing.T) {
	impl := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "{{vars.m}}"}}
	wf := &ir.Workflow{
		Vars: map[string]*ir.Var{
			"m": {Name: "m", Type: ir.VarString, HasDefault: true, Default: "anthropic/claude-opus-5"},
		},
		Nodes: map[string]ir.Node{"impl": impl},
	}
	r := &store.Run{ID: "r", LLMRoutePolicy: teamProviders("anthropic"), Inputs: map[string]any{"m": "anthropic/claude-opus-5"}}
	if err := refuseResumeOutsideAllowlist(wf, r); err != nil {
		t.Fatalf("the run's input names an allowed provider: refused %v, want the resume admitted", err)
	}
	r.Inputs["m"] = "openai/gpt-5"
	var outside *model.ProviderOutsideError
	if err := refuseResumeOutsideAllowlist(wf, r); !errors.As(err, &outside) || outside.Site != "impl" || outside.Provider != "openai" || outside.Level != "team" {
		t.Fatalf("the run's input names openai: err = %v, want the impl/openai/team refusal", err)
	}
	// No policy on the document, and one that names no list, hold no lock.
	r.LLMRoutePolicy = nil
	if err := refuseResumeOutsideAllowlist(wf, r); err != nil {
		t.Fatalf("nil policy: refused %v, want the gate to pass", err)
	}
	r.LLMRoutePolicy = &store.RunLLMRoutePolicy{Sources: map[string]string{}}
	if err := refuseResumeOutsideAllowlist(wf, r); err != nil {
		t.Fatalf("empty list: refused %v, want the gate to pass", err)
	}
}

// TestRefuseResumeOutsideAllowlistReadsTheRunsChain: the gate reads the
// run's OWN fallback chain — a chain stage the list cannot read refuses the
// resume whose primary the list admits. Mutant: the view built without
// r.Fallback → the chain half reddens.
func TestRefuseResumeOutsideAllowlistReadsTheRunsChain(t *testing.T) {
	impl := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"impl": impl}}
	r := &store.Run{ID: "r", LLMRoutePolicy: teamProviders("anthropic")}
	if err := refuseResumeOutsideAllowlist(wf, r); err != nil {
		t.Fatalf("primary on an allowed provider, no chain: refused %v, want the resume admitted", err)
	}
	r.Fallback = store.RunFallback{{Backend: "claw", Model: "claude-opus-5"}}
	var unreadable *model.RouteUnreadableError
	if err := refuseResumeOutsideAllowlist(wf, r); !errors.As(err, &unreadable) {
		t.Fatalf("chain stage the list cannot read: err = %v, want RouteUnreadableError", err)
	}
}

// presetAllowlistBot routes its agent through var m: the declared default
// names an allowed provider, preset cheap names openai, preset same names
// another allowed model.
const presetAllowlistBot = `schema gate_out:
  approved: bool

prompt gate_prompt:
  Approve?

prompt p:
  Do the work.

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

agent work:
  backend: "claw"
  model: "{{vars.m}}"
  system: p

vars:
  m: string = "anthropic/claude-opus-5"

presets:
  cheap:
    m: "openai/gpt-5"
  same:
    m: "anthropic/claude-sonnet-5"

workflow resume_allowlist:
  entry: gate
  gate -> work when approved
  work -> done
  gate -> fail when not approved
`

// TestLaunchGateReadsThePresetValues: the in-process launch gate reads the
// vars the run stores — the preset's values under the launch's — so a
// preset routing a node outside the list refuses at launch, where the
// resume gate (which reads the stored inputs) would refuse it later. A
// preset is the bot author's text: its values narrow as trusted. Mutant:
// the gate fed spec.Vars only → the preset row passes the launch and
// reddens; the preset names not trusted → the "same" row reddens on the
// unreadable late var. A key the launch also sets keeps the launch's own
// trust: an untrusted launch value over a preset key stays late and refuses
// as unreadable. Mutant: every preset key trusted, the launch's own keys
// included → the "launch value over a preset key" row is admitted and
// reddens.
func TestLaunchGateReadsThePresetValues(t *testing.T) {
	for _, tc := range []struct {
		name       string
		preset     string
		vars       map[string]string
		refuse     bool
		unreadable bool
	}{
		{name: "preset routes outside the list", preset: "cheap", refuse: true},
		{name: "preset routes inside the list", preset: "same"},
		{name: "no preset"},
		{name: "untrusted launch value over a preset key", preset: "same", vars: map[string]string{"m": "anthropic/claude-opus-5"}, refuse: true, unreadable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			botPath := filepath.Join(dir, "parent", "main.bot")
			writeFile(t, filepath.Join(dir, "parent"), "main.bot", presetAllowlistBot)
			svc := newAllowlistService(t, dir)
			t.Setenv(envDetached, "0")
			res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, LLMRoutePolicy: teamProviders("anthropic"), Preset: tc.preset, Vars: tc.vars})
			if !tc.refuse {
				if err != nil {
					t.Fatalf("launch refused %v, want admitted", err)
				}
				awaitRunCompletion(t, res.Done, tc.name)
				return
			}
			if tc.unreadable {
				var unreadable *model.RouteUnreadableError
				if !errors.As(err, &unreadable) {
					if res != nil {
						awaitRunCompletion(t, res.Done, tc.name)
					}
					t.Fatalf("launch err = %v, want an untrusted launch value over a preset key refused as unreadable", err)
				}
				if unreadable.Site != "work" || unreadable.Field != "model" {
					t.Fatalf("unreadable refusal = site %q field %q, want work/model", unreadable.Site, unreadable.Field)
				}
				return
			}
			var outside *model.ProviderOutsideError
			if !errors.As(err, &outside) {
				if res != nil {
					awaitRunCompletion(t, res.Done, tc.name)
				}
				t.Fatalf("launch err = %v, want the preset's openai route refused at launch", err)
			}
			if outside.Site != "work" || outside.Provider != "openai" || outside.Level != "team" {
				t.Fatalf("refusal = site %q provider %q level %q, want work/openai/team", outside.Site, outside.Provider, outside.Level)
			}
		})
	}
}

// TestRefusedResumeLeavesTheBudgetUntouched: the gate runs before the
// resume's budget writes, so a refused resume that raised its budget
// persists neither the merged ask nor the snapshot. Mutant: the gate moved
// past the budget writes → the reload carries MaxCostUSD 42.
func TestRefusedResumeLeavesTheBudgetUntouched(t *testing.T) {
	dir := t.TempDir()
	parentPath := filepath.Join(dir, "parent", "main.bot")
	writeFile(t, filepath.Join(dir, "parent"), "main.bot", resumeAllowlistOpenAIBot)
	svc := newAllowlistService(t, dir)
	t.Setenv(envDetached, "0")
	ctx := context.Background()
	const runID = "run-allowlist-budget"
	seedAllowlistRun(t, svc, runID, parentPath, teamProviders("anthropic"))
	res, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: parentPath, Force: true, Budget: &ir.BudgetOverrides{MaxCostUSD: 42}})
	var outside *model.ProviderOutsideError
	if !errors.As(err, &outside) {
		if res != nil {
			awaitRunCompletion(t, res.Done, "budget")
		}
		t.Fatalf("resume err = %v, want ProviderOutsideError", err)
	}
	r, err := svc.store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.BudgetOverrides != nil || r.Budget != nil {
		t.Fatalf("a refused resume persisted a budget: overrides=%+v snapshot=%+v", r.BudgetOverrides, r.Budget)
	}
}

// TestPreflightResumeRefusesOutsideTheListOnAPublisher: on a deployment
// that publishes, the preflight walks the run's frozen list and refuses with
// the typed ProviderOutsideError before the caller's staged uploads are
// consumed. SubmitResume's replay of the frozen list is pinned on its own
// side (TestProviders_ResumeReplaysTheFrozenList in pkg/server/cloudpublisher);
// this test does not compare the two answers. Mutant: the walk
// guarded on a nil publisher in PreflightResume → the preflight passes and
// this test reddens.
func TestPreflightResumeRefusesOutsideTheListOnAPublisher(t *testing.T) {
	dir := t.TempDir()
	parentPath := filepath.Join(dir, "parent", "main.bot")
	writeFile(t, filepath.Join(dir, "parent"), "main.bot", resumeAllowlistOpenAIBot)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "0")
	pub := &resumeHashPublisher{}
	svc, err := NewService(filepath.Join(dir, "store"), WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)), WithSandboxDefault("none"), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { stopService(t, svc) })
	const runID = "run-preflight-publisher"
	seedAllowlistRun(t, svc, runID, parentPath, teamProviders("anthropic"))
	err = svc.PreflightResume(context.Background(), ResumeSpec{RunID: runID, FilePath: parentPath, Force: true})
	var outside *model.ProviderOutsideError
	if !errors.As(err, &outside) {
		t.Fatalf("publisher preflight err = %v, want ProviderOutsideError", err)
	}
	if outside.Site != "work" || outside.Provider != "openai" || outside.Level != "team" {
		t.Fatalf("refusal = site %q provider %q level %q, want work/openai/team", outside.Site, outside.Provider, outside.Level)
	}
}
