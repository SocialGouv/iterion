package runview

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A question this sweep asks of every ExecutorSpec construction site: the
// fields that answer it — any ONE of them does, because "which repository does
// this run resolve its sandbox against" is answered by WorkDir or by
// SandboxProbeWorkDir — and the prose a failure names it by.
//
// The field set is also the key of expectedGiveUps, and that is the point: the
// key used to be a prose label typed at each call site, so a ledger entry
// under a key no question passes was read in NEITHER direction — neither "this
// give-up is on the record" nor "this record entry is still a give-up" runs
// for a key nobody asks. Derived from the fields, two questions can only share
// a ledger by being the same question.
type sweepQuestion struct {
	fields []string
	about  string
}

// key is how expectedGiveUps names this question. Derived, never typed.
func (q sweepQuestion) key() string { return strings.Join(q.fields, "+") }

var (
	botIdentityQuestion    = sweepQuestion{fields: []string{"BotID"}, about: "the bot identity"}
	sandboxTiersQuestion   = sweepQuestion{fields: []string{"SandboxTiersKnown"}, about: "the sandbox tiers"}
	runsRepositoryQuestion = sweepQuestion{
		fields: []string{"WorkDir", "SandboxProbeWorkDir"},
		about:  "the run's repository",
	}

	// sweepQuestions is every question the gates below ask, and therefore
	// every key expectedGiveUps may carry.
	sweepQuestions = []sweepQuestion{botIdentityQuestion, sandboxTiersQuestion, runsRepositoryQuestion}
)

// executorSpecSitesMissing returns every non-test file in the repository that
// builds an ExecutorSpec answering none of q's fields, skipping the allowlist.
//
// One traversal per QUESTION, judged at the SITE, so a file whose two
// construction sites answer differently is not accused of a rule both of them
// follow. Sites are reported by repo-relative path, so a failure names the file
// to fix. The give-up ledger is checked here rather than by the caller: the key
// is then the question that was actually asked, and no gate can forget to ask
// for its give-ups to be on the record.
func executorSpecSitesMissing(t *testing.T, q sweepQuestion, exempt map[string]string) []string {
	t.Helper()

	var offenders, gaveUp []string
	sweepGoFiles(t, func(rel, path string) {
		// This package's OWN tests build partial specs on purpose — varying
		// one field is what they are for. Every other file, test or not, is
		// a CONSUMER and owes the answer. Same rule, same shape, as the
		// sibling sweep in pkg/backend/mcp, which skips `pkg/backend/mcp/`
		// for the identical reason.
		if strings.HasPrefix(rel, "pkg/runview/") && strings.HasSuffix(rel, "_test.go") {
			return
		}
		if _, ok := exempt[rel]; ok {
			return
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return
		}
		missing, unread := executorSpecMissesFields(file, q.fields)
		if missing {
			offenders = append(offenders, rel)
		}
		if unread {
			gaveUp = append(gaveUp, rel)
		}
	})
	assertGiveUpsAreOnTheRecord(t, q, gaveUp)
	return offenders
}

// sweepGoFiles calls visit for every Go file of this repository, by the one
// set of rules every sweep over this tree reads: no vendored or generated
// tree, no nested checkout, and `_test.go` included — the sibling sweep in
// pkg/backend/mcp parses tests too, precisely because two of the sites it
// guards are `live` e2e tests no CI job runs, and skipping them here hid two
// e2e sites that build an executor for real. Two walks with their own copies
// of these rules would drift, and no reader could tell which one was right.
func sweepGoFiles(t *testing.T, visit func(rel, path string)) {
	t.Helper()
	repoRoot := filepath.Join("..", "..")
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			// testdata is skipped because the toolchain skips it: a .go file
			// there is never compiled.
			case "vendor", "node_modules", ".git", ".iterion", "studio", "testdata":
				return filepath.SkipDir
			}
			// A directory carrying its own .git is a NESTED CHECKOUT — a git
			// worktree or a sibling clone an operator keeps on disk. Its files
			// belong to another tree (none are tracked here), and its older
			// copies would report as offenders of a rule they predate. Detect
			// them by that marker rather than by directory name: where someone
			// parks their checkouts is their business, not this test's.
			if path != repoRoot {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		visit(filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator))), path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// Whether a run is sandboxed decides where its MCP servers may be started: in
// the container the workflow declared, or beside the launcher — an operator's
// machine, or the cloud runner pod that holds the platform's credentials. The
// executor predicts that from two tiers the launch surface passes, and the
// engine resolves it from the same two.
//
// An ExecutorSpec that passes neither is indistinguishable, field by field,
// from one whose surface looked and found no sandbox — and that ambiguity is
// not theoretical: when this guard was written FIVE of the nine construction
// sites passed no tier at all (both resumes, both subbot runners, the
// dispatcher), so a resumed run predicted "no sandbox" while its engine
// started one. SandboxTiersKnown is the explicit answer, and this guard is why
// the tenth site cannot quietly omit it.
//
// An entry in the allowlist is a decision on the record, not an exemption.
func TestEveryExecutorConstructionAnswersTheSandboxQuestion(t *testing.T) {
	exempt := map[string]string{}

	offenders := executorSpecSitesMissing(t, sandboxTiersQuestion, exempt)
	if len(offenders) > 0 {
		t.Errorf("these build an executor without saying whether they know the run's sandbox tiers, so the MCP "+
			"start policy cannot tell \"this surface looked and there is no sandbox\" from \"this surface did not "+
			"look\": %v\nSet SandboxTiersKnown (true with the tiers the engine will receive; false to stay "+
			"fail-closed until the engine settles the sandbox), or add the file to this test's exempt map with a "+
			"reason.", offenders)
	}
}

// The same sandbox question has a second input the surface alone holds: WHICH
// REPOSITORY the run resolves it against. The screen asks
// runtime.RunWillBeSandboxed for the root runtime.EngineRepoRoot derives from
// the directory the ENGINE is given, so a spec that names no directory at all
// leaves the probe on the process cwd — the runner pod's, the daemon's, a test
// binary's — and the screen then judges a repository the run never touches.
//
// WorkDir answers it wherever the engine receives that same directory (the
// launch surfaces, the runner, non-isolated subbots and the dispatcher). A
// surface whose engine root differs from the tool workspace, notably resume,
// answers with SandboxProbeWorkDir, which nothing else reads.
//
// Like its two siblings, this guard asserts the site ANSWERED; whether the
// directory it named is the one its engine receives is the site's own claim.
// An entry in the allowlist is a decision on the record, not an exemption.
func TestEveryExecutorConstructionNamesTheRunsRepository(t *testing.T) {
	exempt := map[string]string{
		// A golden replay hands ONE frozen node to the executor directly and
		// builds no engine, so there is no engine root for a probe root to
		// agree with. It chdirs to the repo root for the tool workspace,
		// which is also what the probe then reads.
		"pkg/botreplay/record.go": "replay harness: executes one node, builds no engine",
	}

	// ONE traversal over both answers: the question is "which repository", and
	// a site answers it with either field. Two traversals intersected by FILE
	// accused a file whose engine site named WorkDir while its probe site named
	// SandboxProbeWorkDir, and discarded the second walk's give-ups — so a
	// shape the sweep cannot judge was on the record for one field and silent
	// for the other.
	offenders := executorSpecSitesMissing(t, runsRepositoryQuestion, exempt)
	if len(offenders) > 0 {
		t.Errorf("these build an executor without naming the directory the engine will root this run at, so the "+
			"fallback screen's sandbox probe resolves the PROCESS cwd's repository instead of the run's: %v\n"+
			"Set WorkDir when the engine receives it, else SandboxProbeWorkDir (the value passed to "+
			"runtime.WithWorkDir, \"\" when the engine is given none), or add the file to this test's exempt map "+
			"with a reason.", offenders)
	}
}

// The prediction is the launch surface's best answer before the engine has
// started anything, and it is allowed to be permissive only when the surface
// actually knows the tiers. The asymmetry is the whole point: a wrong
// "sandboxed" costs a warning, a wrong "not sandboxed" starts a
// workflow-controlled process beside the launcher.
func TestThePredictedStartPolicyIsPermissiveOnlyWhenTheSurfaceKnows(t *testing.T) {
	sandboxed := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "auto"}}
	plain := &ir.Workflow{}

	for _, tc := range []struct {
		name string
		spec ExecutorSpec
		want mcp.StartPolicy
	}{
		{"a surface that did not look stays closed", ExecutorSpec{Workflow: plain}, mcp.StartPolicyUnknown},
		{"a surface that did not look is closed even for a sandboxed workflow",
			ExecutorSpec{Workflow: sandboxed}, mcp.StartPolicyUnknown},
		{"knows, and the workflow sandboxes", ExecutorSpec{Workflow: sandboxed, SandboxTiersKnown: true},
			mcp.StartOperatorServersOnly},
		{"knows, and nothing sandboxes", ExecutorSpec{Workflow: plain, SandboxTiersKnown: true},
			mcp.StartAllServers},
		{"knows, and the operator's override neutralises the workflow's block",
			ExecutorSpec{Workflow: sandboxed, SandboxOverride: "none", SandboxTiersKnown: true},
			mcp.StartAllServers},
		{"knows, and the global default sandboxes a workflow that declares nothing",
			ExecutorSpec{Workflow: plain, SandboxDefault: "auto", SandboxTiersKnown: true},
			mcp.StartOperatorServersOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := predictedStartPolicy(tc.spec); got != tc.want {
				t.Errorf("predictedStartPolicy = %v, want %v", got, tc.want)
			}
		})
	}
}

// expectedGiveUps are, PER QUESTION, the files whose ExecutorSpec the sweep's
// predicate cannot judge for that question's fields — a spec declared and
// never handed where the walk can see it, or one whose address escapes to a
// callee that may well be where the field is set. Judging those would fail
// correct code, so the predicate gives up; the list exists so the give-up is
// a decision on the record rather than silence. Empty today.
//
// Keyed by a question's FIELD SET (sweepQuestion.key) because the verdict IS
// per question: a factory that fills a spec field by field and returns it
// answers the question whose field it sets, and is unjudgeable for the others.
// One shared list then has two questions demanding an entry the third demands
// be dropped — no state of the file makes the gate green, for the most
// ordinary refactor of a construction site there is.
var expectedGiveUps = map[string]map[string]string{}

// assertGiveUpsAreOnTheRecord checks q's give-ups both ways: every file the
// predicate could not judge is listed, and every listed file is still one the
// predicate cannot judge.
func assertGiveUpsAreOnTheRecord(t *testing.T, q sweepQuestion, gaveUp []string) {
	t.Helper()
	sort.Strings(gaveUp)
	listed := expectedGiveUps[q.key()]
	for _, rel := range gaveUp {
		if _, ok := listed[rel]; !ok {
			t.Errorf("%s builds an ExecutorSpec in a shape this sweep cannot judge, so %s is not checked "+
				"there. Build the spec as a composite literal, or add it to expectedGiveUps[%q] with the "+
				"reason — and with whatever proves the field is set.", rel, q.about, q.key())
		}
	}
	for _, rel := range sortedMapKeys(listed) {
		if !slices.Contains(gaveUp, rel) {
			t.Errorf("%s is listed as unjudgeable for %s but the sweep can read it now — drop the entry",
				rel, q.about)
		}
	}
}

// Both arms of assertGiveUpsAreOnTheRecord read the ledger under the question
// that was asked, so an entry under any other key is read by neither: the
// files it names are never required of a site, and are never re-tested for
// having become judgeable. A prose key let a typo ("the sandbox tier") carry a
// path that does not exist, in a package whose whole gate was green.
func TestEveryGiveUpOnTheRecordAnswersAQuestionTheSweepAsks(t *testing.T) {
	asked := map[string]bool{}
	for _, q := range sweepQuestions {
		asked[q.key()] = true
	}
	for _, key := range sortedMapKeys(expectedGiveUps) {
		if !asked[key] {
			t.Errorf("expectedGiveUps[%q] lists %v, and no question this sweep asks is keyed %q, so neither "+
				"direction of the give-up check reads it. Drop the entry, or — if a gate asks that question "+
				"— register it in sweepQuestions, whose keys are %v.",
				key, sortedMapKeys(expectedGiveUps[key]), key, sortedMapKeys(asked))
		}
	}
}

// sortedMapKeys keeps the two reports above in one order whatever the map
// iteration gives. The package's own sortedKeys takes map[string]bool only.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
