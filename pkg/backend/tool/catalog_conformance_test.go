package tool

import (
	"context"
	"strings"
	"testing"

	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"

	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// TestClawCatalogMatchesRegistry is the anti-drift guard behind C135.
//
// pkg/backend/toolcatalog holds the compile-time list of names a claw node's
// `tools:` may contain, as literals — it must stay a leaf so pkg/dsl/ir can
// import it. Literals drift, and both directions of drift are silent in
// production:
//
//   - a name the registry gained but the catalog lacks → the compiler rejects
//     a workflow that would have run;
//   - a name the registry lost (a claw rename, a dropped family) but the
//     catalog keeps → the check waves through exactly the failure it exists
//     to catch.
//
// So the truth is asserted from this side, where the registry is real: build
// it with every optional family switched ON (the catalog is a union — see
// toolcatalog.IsBuiltin) and require set equality over the built-in namespace.
func TestClawCatalogMatchesRegistry(t *testing.T) {
	reg := NewRegistry()
	planActive := false
	defaults := ClawDefaults{
		// Workspace stays empty: registration does not touch the disk, and
		// the tools' behaviour is not what this test is about.
		IncludeWebSearch:   true,
		IncludeComputerUse: true,
		PlanMode:           &clawtools.PlanModeState{Active: &planActive, Dir: t.TempDir()},
		Privacy: &privacy.Config{
			StoreDir:     t.TempDir(),
			Detector:     detector.New(),
			RunIDFromCtx: func(_ context.Context) string { return "" },
		},
	}
	if err := RegisterClawAll(reg, defaults); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}

	registered := map[string]bool{}
	for _, td := range reg.List() {
		// MCP-origin tools (the board and watch families register that way)
		// are resolved by the run time, never by the catalog.
		if td.Origin.Kind != OriginBuiltin {
			continue
		}
		registered[td.QualifiedName] = true
	}
	if len(registered) == 0 {
		t.Fatal("no built-in tools registered — the registry wiring changed shape")
	}

	catalog := map[string]bool{}
	for _, name := range toolcatalog.Builtins() {
		catalog[name] = true
	}

	var missing, extra []string
	for name := range registered {
		if !catalog[name] {
			missing = append(missing, name)
		}
	}
	for name := range catalog {
		if !registered[name] {
			extra = append(extra, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("registered but absent from pkg/backend/toolcatalog (C135 would reject a workflow that runs): %s\nadd them to clawBuiltins",
			strings.Join(sorted(missing), ", "))
	}
	if len(extra) > 0 {
		t.Errorf("in pkg/backend/toolcatalog but no longer registered (C135 would wave through a name that fails at dispatch): %s\nremove them from clawBuiltins",
			strings.Join(sorted(extra), ", "))
	}
}

// TestClawCatalogSuggestionsResolve keeps the "did you mean" advice honest: a
// suggestion naming a tool that does not exist sends the author from one
// unresolvable name to another.
func TestClawCatalogSuggestionsResolve(t *testing.T) {
	for _, legacy := range []string{
		"list_files", "search_codebase", "tree", "git_diff", "git_status",
		"git_log", "run_command", "patch", "edit_file", "apply_patch",
		"shell", "web_search_tool",
	} {
		got := toolcatalog.Suggest(legacy)
		if got == "" {
			t.Errorf("Suggest(%q) = \"\" — the legacy names are the ones authors actually type", legacy)
			continue
		}
		if !toolcatalog.IsBuiltin(got) {
			t.Errorf("Suggest(%q) = %q, which is not a registered built-in", legacy, got)
		}
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
