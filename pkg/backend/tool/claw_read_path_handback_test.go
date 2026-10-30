package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The hand-back contract of the read tools (#2650): a path one read tool
// EMITS, handed straight back to read_file — which is what an agent does
// with a glob or grep result — must open the file the search found.
//
// read_file joins a RELATIVE path onto the workspace, so a result spelled
// relative to an extra read root opens the workspace's homonym instead:
// no error, no refusal, the wrong bytes. The decoy below is the whole
// point — the same relative path exists under both roots with different
// content.
//
// The set under test is every builtin whose executor is handed the
// ReadScope (grep `scope *ReadScope` in this package: read_file, glob,
// grep, and the workspace_grep alias of the diagnostics registry) — plus
// read_image, whose registration takes the scope for its `path` input
// (#2885). A fourth scope-gated tool belongs in this table; resolving
// each name from the registry is what fails if one is renamed away.

const (
	handbackScratchBody   = "SCRATCH-BRIEF\n"
	handbackWorkspaceBody = "WORKSPACE-DECOY\n"
)

var grepMatchPathRe = regexp.MustCompile(`(?m)^(.+?):\d+:`)

// handbackFixture plants briefs/lot-01.md under BOTH the workspace and a
// scratch root outside it, with different content, and returns the scope,
// the two roots and a registry carrying the builtins plus the diagnostics
// alias.
func handbackFixture(t *testing.T) (*ReadScope, *Registry, string, string) {
	t.Helper()
	workspace := t.TempDir()
	scratch := t.TempDir()
	for _, plant := range []struct{ root, body string }{
		{workspace, handbackWorkspaceBody},
		{scratch, handbackScratchBody},
	} {
		if err := os.MkdirAll(filepath.Join(plant.root, "briefs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plant.root, "briefs", "lot-01.md"), []byte(plant.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scope := NewReadScope(workspace)
	scope.AddExtraRoot(scratch)
	reg := NewRegistry()
	if err := RegisterClawBuiltinsWithScope(reg, scope, nil); err != nil {
		t.Fatal(err)
	}
	if err := RegisterClawWorkspaceDiagnosticsWithScope(reg, scope, nil); err != nil {
		t.Fatal(err)
	}
	return scope, reg, workspace, scratch
}

func callTool(t *testing.T, reg *Registry, name string, input map[string]any) string {
	t.Helper()
	def, err := reg.Resolve(name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	out, err := def.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("%s(%v): %v", name, input, err)
	}
	return out
}

// emittedPaths pulls every path a tool's output offers back to read_file.
func emittedPaths(t *testing.T, tool, out string) []string {
	t.Helper()
	var paths []string
	switch tool {
	case "glob":
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line == "" || strings.HasPrefix(line, "... [glob partial") {
				continue
			}
			paths = append(paths, line)
		}
	case "grep", "workspace_grep":
		for _, m := range grepMatchPathRe.FindAllStringSubmatch(out, -1) {
			paths = append(paths, m[1])
		}
	default:
		t.Fatalf("no path extractor for %s", tool)
	}
	if len(paths) == 0 {
		t.Fatalf("%s emitted no path to hand back: %q", tool, out)
	}
	return paths
}

func TestEveryReadToolSpellsAScratchPathReadFileOpensAgain(t *testing.T) {
	_, reg, _, scratch := handbackFixture(t)

	for _, tc := range []struct {
		tool  string
		input map[string]any
	}{
		{"glob", map[string]any{"path": scratch, "pattern": "**/*.md"}},
		{"grep", map[string]any{"path": scratch, "pattern": "SCRATCH"}},
		{"workspace_grep", map[string]any{"path": scratch, "pattern": "SCRATCH"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			out := callTool(t, reg, tc.tool, tc.input)
			for _, path := range emittedPaths(t, tc.tool, out) {
				back := callTool(t, reg, "read_file", map[string]any{"path": path})
				if !strings.Contains(back, "SCRATCH-BRIEF") {
					t.Fatalf("%s emitted %q; read_file of that same string returned %q, not the file %s found under the scratch root",
						tc.tool, path, back, tc.tool)
				}
			}
		})
	}
}

// The workspace spelling is the one agents, prompts and goldens use: a
// search inside the workspace keeps reporting workspace-relative paths,
// which read_file rebases onto that same workspace — the same file.
func TestGlobKeepsTheWorkspaceRelativeSpelling(t *testing.T) {
	_, reg, workspace, _ := handbackFixture(t)

	out := callTool(t, reg, "glob", map[string]any{"path": workspace, "pattern": "**/*.md"})
	paths := emittedPaths(t, "glob", out)
	if len(paths) != 1 || paths[0] != "briefs/lot-01.md" {
		t.Fatalf("glob over the workspace = %v, want exactly the relative %q", paths, "briefs/lot-01.md")
	}
	if back := callTool(t, reg, "read_file", map[string]any{"path": paths[0]}); !strings.Contains(back, "WORKSPACE-DECOY") {
		t.Fatalf("read_file(%q) = %q, want the workspace file", paths[0], back)
	}
}

// The credential exclusion is case-FOLDED: a file system that folds case
// makes .ENV and .env the same file, so a list read in one spelling only
// is a bypass of itself. Both roots, the raw and the resolved path.
func TestCredentialExclusionFoldsCaseUnderEveryRoot(t *testing.T) {
	scope, _, workspace, scratch := handbackFixture(t)

	for _, name := range []string{".ENV", ".Env", ".ENV.local", ".NETRC", "CLI-AUTH.JSON", "ID_RSA", "server.PEM", "ITERION-CREDENTIALS"} {
		for _, root := range []string{workspace, scratch} {
			path := filepath.Join(root, name)
			if err := os.WriteFile(path, []byte("TOKEN=x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := executeWorkspaceReadFile(map[string]any{"path": path}, scope); err == nil ||
				!strings.Contains(err.Error(), "credential") {
				t.Fatalf("read_file(%q) under %s must refuse as a credential, got %v", name, root, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A folded name that is NOT on the list still reads, so the fold did
	// not turn the predicate into "anything uppercase is a secret".
	plain := filepath.Join(workspace, "ENVIRONMENT.md")
	if err := os.WriteFile(plain, []byte("public\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := executeWorkspaceReadFile(map[string]any{"path": plain}, scope); err != nil || !strings.Contains(out, "public") {
		t.Fatalf("read_file(ENVIRONMENT.md) = %q, %v — the fold must not widen the list", out, err)
	}
}

// read_image takes a model-supplied path to the SAME filesystem read_file
// reads, so it answers to the same scope (#2885): the extra roots open it,
// a path outside every root refuses, and the credential exclusion binds.
// Every refusal below is against a file holding VALID PNG bytes, so what
// reddens is the gate, never the format sniff.
func TestReadImageAnswersToTheReadScope(t *testing.T) {
	scope, reg, workspace, scratch := handbackFixture(t)
	if err := RegisterClawReadImage(reg, scope); err != nil {
		t.Fatal(err)
	}
	readImage := func(input map[string]any) (string, error) {
		t.Helper()
		def, err := reg.Resolve("read_image")
		if err != nil {
			t.Fatalf("resolve read_image: %v", err)
		}
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		return def.Execute(context.Background(), raw)
	}

	// The positives: an image under the extra root reads by its absolute
	// path, and the workspace-relative spelling joins the workspace — the
	// two shapes read_file accepts.
	scratchPNG := filepath.Join(scratch, "briefs", "shot.png")
	if err := os.WriteFile(scratchPNG, tiny1x1PNG, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := readImage(map[string]any{"path": scratchPNG}); err != nil || !strings.Contains(out, "image/png") {
		t.Fatalf("read_image(%q) under the scratch root = %q, %v — want the decoded PNG block", scratchPNG, out, err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), tiny1x1PNG, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := readImage(map[string]any{"path": "shot.png"}); err != nil || !strings.Contains(out, "image/png") {
		t.Fatalf("read_image(shot.png) relative to the workspace = %q, %v", out, err)
	}

	// Outside every root: refused the way read_file refuses.
	outside := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(outside, tiny1x1PNG, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readImage(map[string]any{"path": outside}); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("read_image(%q) outside every root = %v, want the scope refusal", outside, err)
	}

	// A credential name under EITHER root: refused, although the bytes
	// decode as a PNG — the exclusion, not the loader, said no.
	for _, root := range []string{workspace, scratch} {
		env := filepath.Join(root, ".env")
		if err := os.WriteFile(env, tiny1x1PNG, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readImage(map[string]any{"path": env}); err == nil || !strings.Contains(err.Error(), "credential") {
			t.Fatalf("read_image(%q) = %v, want the credential refusal", env, err)
		}
		if err := os.Remove(env); err != nil {
			t.Fatal(err)
		}
	}
}
