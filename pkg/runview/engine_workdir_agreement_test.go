package runview

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"
)

// executorFactoryProbeParam names, for each factory a run surface calls on
// its way to the executor, the parameter that carries the directory the run's
// ENGINE is rooted at — the value that must reach the fallback screen's
// sandbox probe. The directory travels POSITIONALLY through these, so the
// guard below resolves the position from the factory's own declaration rather
// than hardcoding an index a new parameter would shift.
//
// Every frame of the chain is named, not just its ends: the runner hands the
// directory to buildExecutor, which forwards it to executorSpec, which sets
// the field. A frame missing here is read as naming no root at all, so
// swapping its forwarded argument re-introduces #2165 on every cloud run with
// both halves of this guard green.
var executorFactoryProbeParam = map[string]string{
	"buildExecutor":    "workDir",       // pkg/runner
	"executorSpec":     "workDir",       // pkg/runner
	"buildRunExecutor": "engineWorkDir", // pkg/cli
}

// probeRootDecidedElsewhere is every production site that names the directory
// its run's sandbox screen judges WITHOUT rooting an engine in the same
// function, and the expression each one answers with. The agreement rule below
// cannot read these: the directory the engine receives is decided one frame
// away — by the caller of an executor factory, or by the engine's own resume
// precedence off the run document.
//
// So the value is on the record instead, and a mismatch here does not mean
// "update the record": it means the directory a surface screens its run
// against has changed, and the new one has to BE the directory that surface's
// engine roots the run at. Each entry carries why it is.
var probeRootDecidedElsewhere = map[string]string{
	// `iterion resume`: the resumed engine roots the run at what the run
	// RECORDED, which is the precedence this helper mirrors from the engine's
	// own admissionRepoRoot. The engine is built by the command one frame up.
	"pkg/cli/resume_helpers.go:buildResumeExecutor": "runview.ResumeProbeWorkDir(r)",
	// `iterion run` builds its engine with NO runtime.WithWorkDir
	// (buildEngine), so the engine fills its own directory with the process
	// cwd and roots the run at THAT directory's repository: the empty answer
	// is what leaves the probe on the same cwd. Naming any directory here
	// would screen a repository the engine never resolves.
	"pkg/cli/run.go:RunRun": `""`,
	// The forwarding frames answer with the directory their caller names,
	// and every caller is bound: subbotRunnerForCLI, executeRun and the
	// runner's subbotRunnerFor by the agreement rule, RunRun by the entry
	// above. The runner's chain is read link by link — executeRun names the
	// directory, buildExecutor forwards it, executorSpec sets the field — so
	// a swapped argument reddens at whichever frame swapped it.
	"pkg/cli/run.go:buildRunExecutor":  "engineWorkDir",
	"pkg/runner/loop.go:buildExecutor": "workDir",
	"pkg/runner/loop.go:executorSpec":  "workDir",
	// The studio launch: this very local reaches the engine through
	// launchExtras.workDir, which engineOptions hands to
	// runtime.WithWorkDir. Both sides apply s.effectiveWorkDir, and it
	// returns a non-empty override verbatim and the service root for an empty
	// one (TestServiceEffectiveWorkDir_UsesOverrideThenServiceRoot), so
	// applying it to its own result moves nothing.
	"pkg/runview/service_launch.go:startInProcess": "workDir",
	// The studio resume: the recorded root, through the one helper the CLI
	// resume shares — while WorkDir stays the run's workspace for Claw's
	// file tools.
	"pkg/runview/service_launch.go:resumeExecutorSpec": "ResumeProbeWorkDir(r)",
}

// Six production surfaces decide, IN ONE FUNCTION, both the directory their
// engine is rooted at (runtime.WithWorkDir) and the directory their executor's
// sandbox screen will judge. Those two decisions are the same fact, and the
// defect this guard comes from is what happens when they are two: the runner
// handed the engine a per-run clone and the screen the pod's cwd.
//
// No verdict can tell the two roots apart — every non-empty root resolves an
// active `sandbox: auto` spec, so only an empty one could flip the boolean —
// and at the runner the choice is made one frame above the function the
// value-level tests drive. So the agreement is asserted on the parsed source:
// in any function that builds a run's executor AND roots an engine, the two
// expressions must be IDENTICAL. A surface that needs them to differ says so
// here.
//
// Every OTHER production answer — the four surfaces that build the executor
// one frame away from the engine option, and the three frames that forward
// the directory their caller names — is judged against
// probeRootDecidedElsewhere, so the two rules together leave no site naming a
// screen root nothing looks at. Without that second half the guard read as
// coverage of the class while binding six of the thirteen functions that
// answer: the CLI resume could screen the directory `iterion resume` was typed
// in, and the studio launch the service root instead of the dispatcher's
// worktree, with every test in the tree still green.
//
// Both rules read an answer wherever a site names one — a key of the spec's
// literal, an assignment on that spec after it, a factory's forwarded
// argument. Four of the nine construction sites already set some fields in a
// block below their literal, and a probe root assigned there was read by
// neither rule.
func TestEveryEngineWorkDirReachesItsOwnScreen(t *testing.T) {
	type candidate struct {
		rel      string
		fset     *token.FileSet
		file     *ast.File
		pkgNames map[string]bool
		bare     bool
	}
	var candidates []candidate
	// Keyed by package directory: pkg/runner's buildExecutor is declared in
	// loop.go and called from subbot.go, so the position cannot be resolved
	// file by file.
	params := map[string]map[string]int{}

	sweepGoFiles(t, func(rel, p string) {
		// Production only. A test fixture legitimately points the engine and
		// the screen at different trees — that is how the divergence this
		// guard forbids gets exercised at all.
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Errorf("parse %s: %v", rel, err)
			return
		}
		dir := path.Dir(rel)
		for fn, param := range executorFactoryProbeParam {
			if idx := paramIndex(file, fn, param); idx >= 0 {
				if params[dir] == nil {
					params[dir] = map[string]int{}
				}
				params[dir][fn] = idx
			}
		}
		pkgNames := runviewPackageIdents(file)
		bare := file.Name.Name == "runview" || pkgNames["."]
		// Kept: the files that root an engine, and the files that answer the
		// screen's root. Both halves of the rule need the second set — a
		// surface answering from another frame is exactly what the record
		// below judges.
		if rootsAnEngine(file) || buildsARunsExecutor(file, pkgNames, bare) {
			candidates = append(candidates, candidate{rel: rel, fset: fset, file: file, pkgNames: pkgNames, bare: bare})
		}
	})

	judged := map[string]string{} // "file:func" -> the one expression
	// The sites answering with their engine rooted in another frame, in walk
	// order (lexical by file, source order within one) so two runs of this
	// guard report the same thing in the same order.
	type answeringSite struct {
		site    string
		answers []string
	}
	var offFrame []answeringSite
	for _, c := range candidates {
		for _, decl := range c.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			screenRoots := renderedScreenRoots(c.fset, fn, c.pkgNames, c.bare, params[path.Dir(c.rel)])
			if len(screenRoots) == 0 {
				continue
			}
			site := c.rel + ":" + fn.Name.Name
			engineRoots := renderedEngineWorkDirs(c.fset, fn)
			if len(engineRoots) == 0 {
				offFrame = append(offFrame, answeringSite{site: site, answers: screenRoots})
				continue
			}
			if len(engineRoots) > 1 {
				t.Errorf("%s roots its engine at %v — more than one directory, so no screen can predict which "+
					"repository this run resolves its sandbox against. Resolve one local and pass it once.",
					site, engineRoots)
				continue
			}
			for _, got := range screenRoots {
				if got != engineRoots[0] {
					t.Errorf("%s hands its ENGINE %s and its executor's sandbox screen %s. The screen decides "+
						"whether the operator's `--fallback codex:…` crossing is taken, and it must decide that "+
						"about the run that executes: pass the one expression to both.", site, engineRoots[0], got)
				}
			}
			judged[site] = engineRoots[0]
		}
	}

	// The surfaces this rule binds today. Listed so a surface that stops
	// being judged — a refactor that moves the engine option one frame away
	// from the executor it belongs to, an aliased pkg/runtime import, a
	// factory parameter renamed — reddens here instead of silently leaving
	// the guard covering less.
	want := []string{
		"pkg/cli/run.go:subbotRunnerForCLI",
		"pkg/dispatcher/engine_runner.go:Dispatch",
		"pkg/dispatcher/subbot.go:subbotRunnerForDispatch",
		"pkg/runner/loop.go:executeRun",
		"pkg/runner/subbot.go:subbotRunnerFor",
		"pkg/runview/subbot.go:subbotRunnerFor",
	}
	got := make([]string, 0, len(judged))
	for site := range judged {
		got = append(got, site)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("the surfaces this guard judges are %v, want %v — a surface that left the list no longer has "+
			"its engine directory bound to its own screen; one that joined it belongs in the list.", got, want)
	}

	// And the other half: a site that answers while its engine is rooted one
	// frame away is judged against the record, both ways — an answer that
	// changed, and a site that stopped answering, both redden.
	answered := map[string]bool{}
	for _, a := range offFrame {
		answered[a.site] = true
		expected, ok := probeRootDecidedElsewhere[a.site]
		if !ok {
			t.Errorf("%s names the directory its run's sandbox screen judges (%v) and roots no engine itself, so "+
				"nothing in this tree says whether that is the directory the engine roots this run at. Root the "+
				"engine in the same function, or put the expression on probeRootDecidedElsewhere with the reason "+
				"it IS that directory.", a.site, a.answers)
			continue
		}
		if len(a.answers) != 1 || a.answers[0] != expected {
			t.Errorf("%s screens its run against %v; the record says %s. The screen decides whether the "+
				"operator's `--fallback codex:…` crossing is taken, and it must decide that about the run that "+
				"executes: check the new expression is the directory THIS surface's engine is rooted at, then "+
				"update the record.", a.site, a.answers, expected)
		}
	}
	onRecord := map[string]bool{}
	for site := range probeRootDecidedElsewhere {
		onRecord[site] = true
	}
	for _, site := range sortedKeys(onRecord) {
		if !answered[site] {
			t.Errorf("%s is on the record as answering the sandbox screen's root from another frame, but this "+
				"walk no longer sees it answer: it was renamed, it stopped naming the directory at all, or it "+
				"roots its own engine now — in which case it belongs to the agreement rule, not to the record.",
				site)
		}
	}
}

// renderedScreenRoots is half of the rule above: a shape it does not read is a
// screen root neither the agreement nor the record judges, and a shape it
// reads too widely is a false accusation in a green-by-default gate. Both
// directions are exercised on source text rather than on the tree, because the
// shapes that matter are the ones the tree does not hold yet — except one,
// a `.WorkDir` assignment on something that is not a spec, which the tree
// holds today (pkg/runner's New) and which must stay unread.
func TestRenderedScreenRootsReadsEveryShapeThatNamesTheRoot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		src       string
		factories map[string]int
		want      []string
	}{
		{
			name: "a literal keying the probe root",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f(d string) { _, _ = runview.BuildExecutor(runview.ExecutorSpec{SandboxProbeWorkDir: d}) }`,
			want: []string{"d"},
		},
		{
			name: "a literal keying only the tool workspace",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f(d string) { _, _ = runview.BuildExecutor(runview.ExecutorSpec{WorkDir: d}) }`,
			want: []string{"d"},
		},
		{
			// The documented combination: the probe root is the engine's
			// repository, WorkDir the executor's tool workspace.
			name: "a literal keying both",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f(d, p string) { _, _ = runview.BuildExecutor(runview.ExecutorSpec{WorkDir: d, SandboxProbeWorkDir: p}) }`,
			want: []string{"p"},
		},
		{
			// The shape neither rule read: `iterion run` keys its probe root
			// in the literal and sets five more fields in a block below it,
			// so an override landing in that block shipped the defect green.
			name: "a literal assigned to a local, then overridden",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f(d, o string) {
	spec := runview.ExecutorSpec{SandboxProbeWorkDir: d}
	spec.SandboxProbeWorkDir = o
	_, _ = runview.BuildExecutor(spec)
}`,
			want: []string{"d", "o"},
		},
		{
			// …and the shape that must NOT be accused for it: one spec whose
			// tool workspace is keyed and whose probe root is assigned names
			// ONE screen root, the probe one.
			name: "a literal keying the tool workspace, then given a probe root",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f(d, p string) {
	spec := runview.ExecutorSpec{WorkDir: d}
	spec.SandboxProbeWorkDir = p
	_, _ = runview.BuildExecutor(spec)
}`,
			want: []string{"p"},
		},
		{
			// A spec a factory returned: the studio's resume adjusts three
			// fields on one, so the probe field is read wherever it is
			// assigned, with no need to know the local's type.
			name: "an override on a spec a factory returned",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func mk() runview.ExecutorSpec { return runview.ExecutorSpec{} }
func f(x string) {
	spec := mk()
	spec.SandboxProbeWorkDir = x
	_, _ = runview.BuildExecutor(spec)
}`,
			want: []string{"x"},
		},
		{
			name: "a spec declared and filled field by field",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func f(d string) {
	var spec runview.ExecutorSpec
	spec.WorkDir = d
	_, _ = runview.BuildExecutor(spec)
}`,
			want: []string{"d"},
		},
		{
			// The refusal this rule owes the tree: WorkDir is an ordinary
			// field name, and a function that assigns it on something else
			// names no run's screen root. Read unconditionally, this accused
			// pkg/runner/loop.go:New — which builds no executor at all — of
			// screening a run against os.TempDir().
			name: "a WorkDir assignment on something that is not a spec",
			src: `package p
type cfg struct{ WorkDir string }
func f(c *cfg, d string) { c.WorkDir = d }`,
		},
		{
			// A tuple assignment from one call cannot attribute an expression
			// to a field, so it names nothing rather than guessing.
			name: "two fields assigned from one call",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/runview"
func two() (string, string) { return "", "" }
func f() {
	var spec runview.ExecutorSpec
	spec.WorkDir, spec.SandboxProbeWorkDir = two()
	_, _ = runview.BuildExecutor(spec)
}`,
		},
		{
			name: "a factory call, at the position its declaration gives",
			src: `package p
func f(w string) { _, _ = buildThing(nil, w) }`,
			factories: map[string]int{"buildThing": 1},
			want:      []string{"w"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			pkgNames := runviewPackageIdents(file)
			bare := file.Name.Name == "runview" || pkgNames["."]
			var fn *ast.FuncDecl
			for _, decl := range file.Decls {
				if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "f" {
					fn = d
				}
			}
			if fn == nil {
				t.Fatalf("the fixture declares no func f")
			}
			got := renderedScreenRoots(fset, fn, pkgNames, bare, tc.factories)
			if !slices.Equal(got, tc.want) {
				t.Errorf("renderedScreenRoots = %v, want %v — a root this walk does not read is one no rule "+
					"above judges, and one it reads too widely is a false accusation in a gate nobody "+
					"re-reads", got, tc.want)
			}
		})
	}
}

// buildsARunsExecutor reports whether the file constructs an ExecutorSpec or
// calls one of the executor factories — the two ways a function names the
// directory its run's sandbox screen will judge.
func buildsARunsExecutor(file *ast.File, pkgNames map[string]bool, bare bool) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if isExecutorSpecType(node.Type, pkgNames, bare) {
				found = true
			}
		case *ast.CallExpr:
			if _, ok := executorFactoryProbeParam[calleeName(node.Fun)]; ok {
				found = true
			}
		}
		return !found
	})
	return found
}

// rootsAnEngine reports whether the file calls runtime.WithWorkDir anywhere.
func rootsAnEngine(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if isRuntimeWithWorkDir(n) {
			found = true
		}
		return !found
	})
	return found
}

func isRuntimeWithWorkDir(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "WithWorkDir" {
		return false
	}
	// pkg/runtime is imported unaliased by every surface that roots an
	// engine; an alias leaves the function unjudged, which the site list
	// above is what catches. The qualifier is read because runview and
	// pkg/backend/model export a WithWorkDir of their own.
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "runtime"
}

// renderedEngineWorkDirs returns every distinct expression fn hands to
// runtime.WithWorkDir.
func renderedEngineWorkDirs(fset *token.FileSet, fn *ast.FuncDecl) []string {
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if !isRuntimeWithWorkDir(n) {
			return true
		}
		arg := n.(*ast.CallExpr).Args[0]
		if rendered := renderNode(fset, arg); !slices.Contains(out, rendered) {
			out = append(out, rendered)
		}
		return true
	})
	return out
}

// The two ExecutorSpec fields that can name the directory a run's sandbox
// screen judges. sandboxProbeRoot() reads them in this order, so a spec
// setting both is screened against the probe field and WorkDir is then the
// executor's tool workspace, deliberately another tree.
const (
	probeRootField     = "SandboxProbeWorkDir"
	toolWorkspaceField = "WorkDir"
)

// renderedScreenRoots returns every expression fn hands the executor as the
// directory its sandbox screen judges: the ExecutorSpec key that carries it,
// the same two fields ASSIGNED on a spec instead of keyed in its literal, and
// the probe parameter of each executor factory fn calls.
//
// Answers are grouped BY SPEC, because the field precedence is per spec: a
// literal assigned to a local and an override on that local three lines down
// are one spec with two answers (which reddens), while a literal naming
// WorkDir and an override naming the probe root are one spec with one answer
// (which does not). Each literal handed straight to a builder, and each
// factory call, is its own spec.
func renderedScreenRoots(fset *token.FileSet, fn *ast.FuncDecl, pkgNames map[string]bool, bare bool, factories map[string]int) []string {
	// pos distinguishes the anonymous specs — a literal with no name to
	// group by, a factory call — from one another; named specs carry their
	// local's spelling and pos 0.
	type specID struct {
		name string
		pos  token.Pos
	}
	type answers struct{ probe, workDir []ast.Expr }
	bySpec := map[specID]*answers{}
	var order []specID
	at := func(id specID) *answers {
		a, ok := bySpec[id]
		if !ok {
			a = &answers{}
			bySpec[id] = a
			order = append(order, id)
		}
		return a
	}
	keyed := func(id specID, lit *ast.CompositeLit) {
		a := at(id)
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case probeRootField:
				a.probe = append(a.probe, kv.Value)
			case toolWorkspaceField:
				a.workDir = append(a.workDir, kv.Value)
			}
		}
	}
	// `x.WorkDir = …` names a screen root only when x IS a spec: pkg/runner's
	// New assigns cfg.WorkDir on the runner's own config, and reading that as
	// an answer accused a function that builds no executor at all.
	// SandboxProbeWorkDir needs no such test — ExecutorSpec is the only type
	// in this tree that has one, and a second one appearing is a question
	// this guard SHOULD ask.
	specs := executorSpecLocals(fn.Body, pkgNames, bare)
	owned := map[ast.Node]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			// Positional only: a tuple assignment from one call cannot
			// attribute an expression to a field, so it is left to the arms
			// below.
			if len(node.Lhs) != len(node.Rhs) {
				return true
			}
			for i, lhs := range node.Lhs {
				switch l := lhs.(type) {
				case *ast.Ident:
					lit, ok := node.Rhs[i].(*ast.CompositeLit)
					if !ok || !isExecutorSpecType(lit.Type, pkgNames, bare) {
						continue
					}
					// The literal's keys answer for THIS local, so an
					// override on it competes with them instead of counting
					// as a second spec.
					owned[lit] = true
					specs[l.Name] = true
					keyed(specID{name: l.Name}, lit)
				case *ast.SelectorExpr:
					name := renderNode(fset, l.X)
					switch l.Sel.Name {
					case probeRootField:
						a := at(specID{name: name})
						a.probe = append(a.probe, node.Rhs[i])
					case toolWorkspaceField:
						if specs[name] {
							a := at(specID{name: name})
							a.workDir = append(a.workDir, node.Rhs[i])
						}
					}
				}
			}
		case *ast.CompositeLit:
			if owned[node] || !isExecutorSpecType(node.Type, pkgNames, bare) {
				return true
			}
			keyed(specID{pos: node.Pos()}, node)
		case *ast.CallExpr:
			idx, known := factories[calleeName(node.Fun)]
			if !known || idx >= len(node.Args) {
				return true
			}
			a := at(specID{pos: node.Pos()})
			a.probe = append(a.probe, node.Args[idx])
		}
		return true
	})
	var out []string
	for _, id := range order {
		a := bySpec[id]
		chosen := a.probe
		if len(chosen) == 0 {
			chosen = a.workDir
		}
		for _, e := range chosen {
			if rendered := renderNode(fset, e); !slices.Contains(out, rendered) {
				out = append(out, rendered)
			}
		}
	}
	return out
}

// paramIndex is the position of the parameter named param in the declaration
// of fn within file, flattening grouped names (`a, b string`). -1 when the
// function is not declared here or has no such parameter.
func paramIndex(file *ast.File, fn, param string) int {
	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if !ok || d.Name.Name != fn || d.Type.Params == nil {
			continue
		}
		i := 0
		for _, field := range d.Type.Params.List {
			if len(field.Names) == 0 {
				i++
				continue
			}
			for _, name := range field.Names {
				if name.Name == param {
					return i
				}
				i++
			}
		}
	}
	return -1
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func renderNode(fset *token.FileSet, n ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, n); err != nil {
		return "<unprintable>"
	}
	return buf.String()
}
