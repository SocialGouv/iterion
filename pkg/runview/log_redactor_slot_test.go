package runview

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// botDeclaringASecret is a one-node workflow whose declared secret carries
// an inline value, so BuildExecutor's guard holds a known value without a
// sealed store in reach.
const botDeclaringASecret = `vars:
  planted: string = ""

secrets:
  db: "{{vars.planted}}"

tool noop:
  command: "true"

workflow main:
  entry: noop
  noop -> done
`

func compileSecretBot(t *testing.T) *ir.Workflow {
	t.Helper()
	res := ir.Compile(parsertest.Parse("main.bot", botDeclaringASecret).File)
	if res.Workflow == nil {
		t.Fatalf("compile: %v", res.Diagnostics)
	}
	return res.Workflow
}

// BuildExecutor SETS the run's matcher on the logger it is given instead
// of forking it away: the handle the launch surface still holds — the one
// it logs `executor close`, an MCP server's stderr and a supervisor's
// steer on — crosses the same pass the executor's and the engine's lines
// do (iterion#2822). The service logger it was forked from keeps its own
// reading, which is what lets two runs share one process.
func TestBuildExecutorSetsTheRunsMatcherOnTheLoggerItIsGiven(t *testing.T) {
	const value = "hunter2-9f8e7d6c5b4a"
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var procBuf, runBuf bytes.Buffer
	process := iterlog.New(iterlog.LevelInfo, &procBuf)
	runLogger := process.WithWriter(&runBuf)

	if _, err := BuildExecutor(ExecutorSpec{
		Ctx:      context.Background(),
		Store:    st,
		RunID:    "run-slot",
		Workflow: compileSecretBot(t),
		Vars:     map[string]string{"planted": value},
		Logger:   runLogger,
		WorkDir:  t.TempDir(),
		StoreDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}

	// The shape of pkg/cli/run.go's own line: a child process's stderr
	// quoted on the surface's per-run handle.
	runLogger.Warn("MCP health-check failed for server %q: start: env TOKEN=%s rejected", "github", value)
	process.Warn("process line: %s", value)

	placeholder := secretguard.PlaceholderForName("db")
	if got := runBuf.String(); strings.Contains(got, value) || !strings.Contains(got, placeholder) {
		t.Errorf("the surface's own line did not cross the run's matcher: %q", got)
	}
	if got := procBuf.String(); !strings.Contains(got, "process line: "+value) {
		t.Errorf("the run's matcher landed on the service logger, which every run shares: %q", got)
	}
}

// Two runs, one process, one service logger: each run's lines cross ITS
// matcher and neither replaces the other's. This is the studio shape —
// prepareRunLog forks s.logger per run, BuildExecutor installs that run's
// guard on the fork — driven through the production helpers, concurrently.
func TestTwoConcurrentRunsKeepTheirOwnMatcher(t *testing.T) {
	const valueA = "hunter2-9f8e7d6c5b4a"
	const valueB = "s3kr3t-1122334455aa"
	dir := t.TempDir()
	var procBuf bytes.Buffer
	svc, err := NewService(dir, WithLogger(iterlog.New(iterlog.LevelInfo, &procBuf)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wf := compileSecretBot(t)

	type run struct {
		id, value string
		logger    *iterlog.Logger
	}
	runs := []*run{{id: "run-a", value: valueA}, {id: "run-b", value: valueB}}
	for _, r := range runs {
		_, logger := svc.prepareRunLog(r.id)
		r.logger = logger
		if _, err := BuildExecutor(ExecutorSpec{
			Ctx:      context.Background(),
			Store:    st,
			RunID:    r.id,
			Workflow: wf,
			Vars:     map[string]string{"planted": r.value},
			Logger:   logger,
			WorkDir:  t.TempDir(),
			StoreDir: t.TempDir(),
		}); err != nil {
			t.Fatalf("BuildExecutor(%s): %v", r.id, err)
		}
	}

	// Both runs log the SAME text, carrying both values: what separates
	// the two files is which matcher each run's handle carries.
	line := "snapshot: " + valueA + " / " + valueB
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		go func(r *run) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				r.logger.Warn("%s", line)
			}
		}(r)
	}
	wg.Wait()
	svc.logger.Warn("service line: %s", line)

	placeholder := secretguard.PlaceholderForName("db")
	for _, r := range runs {
		b, err := os.ReadFile(filepath.Join(dir, "runs", r.id, "run.log"))
		if err != nil {
			t.Fatalf("read %s run.log: %v", r.id, err)
		}
		got := string(b)
		if strings.Contains(got, r.value) {
			t.Errorf("%s: its own declared value reached its run.log — another run's matcher replaced its own: %q", r.id, got)
		}
		if !strings.Contains(got, placeholder) {
			t.Errorf("%s: no placeholder in its run.log: %q", r.id, got)
		}
	}
	if got := procBuf.String(); !strings.Contains(got, "service line: "+line) {
		t.Errorf("a run's matcher landed on the service logger: %q", got)
	}
}

// runLoggerFor is what the finalize helpers log on: the run's own handle
// while this process holds the run, and never the service logger — the
// one every run in this process shares.
func TestRunLoggerForNeverHandsOutTheServiceLogger(t *testing.T) {
	dir := t.TempDir()
	var procBuf bytes.Buffer
	svc, err := NewService(dir, WithLogger(iterlog.New(iterlog.LevelInfo, &procBuf)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, live := svc.prepareRunLog("run-live")
	if got := svc.runLoggerFor("run-live"); got != live {
		t.Error("a run this process holds must get the per-run logger prepareRunLog built — the handle carrying its matcher")
	}

	// A run held elsewhere (the reaper's sweep): no matcher exists here,
	// but the handle is still a fork, so nothing a live run installs can
	// reach it and nothing it does can reach a live run.
	foreign := svc.runLoggerFor("run-elsewhere")
	if foreign == svc.logger {
		t.Fatal("runLoggerFor handed out the service logger itself")
	}
	foreign.SetRedactor(func(s string) string { return strings.ReplaceAll(s, "zzz", "__X__") })
	svc.logger.Warn("service line: zzz")
	if got := procBuf.String(); !strings.Contains(got, "service line: zzz") {
		t.Errorf("the pass installed on the foreign-run handle reached the service logger: %q", got)
	}

	svc.dropRunLog("run-live")
	if got := svc.runLoggerFor("run-live"); got == live {
		t.Error("a dropped run must not keep handing out its closed per-run logger")
	}
}

// The three EXPORTED pkg/runtime helpers log a git command's combined
// output on the logger their CALLER passes, so coverage follows the
// handle: every call site in this package hands the RUN's logger, never
// the service logger (iterion#2822).
func TestTheExportedRuntimeHelpersGetTheRunsLogger(t *testing.T) {
	helpers := map[string]bool{
		"RecoverFinalize":              true,
		"CommitUncommittedAndFinalize": true,
		"MergeBundlePresets":           true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !helpers[sel.Sel.Name] {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "runtime" || len(call.Args) == 0 {
				return true
			}
			seen++
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, call.Args[len(call.Args)-1]); err != nil {
				t.Fatalf("print %s arg: %v", sel.Sel.Name, err)
			}
			if got := buf.String(); got == "s.logger" {
				t.Errorf("%s:%d: runtime.%s is handed %s — it logs git output on the handle it is given, and the service logger carries no run's matcher (use s.runLoggerFor(runID) or the run's own logger)",
					name, fset.Position(call.Pos()).Line, sel.Sel.Name, got)
			}
			return true
		})
	}
	if seen < 4 {
		t.Fatalf("the sweep found %d call sites of the exported finalize helpers, want the 4 this package has — the predicate stopped matching", seen)
	}
}
