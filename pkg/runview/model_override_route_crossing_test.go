package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A route-scoped cell's `backend:` re-pins an authored `fallbacks:` route,
// so it meets the refusals the compiler gives the same route written in the
// `.bot` (C176, C135): at launch on the folded list, at the write screen as
// a warning.

const routeCrossingTail = `
prompt p:
  hi

workflow w:
  entry: x
  x -> done
`

// routeCrossingSnapshot is a routing block as a launch stores it:
// validated by the write surfaces' validator, resolved at one level.
func routeCrossingSnapshot(t *testing.T, cells ...llmroute.Override) *store.RunLLMRoutePolicy {
	t.Helper()
	block := llmroute.Policy{Overrides: cells}
	if err := llmroute.Validate(block); err != nil {
		t.Fatalf("the write gate refused the cells: %v", err)
	}
	pol, src := llmroute.Resolve(llmroute.Layer{Source: llmroute.SourceWebhook, Policy: block})
	out := &store.RunLLMRoutePolicy{Sources: src}
	for _, o := range pol.Overrides {
		out.Overrides = append(out.Overrides, store.RunLLMRouteOverride{Selector: o.Selector, Backend: o.Backend, Model: o.Model, Provider: o.Provider, Effort: o.Effort})
	}
	return out
}

// compileRouteCrossingBot compiles src the way a launch compiles it and
// returns the workflow, its path and the compile error.
func compileRouteCrossingBot(t *testing.T, src string) (*ir.Workflow, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.bot")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, _, _, err := compileForLaunch(path, src, "", "", "", AnalysisTrust)
	return wf, path, err
}

// routeCrossingCase is one guard, isolated: inherits is the `.bot` whose
// route `cheap` inherits the node's backend, authored the same `.bot` with
// the route's crossing backend written in it (the compiler's oracle), and
// the cell re-pins the inheriting route onto that backend.
type routeCrossingCase struct {
	name     string
	inherits string
	authored string
	cell     llmroute.Override
	diag     string // the compiler's code on the authored twin
	reason   string // the fragment the compiler's reason and the screen's share
}

func routeCrossingCases() []routeCrossingCase {
	return []routeCrossingCase{
		{
			name: "ungated",
			inherits: "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  permission: deny\n  tools: [read_file]\n" +
				"  fallbacks:\n    cheap:\n      model: \"claude-sonnet-5\"\n" + routeCrossingTail,
			authored: "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  permission: deny\n  tools: [read_file]\n" +
				"  fallbacks:\n    cheap:\n      backend: \"codex\"\n      model: \"gpt-5.4\"\n" + routeCrossingTail,
			cell:   llmroute.Override{Selector: "tb/x#cheap", Backend: "codex", Model: "gpt-5.4"},
			diag:   "[C176]",
			reason: "UNGATED",
		},
		{
			name: "tools inversion",
			inherits: "agent x:\n  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  system: p\n  tools: [read_file]\n" +
				"  fallbacks:\n    cheap:\n      model: \"anthropic/claude-sonnet-5\"\n" + routeCrossingTail,
			authored: "agent x:\n  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  system: p\n  tools: [read_file]\n" +
				"  fallbacks:\n    cheap:\n      backend: \"claude_code\"\n      model: \"claude-sonnet-5\"\n" + routeCrossingTail,
			cell:   llmroute.Override{Selector: "tb/x#cheap", Backend: "claude_code", Model: "claude-sonnet-5"},
			diag:   "[C176]",
			reason: "routes a claw node to a CLI backend",
		},
		{
			name: "session continuity",
			inherits: "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file]\n  session: persist\n" +
				"  fallbacks:\n    cheap:\n      model: \"claude-sonnet-5\"\n" + routeCrossingTail,
			authored: "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file]\n  session: persist\n" +
				"  fallbacks:\n    cheap:\n      backend: \"claw\"\n      model: \"anthropic/claude-sonnet-5\"\n" + routeCrossingTail,
			cell:   llmroute.Override{Selector: "tb/x#cheap", Backend: "claw", Model: "anthropic/claude-sonnet-5"},
			diag:   "[C176]",
			reason: "session continuity has no cross-backend meaning",
		},
		{
			name: "unresolvable tools",
			inherits: "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file, run_command]\n" +
				"  fallbacks:\n    cheap:\n      model: \"claude-sonnet-5\"\n" + routeCrossingTail,
			authored: "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file, run_command]\n" +
				"  fallbacks:\n    cheap:\n      backend: \"claw\"\n      model: \"anthropic/claude-sonnet-5\"\n" + routeCrossingTail,
			cell:   llmroute.Override{Selector: "tb/x#cheap", Backend: "claw", Model: "anthropic/claude-sonnet-5"},
			diag:   "[C135]",
			reason: `"run_command"`,
		},
	}
}

// TestValidateModelOverridePermissions_ARouteCellMeetsTheCompilersCrossingScreens
// runs one scenario per guard: the compiler refuses the authored twin with
// the reason, and the same crossing reached through a route cell is
// refused at launch and warned at the write screen with the same reason.
func TestValidateModelOverridePermissions_ARouteCellMeetsTheCompilersCrossingScreens(t *testing.T) {
	for _, tc := range routeCrossingCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, _, aerr := compileRouteCrossingBot(t, tc.authored)
			if aerr == nil || !strings.Contains(aerr.Error(), tc.diag) || !strings.Contains(aerr.Error(), tc.reason) {
				t.Fatalf("authored twin: compile error = %v, want %s naming %q — the oracle is not the guard this case isolates", aerr, tc.diag, tc.reason)
			}
			wf, path, err := compileRouteCrossingBot(t, tc.inherits)
			if err != nil {
				t.Fatalf("the inheriting bot must compile: %v", err)
			}
			pol := routeCrossingSnapshot(t, tc.cell)
			_, ferr := foldAndScreen(wf, &LaunchSpec{FilePath: path, Source: tc.inherits, BotID: "tb", LLMRoutePolicy: pol})
			if ferr == nil {
				t.Fatalf("the launch admitted a route cell the compiler refuses as %s (%s)", tc.diag, tc.reason)
			}
			for _, want := range []string{tc.reason, `"x#cheap"`} {
				if !strings.Contains(ferr.Error(), want) {
					t.Errorf("launch refusal = %q, want it to name %q", ferr, want)
				}
			}
			warnings, werr := ScreenPolicyOverrides(path, tc.inherits, "", "", llmroute.BotIdentity{ID: "tb"}, pol)
			if werr != nil {
				t.Fatalf("the write screen errored: %v", werr)
			}
			if joined := strings.Join(warnings, "\n"); !strings.Contains(joined, tc.reason) || !strings.Contains(joined, `"x#cheap"`) {
				t.Errorf("write screen warnings = %q, want the crossing named (%q on \"x#cheap\")", warnings, tc.reason)
			}
		})
	}
}

// TestValidateModelOverridePermissions_ALaunchRowRouteKeyIsScreened: a
// launch's own row on a route key reaches ForRoute as a policy cell does,
// and the screen BuildExecutor runs on every plane judges it.
func TestValidateModelOverridePermissions_ALaunchRowRouteKeyIsScreened(t *testing.T) {
	tc := routeCrossingCases()[0]
	wf, _, err := compileRouteCrossingBot(t, tc.inherits)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rows := []ModelOverrideEntry{{Selector: "x#cheap", Backend: "codex", Model: "gpt-5.4"}}
	err = ValidateModelOverridePermissions(wf, toModelOverrides(rows), "")
	if err == nil || !strings.Contains(err.Error(), "UNGATED") || !strings.Contains(err.Error(), `"x#cheap"`) {
		t.Fatalf("launch row on a route key: got %v, want an UNGATED refusal naming \"x#cheap\"", err)
	}
}

// TestValidateModelOverridePermissions_ARouteNoCellRepinsIsNotJudged: the
// route screen judges the backend a route-scoped override pins. A gated
// node's route that inherits the node's backend, under a cell that pins
// only the node's model, crosses nothing and launches.
func TestValidateModelOverridePermissions_ARouteNoCellRepinsIsNotJudged(t *testing.T) {
	tc := routeCrossingCases()[0]
	wf, path, err := compileRouteCrossingBot(t, tc.inherits)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	pol := routeCrossingSnapshot(t, llmroute.Override{Selector: "tb/x", Model: "claude-sonnet-5"})
	if _, ferr := foldAndScreen(wf, &LaunchSpec{FilePath: path, Source: tc.inherits, BotID: "tb", LLMRoutePolicy: pol}); ferr != nil {
		t.Fatalf("launch refused %v — no cell re-pins the route's backend", ferr)
	}
}

// A route cell's crossing on a node whose backend a dial decides: the
// launch (on the cloud lane the server, not the dispatching runner) judges
// the arms that read the node's backend on the dial's default and warns,
// the same text whatever its own environment says; the arms that never
// read the node's backend refuse; BuildExecutor's reading, the dispatching
// process's own, refuses on that process's environment.
func TestRouteCellCrossing_ADialedNodeBackendIsTheRunnersToDecide(t *testing.T) {
	const dial = "ITERION_ROUTEENV_WITNESS_B"
	cases := []struct {
		name, src string
		cells     []llmroute.Override
		// decided: refused at launch whatever the dial says.
		decided bool
		reason  string
		// clawServes: the dispatch reading passes when the dial says claw.
		clawServes bool
		// movesWithTheNode: the arm reads the node's backend, so a cell
		// moving the node with the route clears it.
		movesWithTheNode bool
	}{
		{
			name: "tools inversion on the node's own dial",
			src: "agent x:\n  backend: \"${" + dial + ":-claude_code}\"\n  model: \"claude-opus-5\"\n  system: p\n" +
				"  fallbacks:\n    cheap:\n      model: \"claude-sonnet-5\"\n" + routeCrossingTail,
			cells:      []llmroute.Override{{Selector: "tb/x#cheap", Backend: "claw", Model: "anthropic/claude-sonnet-5"}},
			reason:     "crosses the claw⇄CLI boundary",
			clawServes: true,
		},
		{
			name: "tools inversion on a dialed default_backend",
			src: "agent x:\n  model: \"claude-opus-5\"\n  system: p\n" +
				"  fallbacks:\n    cheap:\n      model: \"claude-sonnet-5\"\n" +
				"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  default_backend: \"${" + dial + ":-claude_code}\"\n  x -> done\n",
			cells:      []llmroute.Override{{Selector: "tb/x#cheap", Backend: "claw", Model: "anthropic/claude-sonnet-5"}},
			reason:     "crosses the claw⇄CLI boundary",
			clawServes: true,
		},
		{
			name: "session continuity on the node's own dial",
			src: "agent x:\n  backend: \"${" + dial + ":-claude_code}\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file]\n  session: persist\n" +
				"  fallbacks:\n    cheap:\n      model: \"claude-sonnet-5\"\n" + routeCrossingTail,
			cells:      []llmroute.Override{{Selector: "tb/x#cheap", Backend: "claw", Model: "anthropic/claude-sonnet-5"}},
			reason:     "session continuity has no cross-backend meaning",
			clawServes: true,
		},
		{
			name: "ungated route on a dialed node stays decided",
			src: "agent x:\n  backend: \"${" + dial + ":-claude_code}\"\n  model: \"claude-opus-5\"\n  system: p\n  permission: deny\n  tools: [read_file]\n" +
				"  fallbacks:\n    cheap:\n      model: \"claude-sonnet-5\"\n" + routeCrossingTail,
			cells:   []llmroute.Override{{Selector: "tb/x#cheap", Backend: "codex", Model: "gpt-5.4"}},
			decided: true,
			reason:  "UNGATED",
		},
		{
			name: "unresolvable tools on a dialed node stays decided",
			src: "agent x:\n  backend: \"${" + dial + ":-claude_code}\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file, run_command]\n" +
				"  fallbacks:\n    cheap:\n      model: \"claude-sonnet-5\"\n" + routeCrossingTail,
			cells:   []llmroute.Override{{Selector: "tb/x#cheap", Backend: "claw", Model: "anthropic/claude-sonnet-5"}},
			decided: true,
			reason:  `"run_command"`,
		},
		{
			name: "a node cell's backend decides the dialed node",
			src: "agent x:\n  backend: \"${" + dial + ":-claw}\"\n  model: \"anthropic/claude-opus-5\"\n  system: p\n" +
				"  fallbacks:\n    cheap:\n      model: \"anthropic/claude-sonnet-5\"\n" + routeCrossingTail,
			cells: []llmroute.Override{
				{Selector: "tb/x", Backend: "claude_code", Model: "claude-opus-5"},
				{Selector: "tb/x#cheap", Backend: "claw", Model: "anthropic/claude-sonnet-5"},
			},
			decided:          true,
			reason:           "crosses the claw⇄CLI boundary",
			movesWithTheNode: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol := routeCrossingSnapshot(t, tc.cells...)
			var launch []string
			for _, env := range []string{"", "claw"} {
				t.Setenv(dial, env)
				if env == "" {
					os.Unsetenv(dial)
				}
				wf, path, err := compileRouteCrossingBot(t, tc.src)
				if err != nil {
					t.Fatalf("the bot must compile: %v", err)
				}
				spec := &LaunchSpec{FilePath: path, Source: tc.src, BotID: "tb", LLMRoutePolicy: pol}
				warnings, ferr := foldAndScreen(wf, spec)
				if tc.decided {
					if ferr == nil || !strings.Contains(ferr.Error(), tc.reason) {
						t.Fatalf("launch env %s=%q: err = %v, want a refusal naming %q — this arm does not read the dial", dial, env, ferr, tc.reason)
					}
					if tc.movesWithTheNode && !strings.Contains(ferr.Error(), "move the node with the route") {
						t.Fatalf("launch refusal %q, want the remedy a routing cell can write", ferr)
					}
					continue
				}
				if ferr != nil {
					t.Fatalf("launch env %s=%q: refused %v — the launching process's environment decided a crossing the runner's decides", dial, env, ferr)
				}
				var got []string
				for _, w := range warnings {
					if strings.Contains(w, tc.reason) && strings.Contains(w, `"x#cheap"`) {
						got = append(got, w)
					}
				}
				if len(got) == 0 {
					t.Fatalf("launch env %s=%q: warnings %q, want the crossing read on the dial's default", dial, env, warnings)
				}
				if joined := strings.Join(got, "\n"); !strings.Contains(joined, dial) || !strings.Contains(joined, "move the node with the route") {
					t.Fatalf("launch warning %q, want the dial named and the remedy a routing cell can write", joined)
				}
				launch = append(launch, strings.Join(got, "\n"))
				// The dispatching process's reading refuses on ITS env.
				derr := ValidateModelOverridePermissions(wf, toModelOverrides(spec.ModelOverrides), "")
				if env == "" && (derr == nil || !strings.Contains(derr.Error(), tc.reason)) {
					t.Fatalf("dispatch reading, %s unset: %v, want the refusal %q", dial, derr, tc.reason)
				}
				if env == "claw" && tc.clawServes && derr != nil {
					t.Fatalf("dispatch reading, %s=claw: %v, want no crossing", dial, derr)
				}
			}
			if !tc.decided && launch[0] != launch[1] {
				t.Fatalf("the launch verdict moved with the launching process's environment:\n%s\nvs\n%s", launch[0], launch[1])
			}
		})
	}
}
