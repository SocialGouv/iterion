package tool

import (
	"context"
	"strings"
	"testing"

	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"

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

// TestInternalMCPShorthandsMatchRegistry guards the second half of the
// catalog: the bare names Registry.Resolve reaches through its shorthand path
// (a dot-free reference matched as a unique suffix over the registered MCP
// tools). iterion registers the board and watch families itself for every run,
// so `tools: [create_issue]` on a claw node resolves — and C135 must not
// reject it. If a board op is added or renamed and the catalog is not updated,
// the compiler starts refusing a workflow that runs; this fails first.
func TestInternalMCPShorthandsMatchRegistry(t *testing.T) {
	reg := NewRegistry()
	store, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("open board store: %v", err)
	}
	if err := RegisterClawBoardTools(reg, &BoardConfig{Store: store, Capabilities: boardops.AllCapabilities()}); err != nil {
		t.Fatalf("RegisterClawBoardTools: %v", err)
	}
	if err := RegisterClawWatchTools(reg, &WatchConfig{
		Store:        conformanceWatchStore{},
		RunID:        "run-1",
		Capabilities: []string{"watch.subscribe", "watch.unsubscribe"},
	}); err != nil {
		t.Fatalf("RegisterClawWatchTools: %v", err)
	}

	var registered []string
	for _, td := range reg.List() {
		if td.Origin.Kind != OriginMCP {
			continue
		}
		// The shorthand key is the segment after the last dot.
		idx := strings.LastIndex(td.QualifiedName, ".")
		if idx < 0 {
			continue
		}
		registered = append(registered, td.QualifiedName[idx+1:])
	}
	if len(registered) == 0 {
		t.Fatal("no internal MCP tools registered — the wiring changed shape")
	}
	for _, bare := range registered {
		if !toolcatalog.ResolvesViaShorthand(bare) {
			t.Errorf("%q resolves through Registry.Resolve's shorthand path but the catalog rejects it — C135 would refuse a workflow that runs; add it to internalMCPShorthands", bare)
		}
	}
	// And nothing stale in the other direction: a name the catalog accepts
	// but nothing registers is a name C135 waves through to a dispatch-time
	// failure.
	live := map[string]bool{}
	for _, bare := range registered {
		live[bare] = true
	}
	for _, bare := range []string{
		"add_labels", "assign_issue", "close_issue", "comment_issue", "create_issue",
		"get_issue", "list_issues", "list_labels", "remove_labels", "set_bot", "set_labels",
		"transition_issue", "subscribe", "unsubscribe",
	} {
		if !live[bare] {
			t.Errorf("the catalog accepts %q but no internal MCP server registers it any more", bare)
		}
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

// conformanceWatchStore satisfies WatchStore without touching a run store —
// this test only cares that the tools REGISTER under their names.
type conformanceWatchStore struct{}

func (conformanceWatchStore) AddWatchedIssues(_ context.Context, _ string, ids []string) ([]string, error) {
	return ids, nil
}

func (conformanceWatchStore) RemoveWatchedIssues(_ context.Context, _ string, _ []string) ([]string, error) {
	return nil, nil
}
