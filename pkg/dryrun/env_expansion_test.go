package dryrun

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A bot whose tool bodies read the environment in both arms the engine
// answers: a `${NAME:-default}` it resolves unconditionally, and a plain
// `${NAME}` only its own process can answer. The three bodies the engine
// hands a reader are all here — a `command:`, a `script:` and a
// `postcondition:`.
const envReadBot = `tool dial:
  command: "echo ${DRYRUN_PROBE_DIAL:-fallback}"
  postcondition: "test -n ${DRYRUN_PROBE_DIAL:-fallback}"

tool plain:
  command: "echo ${DRYRUN_PROBE_PLAIN}"

tool scripted:
  language: sh
  script: "printf %s ${DRYRUN_PROBE_DIAL:-fallback}"

workflow envread:
  worktree: none
  sandbox: none
  entry: dial
  dial -> plain
  plain -> scripted
  scripted -> done
`

// bodyRecorder keeps every text the dry run hands to a syntax check — the
// preview's own body, which is what `bash -n` is held to and what the
// operator reads. A pass's bodies are checked from the goroutine that runs
// the node (a fan-out branch checks its own), so the observer takes its
// mutex.
type bodyRecorder struct {
	mu   sync.Mutex
	seen []string
}

// Check implements ShellChecker: it records and answers "parses", so a
// witness of the TEXT never depends on dash being on this PATH.
func (r *bodyRecorder) Check(_, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, text)
	return nil
}

func (r *bodyRecorder) bodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// previewBodies runs the two passes of envReadBot and returns every body the
// preview prepared, joined for a message.
func previewBodies(t *testing.T) ([]string, string) {
	t.Helper()
	rec := &bodyRecorder{}
	if _, err := Run(context.Background(), compileBot(t, envReadBot), Options{Shell: rec}); err != nil {
		t.Fatal(err)
	}
	bodies := rec.bodies()
	if len(bodies) == 0 {
		t.Fatal("the preview prepared no body at all: the witness would pass on nothing")
	}
	return bodies, strings.Join(bodies, "\n")
}

func anyBody(bodies []string, want string) bool {
	for _, b := range bodies {
		if b == want {
			return true
		}
	}
	return false
}

// The preview is what runs: a `${NAME:-default}` the engine resolves
// unconditionally is resolved in the preview too, in all three bodies — so
// the text held to `bash -n`, and the text the operator reads, is the text
// the first paid run will hand the shell.
func TestDryRunResolvesTheUnconditionalDefault(t *testing.T) {
	bodies, joined := previewBodies(t)
	for _, want := range []string{"echo fallback", "test -n fallback", "printf %s fallback"} {
		if !anyBody(bodies, want) {
			t.Errorf("the preview did not resolve a `:-` body to %q — the dry run checks text the engine will not hand the reader:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "${DRYRUN_PROBE_DIAL") {
		t.Errorf("a `${NAME:-default}` reached the preview as written:\n%s", joined)
	}
}

// And the preview reads the same on every machine: the lookup it gives the
// expansion answers nothing, so neither arm consults THIS process. A plain
// `${NAME}` stays as written — whether the engine answers it depends on the
// engine's own process, which the validating process is not — and a
// `${NAME:-default}` resolves to the author's default even when this host
// carries the name. The doctrine `validate` already holds a launch value to
// (ir.ResolveVarTextAsWritten, #1607), extended to tool bodies.
func TestDryRunDoesNotReadTheHostEnvironment(t *testing.T) {
	t.Setenv("DRYRUN_PROBE_PLAIN", "laptop")
	t.Setenv("DRYRUN_PROBE_DIAL", "laptop")
	bodies, joined := previewBodies(t)
	if !anyBody(bodies, "echo ${DRYRUN_PROBE_PLAIN}") {
		t.Errorf("the plain read was not kept as written — the preview answered it from THIS process, so the verdict follows the machine:\n%s", joined)
	}
	if !anyBody(bodies, "echo fallback") {
		t.Errorf("the `:-` read did not resolve to the author's default — the preview answered it from THIS process:\n%s", joined)
	}
	if strings.Contains(joined, "laptop") {
		t.Errorf("the preview substituted this host's environment value:\n%s", joined)
	}
}

// A body a syntax check refuses is attributed where the PREVIEW's own answer
// may be the reason: a `${NAME:-default}` the preview resolved to the
// author's default reads differently on an engine whose process carries the
// name. A plain `${NAME}` is kept as written, so a refusal there is the
// author's text and carries no clause — one scenario per arm, each isolated,
// so neither guard covers the other.
func TestAShellSyntaxFindingNamesThePreviewsOwnAnswer(t *testing.T) {
	const bot = `tool guarded:
  command: "echo ${R1_PREVIEW_SUFFIX:-|}"

tool plain:
  command: "if [ -n \"${R1_PREVIEW_PLAIN}\" ]; then"

workflow attributed:
  worktree: none
  sandbox: none
  entry: guarded
  guarded -> plain
  plain -> done
`
	// The real interpreter is the oracle here: Options{Shell: nil} is Bash{},
	// which feeds the text to `bash -n` on stdin — a parse, nothing runs.
	rep, err := Run(context.Background(), compileBot(t, bot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	const clause = "the preview answered ${R1_PREVIEW_SUFFIX:-…} with the author's default"
	details := map[string]string{}
	for _, f := range rep.Findings {
		if f.Kind == KindShellSyntax {
			details[f.Node] = f.Detail
		}
	}
	if len(details) != 2 {
		t.Fatalf("the two bodies did not both reach a syntax refusal (%d did): the attribution would be asserted on nothing — %+v", len(details), details)
	}
	if !strings.Contains(details["guarded"], clause) {
		t.Errorf("the refusal on the `:-` body does not name the preview's own answer — the operator reads a bare parser error on a body that parses as written:\n%s", details["guarded"])
	}
	if strings.Contains(details["plain"], "the preview answered") {
		t.Errorf("the refusal on the plain `${NAME}` body was attributed to the preview's answer — a plain read is kept as written, so that text IS the author's:\n%s", details["plain"])
	}
}

// A node that declares `env_expansion: none` (ADR-130) previews the body its
// author wrote: the preview is the one place a reader looks before paying for
// a run, so the property has to be visible there as the text itself. The
// `:-` arm is the discriminator — the preview resolves it to the default on
// every other node, so a body that keeps it is a body the engine declined.
func TestDryRunKeepsADeclinedBodyVerbatim(t *testing.T) {
	const bot = `tool declined:
  command: "echo ${DRYRUN_DECLINED:-fallback}"
  env_expansion: none
  goal: "g"
  postcondition: "test -n ${DRYRUN_DECLINED:-fallback}"

tool scripted:
  language: sh
  env_expansion: none
  script: "printf %s ${DRYRUN_DECLINED:-fallback}"

workflow declined:
  worktree: none
  sandbox: none
  entry: declined
  declined -> scripted
  scripted -> done
`
	rec := &bodyRecorder{}
	if _, err := Run(context.Background(), compileBot(t, bot), Options{Shell: rec}); err != nil {
		t.Fatal(err)
	}
	bodies := rec.bodies()
	joined := strings.Join(bodies, "\n")
	// Every pass of the walk prepares the same three bodies, so the count is
	// a multiple of three; what this witness reads is the TEXT, and a run
	// that prepared nothing would assert nothing.
	if len(bodies) == 0 {
		t.Fatal("the preview prepared no body at all: the witness would pass on nothing")
	}
	for _, want := range []string{
		"echo ${DRYRUN_DECLINED:-fallback}",
		"test -n ${DRYRUN_DECLINED:-fallback}",
		"printf %s ${DRYRUN_DECLINED:-fallback}",
	} {
		if !anyBody(bodies, want) {
			t.Errorf("the preview did not keep %q verbatim — it shows a body the engine will not hand the reader, and the declaration is invisible where an author looks:\n%s", want, joined)
		}
	}
	for _, resolved := range []string{"echo fallback", "test -n fallback", "printf %s fallback"} {
		if anyBody(bodies, resolved) {
			t.Errorf("the preview resolved a form the node declined, to %q:\n%s", resolved, joined)
		}
	}
}

// previewRenderAllowed lists the files that may render a tool body WITHOUT
// reading its environment forms first, keyed by the path from the module
// root, with the reason. The class is "sites that prepare a tool body for a
// reader": a new one that renders the authored text alone previews — and
// holds to `bash -n` — a command the engine will not hand the shell, which
// is the divergence bodyEnv closes.
var previewRenderAllowed = map[string]string{}

// previewRenderers are the exported renderers a preview reaches a tool body
// through. They are pkg/backend/model's door out: inside that package the
// class is held by its own sweeps (tool_body_env_class_test.go), and every
// caller ANYWHERE ELSE in the module is held here.
var previewRenderers = map[string]bool{"RenderCommand": true, "RenderScript": true}

// modelImportPath is the package whose renderers this sweep follows.
const modelImportPath = "github.com/SocialGouv/iterion/pkg/backend/model"

// previewChokepointWrappers names the local helpers whose answer IS the
// chokepoint's, with the reason. A helper spelled like one of these proves
// nothing on its own: every BINDING of the name — a method, a package-level
// `var`, a local closure — is held to calling ToolBodyEnv itself, and a body
// taken from any OTHER name is not the chokepoint's answer, whatever it is
// called.
var previewChokepointWrappers = map[string]string{
	"bodyEnv": "pkg/dryrun's own wrapper: it calls model.ToolBodyEnv with the answers-nothing lookup and returns the body beside the forms it read",
}

// previewNoEnvLookups are the lookups a preview may hand the chokepoint,
// keyed by the spelling of the argument, with the reason. A preview reads the
// same on a laptop and in CI, so the only environment it may speak for is the
// one that answers no name. Any other lookup — this host's, the engine's own
// chain — makes the verdict follow the machine the check ran on and can print
// a host value into a report, so it is refused here rather than read as "the
// chokepoint's answer": the body's provenance alone says nothing about WHICH
// environment answered it.
var previewNoEnvLookups = map[string]string{
	"nil": "model.ToolBodyEnv's answers-nothing contract: a nil lookup becomes the environment that answers no name",
}

// previewAuthoredBodies are the fields of ir.ToolNode the engine hands a
// reader, with the body each is. This is the sweep's REVERSE pin, and it is
// keyed by the authored VALUE rather than by a helper: the three arms of the
// preview are sibling blocks of ONE function binding one name, so a sweep
// that only asks "did some reading precede this render" reads a reverted arm
// as its neighbour's answer. Each authored body must be handed to a reading,
// so an arm that stops reading is NAMED.
var previewAuthoredBodies = map[string]string{
	"Command":       "the `command:` body",
	"Script":        "the `script:` body",
	"Postcondition": "the `postcondition:` body",
}

// wrapperDecl is one binding of a named chokepoint wrapper: where it is,
// whether the sweep read a body for it that calls the chokepoint, and why not
// where it could not.
type wrapperDecl struct {
	name string
	file string
	line string
	read bool
	why  string
}

// A walk that lost the tree finds no offender and reads exactly like a clean
// run, so what was walked is asserted two ways: previewWalkFloor catches a
// walk that collapsed, previewAnchorDirs catch a skip rule that ate ONE
// parent, which a total cannot — pkg/ carries 1567 of the 1832 non-test Go
// files this module has today, so losing every other top-level directory
// still clears any tolerable floor.
//
// Seven directories are anchored; bots/ holds exactly one non-test file
// (bots/embed.go, the catalogue's embed) and e2e/ eleven, which hide nothing
// on their own — they are named here rather than left to chance, so a skip
// rule that eats one of them is said instead of passing.
const previewWalkFloor = 1200

var previewAnchorDirs = []string{"pkg", "cmd", "internal", "bots", "e2e", "third_party", "contrib"}

// renderSite is one mention of a preview renderer: where it is, and whether
// the body it renders is the chokepoint's answer.
type renderSite struct {
	file string
	line string
	arg  string
	read bool
}

// TestEveryPreviewOfABodyReadsItsEnvironmentFirst: every call to the model's
// exported renderers, anywhere in the module, takes its body from the
// chokepoint — `model.ToolBodyEnv` directly, or a `bodyEnv` helper that
// wraps it — and the reading happens BEFORE the call. The sweep is
// module-wide because the exported pair is the door a preview in ANY package
// reaches a tool body through: a server-side settings preview or a new CLI
// render would otherwise be pinned by nothing.
//
// A mention this sweep cannot read as a compliant call — a renderer taken as
// a function VALUE, a body argument whose provenance is some other
// expression, a reading handed an environment — is reported, not assumed
// sound: the shapes it was not written for fall on the side that fails.
func TestEveryPreviewOfABodyReadsItsEnvironmentFirst(t *testing.T) {
	root := filepath.Join("..", "..")
	previewSite := filepath.Join("pkg", "dryrun", "executor.go")
	skipNames := map[string]bool{"vendor": true, "node_modules": true, "studio": true}
	parsed := 0
	perDir := map[string]int{}
	var sites []renderSite
	var wrappers []wrapperDecl
	authored := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			// Hidden directories hold worktrees and scratch clones; vendor/
			// and node_modules/ are other people's trees; studio/ is TypeScript.
			if path != root && (strings.HasPrefix(info.Name(), ".") || skipNames[info.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		parsed++
		if top := strings.Split(rel, string(filepath.Separator))[0]; top != rel {
			perDir[top]++
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		sites = append(sites, renderSitesIn(fset, f, rel)...)
		wrappers = append(wrappers, chokepointWrappersIn(fset, f, rel)...)
		if rel == previewSite {
			for field := range authoredBodiesReadIn(f) {
				authored[field] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if parsed < previewWalkFloor {
		t.Fatalf("the walk parsed %d non-test Go files of this module, floor %d — the sweep's subject is not the module", parsed, previewWalkFloor)
	}
	for _, dir := range previewAnchorDirs {
		if perDir[dir] == 0 {
			t.Fatalf("the walk read no Go file under %s/ — a skip rule ate a parent of the class, which the total above cannot show", dir)
		}
	}
	if len(sites) == 0 {
		t.Fatalf("nothing in this module calls %s's exported renderers: the sweep's subject is gone and it proves nothing", modelImportPath)
	}
	for _, s := range sites {
		if why, ok := previewRenderAllowed[s.file]; ok {
			_ = why
			continue
		}
		if !s.read {
			t.Errorf("%s:%s renders a tool body whose text is not the chokepoint's answer (%s) — read the environment forms first (model.ToolBodyEnv, or this package's bodyEnv), bind the result and render THAT, or add an allowlist entry with its reason; a preview of the authored text alone holds to `bash -n` a command the engine will not hand the shell", s.file, s.line, s.arg)
		}
	}
	for file, why := range previewRenderAllowed {
		found := false
		for _, s := range sites {
			if s.file == file {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("previewRenderAllowed still names %s (%q) but it renders no tool body any more — drop the entry, so the list keeps saying what is true", file, why)
		}
	}
	// The sweep must SEE the preview it exists for. A provenance recogniser
	// that read nothing as compliant — an import resolved under the wrong
	// name, a shape it no longer parses — would report no offender either,
	// and read exactly like a clean run.
	answered := 0
	for _, s := range sites {
		if s.file == previewSite && s.read {
			answered++
		}
	}
	if answered < 3 {
		t.Errorf("%s holds %d renderer calls this sweep read as the chokepoint's answer, want the three tool bodies (`command:`, `script:`, `postcondition:`) — a recogniser that reads none of them reports no offender either", previewSite, answered)
	}
	// The reverse pin: each authored body the engine hands a reader is handed
	// to a reading at the preview site, named by its own field. The count
	// above is a liveness check and three sibling arms can satisfy it with
	// two readings and one revert; this names the arm that stopped reading.
	for field, body := range previewAuthoredBodies {
		if !authored[field] {
			t.Errorf("%s hands %s (n.%s) to no reading of its environment forms — the preview renders the text the author wrote and holds THAT to the interpreter's parser, which is not the text the engine will hand the shell", previewSite, body, field)
		}
	}
	// Each named wrapper earns its name from its own binding, not from the
	// list: a helper called bodyEnv that reads nothing would launder every
	// site it feeds, and so would one spelled as a value.
	declared := map[string]int{}
	for _, w := range wrappers {
		declared[w.name]++
		if !w.read {
			t.Errorf("%s:%s binds %s, which this sweep reads as the chokepoint's answer, but %s — a wrapper that reads nothing launders every site it feeds; call the chokepoint there, or rename the binding", w.file, w.line, w.name, w.why)
		}
	}
	for name, why := range previewChokepointWrappers {
		if declared[name] == 0 {
			t.Errorf("previewChokepointWrappers still names %s (%q) but nothing in this module declares it — drop the entry, so a body taken from that name stops reading as the chokepoint's answer", name, why)
		}
	}
}

// renderSitesIn lists every mention of a preview renderer in f, with the
// provenance of the body each renders. The import is resolved by PATH, so a
// local alias or a dot-import is read like any other spelling.
func renderSitesIn(fset *token.FileSet, f *ast.File, rel string) []renderSite {
	local := ""
	dot := false
	for _, imp := range f.Imports {
		p, uerr := strconv.Unquote(imp.Path.Value)
		if uerr != nil || p != modelImportPath {
			continue
		}
		switch {
		case imp.Name == nil:
			local = "model"
		case imp.Name.Name == ".":
			dot = true
		case imp.Name.Name == "_":
		default:
			local = imp.Name.Name
		}
	}
	if local == "" && !dot {
		return nil
	}
	// renderer reports whether e names one of the preview renderers.
	renderer := func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.SelectorExpr:
			if id, ok := v.X.(*ast.Ident); ok && id.Name == local && local != "" && previewRenderers[v.Sel.Name] {
				return v.Sel.Name
			}
		case *ast.Ident:
			if dot && previewRenderers[v.Name] {
				return v.Name
			}
		}
		return ""
	}
	// Each statement list is read on its own, and a reading speaks only for
	// the list that binds it. The preview's three arms are sibling blocks of
	// ONE function binding one name, so a reading "somewhere earlier in the
	// file" — or even earlier in the same function — would read a reverted arm
	// as its neighbour's answer; and a body rebound after the reading is the
	// same defect, which a per-function scope also misses.
	compliant := map[token.Pos]bool{}
	refused := map[token.Pos]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		var list []ast.Stmt
		switch v := n.(type) {
		case *ast.BlockStmt:
			list = v.List
		case *ast.CaseClause:
			list = v.Body
		case *ast.CommClause:
			list = v.Body
		default:
			return true
		}
		noteCompliantRenders(list, renderer, compliant, refused)
		return true
	})
	var sites []renderSite
	ast.Inspect(f, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok || renderer(e) == "" || compliant[e.Pos()] {
			return true
		}
		why, read := refused[e.Pos()]
		if !read {
			why = "this mention is not a renderer call inside a statement list: a renderer taken as a function VALUE, or called outside any function body, is held to nothing"
		}
		sites = append(sites, renderSite{
			file: rel,
			line: strconv.Itoa(fset.Position(e.Pos()).Line),
			arg:  why,
			read: false,
		})
		return true
	})
	for pos := range compliant {
		sites = append(sites, renderSite{file: rel, line: strconv.Itoa(fset.Position(pos).Line), arg: "the chokepoint's answer", read: true})
	}
	return sites
}

// noteCompliantRenders reads ONE statement list and marks each renderer call
// it owns compliant, or refused with the reason. The walk stops at every
// nested statement list: the walk that reaches that list judges it against
// ITS own bindings, so no reading of an enclosing list ever speaks for a body
// prepared — or tampered with — inside a nested block.
func noteCompliantRenders(list []ast.Stmt, renderer func(ast.Expr) string, compliant map[token.Pos]bool, refused map[token.Pos]string) {
	// bound holds, for each name this list binds, whether the binding IS a
	// reading; refusedBind carries why a binding that looked like one is not.
	bound := map[string]bool{}
	refusedBind := map[string]string{}
	for _, stmt := range list {
		if !opensStatementList(stmt) {
			inspectOutsideNestedLists(stmt, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall || renderer(call.Fun) == "" {
					return true
				}
				if len(call.Args) == 0 {
					refused[call.Fun.Pos()] = "the renderer is called with no body at all"
					return true
				}
				ok, why := previewChokepointCall(call.Args[0])
				if id, isIdent := call.Args[0].(*ast.Ident); isIdent && !ok {
					switch {
					case bound[id.Name]:
						ok = true
					case refusedBind[id.Name] != "":
						why = "the body is " + id.Name + ", bound in this statement list by " + refusedBind[id.Name]
					default:
						why = "the body is " + id.Name + ", which no reading in this statement list bound"
					}
				}
				if ok {
					compliant[call.Fun.Pos()] = true
				} else {
					refused[call.Fun.Pos()] = why
				}
				return true
			})
		}
		noteBindings(stmt, bound, refusedBind)
	}
}

// noteBindings records what this statement binds: the first result of a
// reading is a body the chokepoint answered, and every other name it binds is
// not — including a name it REBINDS, which forgets the reading that bound it
// before.
func noteBindings(stmt ast.Stmt, bound map[string]bool, refusedBind map[string]string) {
	note := func(names []ast.Expr, read bool, why string) {
		for i, e := range names {
			id, isIdent := e.(*ast.Ident)
			if !isIdent || id.Name == "_" {
				continue
			}
			bound[id.Name] = read && i == 0
			if read && i == 0 {
				delete(refusedBind, id.Name)
				continue
			}
			if i == 0 {
				refusedBind[id.Name] = why
			} else {
				refusedBind[id.Name] = "a result of a reading that is not the body it answered"
			}
		}
	}
	switch v := stmt.(type) {
	case *ast.AssignStmt:
		if len(v.Rhs) != 1 {
			note(v.Lhs, false, "an assignment of several values, which is not a reading")
			return
		}
		ok, why := previewChokepointCall(v.Rhs[0])
		note(v.Lhs, ok, why)
	case *ast.DeclStmt:
		gen, isGen := v.Decl.(*ast.GenDecl)
		if !isGen {
			return
		}
		for _, spec := range gen.Specs {
			vs, isVS := spec.(*ast.ValueSpec)
			if !isVS {
				continue
			}
			names := make([]ast.Expr, 0, len(vs.Names))
			for _, nm := range vs.Names {
				names = append(names, nm)
			}
			if len(vs.Values) != 1 {
				note(names, false, "a declaration that is not a reading")
				continue
			}
			ok, why := previewChokepointCall(vs.Values[0])
			note(names, ok, why)
		}
	}
}

// opensStatementList reports whether n owns a statement list of its own.
func opensStatementList(n ast.Node) bool {
	switch n.(type) {
	case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause, *ast.FuncLit:
		return true
	}
	return false
}

// inspectOutsideNestedLists walks n without entering a nested statement list.
func inspectOutsideNestedLists(n ast.Node, visit func(ast.Node) bool) {
	ast.Inspect(n, func(c ast.Node) bool {
		if c == nil {
			return false
		}
		if c != n && opensStatementList(c) {
			return false
		}
		return visit(c)
	})
}

// chokepointWrappersIn lists every BINDING of a named chokepoint wrapper in
// f, with whether the sweep read a body for it that calls ToolBodyEnv. A
// wrapper earns its name from the binding, never from the list: the three
// shapes a reading can legitimately be spelled as are a method or function
// declaration, a package-level `var`, and a local closure. EVERY other
// occurrence of the name outside a callee position — a struct field or
// parameter of func type, a method value handed around, a shape this sweep
// does not parse — is reported, because a body taken from it reads as the
// chokepoint's answer and nothing holds it to the reading.
func chokepointWrappersIn(fset *token.FileSet, f *ast.File, rel string) []wrapperDecl {
	callees := calleeIdents(f)
	accounted := map[*ast.Ident]bool{}
	named := func(id *ast.Ident) bool {
		if id == nil || callees[id] {
			return false
		}
		_, ok := previewChokepointWrappers[id.Name]
		return ok
	}
	var out []wrapperDecl
	add := func(id *ast.Ident, read bool, why string) {
		accounted[id] = true
		out = append(out, wrapperDecl{
			name: id.Name,
			file: rel,
			line: strconv.Itoa(fset.Position(id.Pos()).Line),
			read: read,
			why:  why,
		})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncDecl:
			if named(v.Name) {
				if v.Body == nil {
					add(v.Name, false, "the declaration has no body this sweep can read")
					return true
				}
				add(v.Name, bodyReadsChokepoint(v.Body), "its body calls no model.ToolBodyEnv under a lookup a preview may speak for")
			}
		case *ast.ValueSpec:
			for i, nm := range v.Names {
				if !named(nm) {
					continue
				}
				add(nm, i < len(v.Values) && funcLitReadsChokepoint(v.Values[i]), "it is bound to something other than a function literal that calls model.ToolBodyEnv")
			}
		case *ast.AssignStmt:
			for i, lhs := range v.Lhs {
				id, isIdent := lhs.(*ast.Ident)
				if !isIdent || !named(id) {
					continue
				}
				add(id, i < len(v.Rhs) && funcLitReadsChokepoint(v.Rhs[i]), "it is assigned something other than a function literal that calls model.ToolBodyEnv")
			}
		case *ast.Field:
			for _, nm := range v.Names {
				if named(nm) {
					add(nm, false, "it is a field or a parameter: nothing holds whatever is stored in it to reading the environment")
				}
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		id, isIdent := n.(*ast.Ident)
		if !isIdent || !named(id) || accounted[id] {
			return true
		}
		add(id, false, "the name appears outside a call and outside a declaration this sweep can read")
		return true
	})
	return out
}

// calleeIdents are the identifiers that NAME the function a call calls: the
// `f` of `f(…)` and the `f` of `x.f(…)`. Every OTHER occurrence of a
// wrapper's name binds the wrapper or hands it around, which is what
// chokepointWrappersIn holds.
func calleeIdents(f *ast.File) map[*ast.Ident]bool {
	out := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			out[fun] = true
		case *ast.SelectorExpr:
			out[fun.Sel] = true
		}
		return true
	})
	return out
}

// bodyReadsChokepoint reports whether body calls model.ToolBodyEnv under a
// lookup a preview may speak for. A wrapper that calls ANOTHER wrapper proves
// nothing: the name is what is in question here.
func bodyReadsChokepoint(body *ast.BlockStmt) bool {
	reads := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall || callee(call.Fun) != "ToolBodyEnv" {
			return true
		}
		if ok, _ := previewLookupArg(call); ok {
			reads = true
		}
		return true
	})
	return reads
}

// funcLitReadsChokepoint reports whether e is a function literal whose body
// reads the environment at the chokepoint.
func funcLitReadsChokepoint(e ast.Expr) bool {
	lit, isLit := e.(*ast.FuncLit)
	return isLit && bodyReadsChokepoint(lit.Body)
}

// authoredBodiesReadIn lists the authored body fields f hands to a reading.
// Every argument of the call is looked at, so the pin does not re-model the
// chokepoint's parameter order.
func authoredBodiesReadIn(f *ast.File) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		if ok, _ := previewChokepointCall(call); !ok {
			return true
		}
		for _, arg := range call.Args {
			sel, isSel := arg.(*ast.SelectorExpr)
			if !isSel {
				continue
			}
			if _, isBody := previewAuthoredBodies[sel.Sel.Name]; isBody {
				out[sel.Sel.Name] = true
			}
		}
		return true
	})
	return out
}

// previewChokepointCall reports whether e is a call that returns a body the
// chokepoint read: ToolBodyEnv itself, under a lookup a preview may speak
// for, or a wrapper previewChokepointWrappers names — whose own binding this
// sweep holds to the same rule. The reason comes back with the verdict, so a
// refusal says WHY instead of "not the chokepoint".
func previewChokepointCall(e ast.Expr) (bool, string) {
	call, isCall := e.(*ast.CallExpr)
	if !isCall {
		return false, "a value no reading produced (" + exprName(e) + ")"
	}
	name := callee(call.Fun)
	if name == "ToolBodyEnv" {
		return previewLookupArg(call)
	}
	if _, isWrapper := previewChokepointWrappers[name]; isWrapper {
		return true, ""
	}
	return false, "a call to " + exprName(call.Fun) + "(…), which is not a reading of the body's environment forms"
}

// previewLookupArg holds a direct ToolBodyEnv call to the lookup a preview
// may hand it. The lookup is the last argument of ADR-130's signature
// (node, body, lookup); a call of any other arity is refused rather than
// guessed at, so a signature change falls on the side that fails.
func previewLookupArg(call *ast.CallExpr) (bool, string) {
	if len(call.Args) != 3 {
		return false, fmt.Sprintf("a ToolBodyEnv call of %d arguments, not the (node, body, lookup) this sweep reads the lookup from", len(call.Args))
	}
	spelled := exprName(call.Args[2])
	if _, ok := previewNoEnvLookups[spelled]; ok {
		return true, ""
	}
	return false, "a ToolBodyEnv call handed the lookup " + spelled + ", so the preview speaks for an environment: the verdict would follow the machine the check ran on, and a host value could be printed into a report"
}

// callee names the function a call expression calls, selector or bare.
func callee(fun ast.Expr) string {
	switch v := fun.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.Ident:
		return v.Name
	}
	return ""
}

// exprName spells an expression as a failure message should read it.
func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprName(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprName(v.Fun) + "(…)"
	case *ast.BasicLit:
		return v.Value
	case *ast.FuncLit:
		return "a function literal"
	}
	return "an expression this sweep does not read"
}
