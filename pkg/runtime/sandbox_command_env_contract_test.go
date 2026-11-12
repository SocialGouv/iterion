package runtime

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/modtypes"
)

// This inventory names every reference to a sandbox command factory or SDK
// CommandBuilder in the code the current platform builds. It uses resolved Go
// types, not the spelling Command: import aliases, method values/expressions,
// callback aliases and references in nested closures remain visible. The count
// is per owning function, so a second arrival in an existing caller fails too.
//
// This is an arrival guard, not a whole-program Env flow analysis. Each row
// names a behavioral proof that executes the real caller. Delegates, wrappers
// and the shared ExecCmd helper receive a driver-only nonce absent from the
// ambient environment; Docker and noop exercise their real Command payload.
// Kubernetes' native transport test checks the captured operator client and
// environment separately from the workload envelope. The shared ExecCmd proof
// exercises both buffered and streamed paths with a driver-only environment.
// Changes to a known caller still owe their behavioral tests and review. Like
// the compiler, modtypes does not read files excluded by platform/build tags;
// other platform builds must run their corresponding inventory.
type commandEnvProof struct {
	References int
	Proof      string // package path:TestName; checked against actual test source
}

var commandEnvProofs = map[string]commandEnvProof{
	"pkg/backend/delegate/claude_code.go:(*ClaudeCodeBackend).buildSpawnOptions":    {1, "pkg/backend/delegate:TestDelegateCommandPreservesDriverEnvironmentClaude"},
	"pkg/backend/delegate/claude_code.go:(*ClaudeCodeBackend).formatOutput":         {1, "pkg/backend/delegate:TestDelegateCommandPreservesDriverEnvironmentClaude"},
	"pkg/backend/delegate/claude_code_sandbox_kill.go:killSandboxDelegateReporting": {1, "pkg/backend/delegate:TestDelegateCommandPreservesDriverEnvironmentCleanup"},
	"pkg/backend/delegate/claudesdk/claude.go:Prompt":                               {2, "pkg/backend/delegate/claudesdk:TestCommandBuilderPreservesDriverEnvironment"},
	"pkg/backend/delegate/claudesdk/option.go:WithCommandBuilder":                   {2, "pkg/backend/delegate/claudesdk:TestCommandBuilderPreservesDriverEnvironment"},
	"pkg/backend/delegate/claudesdk/process.go:resolveCLIPath":                      {1, "pkg/backend/delegate/claudesdk:TestCommandBuilderPreservesDriverEnvironment"},
	"pkg/backend/delegate/claudesdk/process.go:spawnProcess":                        {3, "pkg/backend/delegate/claudesdk:TestCommandBuilderPreservesDriverEnvironment"},
	"pkg/backend/delegate/claudesdk/session.go:(*Session).ensureStarted":            {2, "pkg/backend/delegate/claudesdk:TestCommandBuilderPreservesDriverEnvironment"},
	"pkg/backend/delegate/cliagent.go:(*CLIAgentBackend).runOnce":                   {1, "pkg/backend/delegate:TestDelegateCommandPreservesDriverEnvironmentCLI"},
	"pkg/backend/delegate/pi_rpc.go:(*PiRPCBackend).spawner":                        {1, "pkg/backend/delegate:TestDelegateCommandPreservesDriverEnvironmentPi"},
	"pkg/backend/model/claw_backend.go:(*ClawBackend).executeViaSandboxRunner":      {1, "pkg/backend/model:TestClawRunnerPreservesDriverTransport"},
	"pkg/backend/model/executor_tool.go:(*ClawExecutor).sandboxToolCommand":         {2, "pkg/backend/model:TestSandboxToolConstructorsPreserveDriverTransport"},
	"pkg/runtime/child_sandbox_resources.go:(*childPathRun).Command":                {1, "pkg/runtime:TestChildPathCommandPreservesDriverEnvironment"},
	"pkg/sandbox/completion.go:(*Completion).sweepOnce":                             {1, "pkg/sandbox:TestCompletionSweepPreservesDriverEnvironment"},
	"pkg/sandbox/docker/driver.go:(*Run).Exec":                                      {1, "pkg/sandbox/docker:TestExecPreservesDriverEnvironment"},
	"pkg/sandbox/kubernetes/driver.go:(*Run).Command":                               {1, "pkg/sandbox/kubernetes:TestExecTransportCommandUsesCapturedProfileAndClient"},
	"pkg/sandbox/kubernetes/driver.go:(*Run).Exec":                                  {1, "pkg/sandbox/kubernetes:TestExecPreservesDriverEnvironment"},
	"pkg/sandbox/noop/noop.go:(*run).Exec":                                          {1, "pkg/sandbox/noop:TestExecPreservesDriverEnvironment"},
}

// Structural identity keeps aliases transparent at every compound component.
// Named types use package path/name because modtypes checks a source package
// against independent export-data package objects.
func commandEnvType(t types.Type) string {
	switch t := types.Unalias(t).(type) {
	case *types.Pointer:
		return "*" + commandEnvType(t.Elem())
	case *types.Slice:
		return "[]" + commandEnvType(t.Elem())
	case *types.Map:
		return "map[" + commandEnvType(t.Key()) + "]" + commandEnvType(t.Elem())
	case *types.Basic:
		return t.Name()
	case *types.Named:
		obj := t.Obj()
		if obj.Pkg() != nil {
			// A local definition can shadow a package contract's name.
			if obj.Pkg().Scope().Lookup(obj.Name()) != obj {
				return "<local:" + obj.Name() + ">"
			}
			return obj.Pkg().Path() + "." + obj.Name()
		}
		return obj.Name()
	default:
		return types.TypeString(t, func(p *types.Package) string { return p.Path() })
	}
}

func isCommandEnvFactory(t types.Type) bool {
	if t == nil {
		return false
	}
	sig, ok := types.Unalias(t).Underlying().(*types.Signature)
	if !ok || sig.Results().Len() != 1 || commandEnvType(sig.Results().At(0).Type()) != "*os/exec.Cmd" {
		return false
	}
	// A method expression adds a receiver before the same parameters. Named
	// function types and aliases retain the same underlying signature.
	for _, params := range [][]string{
		{"context.Context", "[]string", "github.com/SocialGouv/iterion/pkg/sandbox.ExecOpts"},
		{"context.Context", "string", "[]string", "string", "map[string]string", "bool"},
	} {
		n := sig.Params().Len()
		if n < len(params) {
			continue
		}
		match := true
		for i, want := range params {
			match = match && commandEnvType(sig.Params().At(n-len(params)+i).Type()) == want
		}
		if match {
			return true
		}
	}
	return false
}

func commandEnvReferences(pkg *modtypes.Package) map[string]int {
	counts := map[string]int{}
	for i, file := range pkg.Files {
		for _, decl := range file.Decls {
			var owner string
			var node ast.Node
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				owner, node = d.Name.Name, d.Body
				if d.Recv != nil {
					owner = "(" + types.ExprString(d.Recv.List[0].Type) + ")." + owner
				}
			case *ast.GenDecl:
				owner, node = "<package>", d
			default:
				continue
			}
			seen := map[token.Pos]bool{}
			record := func(e ast.Expr) {
				if !seen[e.Pos()] && isCommandEnvFactory(pkg.Info.TypeOf(e)) {
					seen[e.Pos()] = true
					counts[pkg.FileRels[i]+":"+owner]++
				}
			}
			ast.Inspect(node, func(n ast.Node) bool {
				// A reference can be any value expression, including a
				// selected callback, a dereference or a returned factory.
				// Type syntax and declarations are not references. Function
				// literals construct a value; their bodies are still read.
				switch n := n.(type) {
				case *ast.FuncLit:
					return true
				case *ast.ParenExpr:
					return true // its inner expression is the same reference
				case *ast.SelectorExpr:
					seen[n.Sel.Pos()] = true
					if _, isType := pkg.Info.Uses[n.Sel].(*types.TypeName); !isType {
						record(n)
					}
				case *ast.Ident:
					switch pkg.Info.Uses[n].(type) {
					case *types.Var, *types.Func:
						record(n)
					}
				case ast.Expr:
					if tv, ok := pkg.Info.Types[n]; ok && tv.IsValue() {
						record(n)
					}
				}
				return true
			})
		}
	}
	return counts
}

func TestSandboxCommandEnvironmentCallersHaveBehavioralProofs(t *testing.T) {
	root := repoRootForInventory(t)
	module, err := modtypes.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, pkg := range module.Packages {
		for site, count := range commandEnvReferences(pkg) {
			found[site] += count
		}
	}
	for _, site := range slices.Sorted(maps.Keys(found)) {
		proof, ok := commandEnvProofs[site]
		if !ok || proof.References != found[site] {
			t.Errorf("COMMAND_ENV_ARRIVAL: %s has %d factory references; reviewed %d; add or update its executed driver-environment proof", site, found[site], proof.References)
		}
	}
	for site, proof := range commandEnvProofs {
		if found[site] == 0 {
			t.Errorf("stale command environment row: %s", site)
		}
		if !commandEnvProofExists(t, root, proof.Proof) {
			t.Errorf("missing behavioral proof %q for %s", proof.Proof, site)
		}
	}
	if !commandEnvProofExists(t, root, "pkg/sandbox:TestExecCmdPreservesDriverEnvironment") {
		t.Error("missing shared ExecCmd transport environment proof")
	}
}

func commandEnvProofExists(t *testing.T, root, proof string) bool {
	t.Helper()
	dir, name, ok := strings.Cut(proof, ":")
	if !ok || !strings.HasPrefix(name, "Test") {
		return false
	}
	files, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range files {
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, dir, entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name && fn.Body != nil {
				return true
			}
		}
	}
	return false
}

func TestCommandEnvironmentInventoryResolvesTypes(t *testing.T) {
	module, err := modtypes.Load(repoRootForInventory(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, imports, body string
		want                int
	}{
		{"direct", "", `_ = r.Command(ctx, nil, sb.ExecOpts{})`, 1},
		{"method value", "", `f := r.Command; _ = f(ctx, nil, sb.ExecOpts{})`, 2},
		{"method expression", "", `f := sb.Run.Command; _ = f(r, ctx, nil, sb.ExecOpts{})`, 2},
		{"nested capture", "", `f := func() { _ = r.Command(ctx, nil, sb.ExecOpts{}) }; f()`, 1},
		{"two arrivals", "", `_ = r.Command(ctx,nil,sb.ExecOpts{}); _ = r.Command(ctx,nil,sb.ExecOpts{})`, 2},
		{"type alias", "", `type R = sb.Run; var other R = r; _ = other.Command(ctx,nil,sb.ExecOpts{})`, 1},
		{"callback handed onward", `sdk "github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"`, `var f sdk.CommandBuilder; _ = sdk.WithCommandBuilder(f)`, 1},
		{"data named Command", "", `v := struct{Command string}{"workload"}; _ = v.Command; _ = ctx; _ = r`, 0},
		{"valid env alias", `os "os"; ex "os/exec"`, `var c *ex.Cmd = r.Command(ctx,nil,sb.ExecOpts{}); seed := c.Environ(); c.Env = append(seed, os.Environ()...)`, 1},
		{"valid branches", `"os"`, `c := r.Command(ctx,nil,sb.ExecOpts{}); var seed []string; switch {case ctx.Err()==nil: seed=c.Environ(); default: seed=c.Environ()}; c.Env=append(seed,os.Environ()...)`, 1},
		{"valid terminal branch", `"os"`, `c := r.Command(ctx,nil,sb.ExecOpts{}); if ctx.Err()!=nil{return}; c.Env=append(c.Environ(),os.Environ()...)`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf("package probe\nimport (\"context\"; sb \"github.com/SocialGouv/iterion/pkg/sandbox\"; %s)\nfunc caller(ctx context.Context,r sb.Run){%s}", tc.imports, tc.body)
			pkg, err := module.CheckSource("inventory/probe", "new_file.go", src)
			if err != nil {
				t.Fatal(err)
			}
			got := commandEnvReferences(pkg)
			if got["new_file.go:caller"] != tc.want || (tc.want == 0 && len(got) != 0) {
				t.Fatalf("inventory = %v, want caller %d", got, tc.want)
			}
		})
	}
}
