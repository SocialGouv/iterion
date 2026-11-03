package liveledger

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestTheLiveHarnessWiresTheOverrideSeams guards the three wirings that
// make an overridden live run honest, and that no free test can reach:
// they live in `live`-tagged harness files, so only `test:live:compile`
// builds them and only a paid run exercises them. Deleting a call site
// would compile, pass every free gate, and silently restore the defect it
// replaced — a run that keeps the fixture's pinned model, a cap that never
// reaches the budget, a row that cannot say what served.
//
// A source scan is the repo's own motif for this class
// (TestEveryLiveTestRecordsToTheLedger reads the same files to prove every
// live test records a row). It is deliberately narrow: ONE symbol per
// wiring, named where it is called.
func TestTheLiveHarnessWiresTheOverrideSeams(t *testing.T) {
	root := repoRoot(t)
	sources, err := readLiveTestSources(filepath.Join(root, "e2e"))
	if err != nil {
		t.Fatalf("read live test sources: %v", err)
	}
	// Anti-vacuity: an empty or truncated read would find no missing
	// wiring and report green on a scanner that scanned nothing.
	if len(sources) < 40 {
		t.Fatalf("only %d _test.go file(s) read out of e2e/ — the source reader is broken, and an empty set would make every requirement below pass vacuously", len(sources))
	}
	for _, f := range []string{"live_support_test.go", liveKnobsFile} {
		if _, ok := sources[f]; !ok {
			t.Fatalf("e2e/%s is not in the scanned set — a harness file the requirements below are about", f)
		}
	}

	required := []struct {
		file, call, why string
	}{
		{
			liveKnobsFile, "model.WithModelOverrides(mo)",
			"armLiveRun must pass the \"*\" override set to every executor it arms, or ITERION_LIVE_MODEL is read and then dropped: the run keeps the fixture's pinned model while the operator believes it was re-targeted",
		},
		{
			liveKnobsFile, "ir.ApplyBudgetOverrides(wf, bo)",
			"armLiveRun must apply the ceiling to the workflow the engine runs, or an authorised per-target cap never reaches the run and the bot's own (larger) budget: governs the spend",
		},
		{
			"live_support_test.go", "feedLedgerProvenance(tracker, override, events)",
			"runBotLive must record what actually served, or a pass obtained on an overridden route is indistinguishable in the committed ledger from a pass on the fixture's pinned model",
		},
		{
			liveKnobsFile, "liveledger.RecordSkip(t, reason)",
			"skipLive must record the skip, or a target this machine cannot run keeps reading `never` and `never` keeps meaning both \"nobody has paid\" and \"nobody could run it here\"",
		},
		{
			liveKnobsFile, "liveledger.ServedModelFor(served, llmNodesStarted(events), override.Model)",
			"feedLedgerProvenance must say which case the column is in when the events named no model: only the claw path emits llm_request, so a claude_code / codex / pi pass would otherwise record served_model \"\" — the column's word for a run with no LLM node — and a reader could not tell an overridden pass from a pass on the fixture's pinned model",
		},
	}
	var missing []string
	for _, r := range required {
		src, ok := sources[r.file]
		if !ok {
			t.Fatalf("e2e/%s is not in the scanned set", r.file)
		}
		if !strings.Contains(src, r.call) {
			missing = append(missing, "e2e/"+r.file+": "+r.call+" — "+r.why)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("the live harness lost %d override wiring(s):\n  - %s", len(missing), strings.Join(missing, "\n  - "))
	}

	// The guards must consult ONE helper, not a copy each: three sites
	// deciding "does the override cover this credential?" is how the
	// third one drifts from the first two.
	knobs := sources[liveKnobsFile]
	for _, helper := range []string{"func overrideCoversProvider(", "func overrideCoversCredential(", "func skipLive(", "func liveOverride("} {
		if !strings.Contains(knobs, helper) {
			t.Errorf("e2e/%s no longer declares %q — the guard chokepoint moved or was inlined per guard", liveKnobsFile, helper)
		}
	}
	// Who CONSULTS the helpers, counted outside their own declarations. A
	// substring count over the whole file is >= 1 whatever the guards do —
	// overrideCoversCredential's body spells `overrideCoversProvider(t,`
	// itself — so the check it replaces could not redden on a guard that
	// stopped asking.
	consumers, err := overrideConsumers(sources)
	if err != nil {
		t.Fatalf("scan the override consumers: %v", err)
	}
	var guards []string
	for _, fn := range consumers {
		if strings.HasPrefix(fn, "require") {
			guards = append(guards, fn)
		}
	}
	if len(guards) < 3 {
		t.Errorf("only %d requireX guard(s) consult the shared override helper (%v; all consumers: %v) — the three credential guards (requireEnv, requireOpenAI, requireOpus48) must each ask it, or an overridden run skips on a vendor key it no longer reaches",
			len(guards), guards, consumers)
	}
}

// overrideConsumers names every function in the live harness whose body
// calls overrideCoversProvider or overrideCoversCredential, EXCLUDING the
// two helpers' own declarations — a consultation the helper makes of itself
// proves nothing about the guards.
func overrideConsumers(sources map[string]string) ([]string, error) {
	helpers := map[string]bool{"overrideCoversProvider": true, "overrideCoversCredential": true}
	seen := map[string]bool{}
	for name, src := range sources {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse e2e/%s: %w", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || helpers[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && helpers[id.Name] {
					seen[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	out := make([]string, 0, len(seen))
	for fn := range seen {
		out = append(out, fn)
	}
	sort.Strings(out)
	return out, nil
}

// The consumer scan's own table: the helper's self-call must NOT count, a
// guard's call must, and a call from a non-guard function is reported but
// is not a guard.
func TestOverrideConsumers(t *testing.T) {
	src := `package e2e

func overrideCoversCredential(t *testing.T, name string) bool {
	return overrideCoversProvider(t, liveenv.ProviderOfCredentialEnv(name))
}

func requireEnv(t *testing.T, name string) {
	if overrideCoversCredential(t, name) {
		return
	}
}

func TestLive_Probe(t *testing.T) {
	_ = overrideCoversProvider(t, "anthropic")
}

func requireCLI(t *testing.T, name string) {}
`
	got, err := overrideConsumers(map[string]string{"probe_live_test.go": src})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := []string{"TestLive_Probe", "requireEnv"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("overrideConsumers = %v, want %v — the helper's own body must not count as a consultation", got, want)
	}
}

// TestTheDotEnvLoaderAsksTheOperatorInventory pins the harness's own
// dotenv loader to the rule the engine's loaders follow: a project `.env`
// plants no operator-grade ITERION_ name. The four launch knobs are in that
// inventory (internal/envtrust) because they wave the credential guards,
// move a run's spend ceiling and write the funder into a committed file —
// and the harness is the only process that reads them, so a loader that
// planted every line unfiltered was the one path the classification did
// not cover.
func TestTheDotEnvLoaderAsksTheOperatorInventory(t *testing.T) {
	live := liveTaggedSources(t)
	if _, ok := live["live_test.go"]; !ok {
		t.Fatal("e2e/live_test.go is not in the live-tagged set — the file that declares loadDotEnv")
	}
	callers, err := selectorCallers(live, "envtrust", "OperatorGradeRefused")
	if err != nil {
		t.Fatalf("scan the envtrust consumers: %v", err)
	}
	for _, fn := range callers {
		if fn == "loadDotEnv" {
			return
		}
	}
	t.Fatalf("loadDotEnv does not ask envtrust.OperatorGradeRefused before planting a .env line (callers: %v) — a repo-root .env could set ITERION_LIVE_MODEL and wave every credential guard, move the spend ceiling and name the funder of a committed receipt", callers)
}

// liveTaggedSources reads the e2e test sources and keeps the live-tagged
// ones, refusing an empty or mis-filtered set: a scanner over nothing is
// green on nothing.
func liveTaggedSources(t *testing.T) map[string]string {
	t.Helper()
	sources, err := readLiveTestSources(filepath.Join(repoRoot(t), "e2e"))
	if err != nil {
		t.Fatalf("read live test sources: %v", err)
	}
	live := map[string]string{}
	for name, src := range sources {
		if strings.Contains(src, "//go:build live") {
			live[name] = src
		}
	}
	if len(live) < 30 {
		t.Fatalf("only %d live-tagged _test.go file(s) found in e2e/ — the build-tag filter is broken, and an empty set would make every requirement pass vacuously", len(live))
	}
	return live
}

// selectorCallers names every function whose body calls `<pkg>.<sel>(`,
// sorted.
func selectorCallers(sources map[string]string, pkg, sel string) ([]string, error) {
	seen := map[string]bool{}
	for name, src := range sources {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse e2e/%s: %w", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && isSelectorCall(call, pkg, sel) {
					seen[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	out := make([]string, 0, len(seen))
	for fn := range seen {
		out = append(out, fn)
	}
	sort.Strings(out)
	return out, nil
}

// isSelectorCall reports whether call is `<pkg>.<sel>(…)`.
func isSelectorCall(call *ast.CallExpr, pkg, sel string) bool {
	s, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != sel {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// TestTheHarnessFeedsProvenanceBeforeItTakesAVerdict pins the ORDER inside
// runBotLive: the tracker is told what served before the acceptability
// check can t.Fatalf. Measured: a `fail` row with no served_model for a run
// that made three LLM calls — the events were there, the harness read them
// only on the path that survived its own verdict.
func TestTheHarnessFeedsProvenanceBeforeItTakesAVerdict(t *testing.T) {
	live := liveTaggedSources(t)
	src, ok := live["live_support_test.go"]
	if !ok {
		t.Fatal("e2e/live_support_test.go is not in the live-tagged set")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "live_support_test.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	feedLine, verdictLine := 0, 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runBotLive" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			line := fset.Position(call.Pos()).Line
			switch id.Name {
			case "feedLedgerProvenance":
				if feedLine == 0 {
					feedLine = line
				}
			case "liveRunResultAcceptable":
				if verdictLine == 0 {
					verdictLine = line
				}
			}
			return true
		})
	}
	if feedLine == 0 || verdictLine == 0 {
		t.Fatalf("runBotLive calls feedLedgerProvenance at line %d and liveRunResultAcceptable at line %d — both must be there for the order to mean anything", feedLine, verdictLine)
	}
	if feedLine > verdictLine {
		t.Fatalf("runBotLive takes its verdict (liveRunResultAcceptable, line %d) before it feeds what served (feedLedgerProvenance, line %d): a run the check refuses leaves a `fail` row that cannot say what answered", verdictLine, feedLine)
	}
}
