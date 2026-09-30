package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"

	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/store"
)

// buildFullClawRegistry builds the registry a host wires for a run, with every
// optional family switched ON: RegisterClawAll plus the three families
// pkg/runview/executor.go registers next to it (board, watch, runs).
//
// It is the only honest source for "which tools can be advertised to a node":
// a literal list drifts on the next claw bump, which is the drift the tests
// below exist to catch.
func buildFullClawRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	planActive := false
	defaults := ClawDefaults{
		// Workspace stays empty: registration does not touch the disk, and
		// where the tools would run is not what these tests are about.
		IncludeWebSearch:            true,
		IncludeComputerUse:          true,
		IncludeWorkspaceDiagnostics: true,
		PlanMode:                    &clawtools.PlanModeState{Active: &planActive, Dir: t.TempDir()},
		Privacy: &privacy.Config{
			StoreDir:     t.TempDir(),
			Detector:     detector.New(),
			RunIDFromCtx: func(_ context.Context) string { return "" },
		},
	}
	// The comment above claims "every optional family switched ON". Prove
	// it rather than claim it: a new `Include…` flag nobody adds here takes
	// its family out of the exhaustiveness guard below, silently, and its
	// first execution-capable tool then reaches a sandboxed node with no
	// placement — refused with a reason that says nothing.
	assertEveryOptionalFamilyIsOn(t, defaults)
	if err := RegisterClawAll(reg, defaults); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}

	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("open board store: %v", err)
	}
	if err := RegisterClawBoardTools(reg, &BoardConfig{Store: boardStore, Capabilities: boardops.AllCapabilities()}); err != nil {
		t.Fatalf("RegisterClawBoardTools: %v", err)
	}
	if err := RegisterClawWatchTools(reg, &WatchConfig{
		Store:        placementWatchStore{},
		RunID:        "run-1",
		Capabilities: []string{"watch.subscribe", "watch.unsubscribe"},
	}); err != nil {
		t.Fatalf("RegisterClawWatchTools: %v", err)
	}
	runStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("open run store: %v", err)
	}
	if err := RegisterClawRunTools(reg, &RunConfig{Store: runStore, Capabilities: []string{runops.CapRunsRead}}); err != nil {
		t.Fatalf("RegisterClawRunTools: %v", err)
	}
	return reg
}

// TestSandboxPlacementCoversEveryRegisteredClawTool is the exhaustiveness
// guard behind the sandbox boundary.
//
// SandboxPlacementOf falls back to PlacementRefused for a name it does not
// know, which is the right direction (never proxy an unknown tool to the
// host) but a silent one: a claw bump that adds an execution-capable tool
// would start refusing every node that declares it, with a reason that says
// nothing. So the truth is asserted from the registry side — build the real
// thing, and require an EXPLICIT classification for every name in it.
func TestSandboxPlacementCoversEveryRegisteredClawTool(t *testing.T) {
	reg := buildFullClawRegistry(t)

	classified := map[string]bool{}
	for _, p := range []SandboxPlacement{PlacementSandbox, PlacementLauncher, PlacementRefused} {
		for _, name := range SandboxPlacedTools(p) {
			classified[name] = true
		}
	}

	var builtins, mcp int
	for _, td := range reg.List() {
		switch td.Origin.Kind {
		case OriginBuiltin:
			builtins++
			if !classified[td.QualifiedName] {
				t.Errorf("%q is registered but has no explicit sandbox placement — add it to sandboxPlacements, "+
					"deciding whether a sandboxed runner must execute it in-container, may proxy it, or must refuse it",
					td.QualifiedName)
			}
		case OriginMCP:
			mcp++
			// The model sees the sanitized spelling; that is the name the
			// runner classifies.
			sanitized := strings.ReplaceAll(td.QualifiedName, ".", "_")
			for _, spelling := range []string{td.QualifiedName, sanitized} {
				if placement, _ := SandboxPlacementOf(spelling); placement != PlacementLauncher {
					t.Errorf("MCP tool %q classified %s — the servers are connected launcher-side, so it must proxy", spelling, placement)
				}
			}
		}
	}
	if builtins == 0 || mcp == 0 {
		t.Fatalf("the registry wiring changed shape: %d built-ins, %d MCP tools", builtins, mcp)
	}

	// And the other direction: a placement for a name nothing registers is a
	// rule that will never fire, and an entry someone will trust.
	registered := map[string]bool{}
	for _, td := range reg.List() {
		registered[td.QualifiedName] = true
	}
	for name := range classified {
		if !registered[name] {
			t.Errorf("%q has a sandbox placement but no registrar puts it in the registry any more — remove it", name)
		}
	}
}

// TestSandboxPlacementFailsClosed pins the two properties the routing rests
// on: the zero value is the refusal, and an unknown non-MCP name is refused
// rather than proxied.
func TestSandboxPlacementFailsClosed(t *testing.T) {
	var zero SandboxPlacement
	if zero != PlacementRefused {
		t.Errorf("the zero SandboxPlacement is %s; a forgotten assignment must refuse, never proxy to the host", zero)
	}
	for _, name := range []string{"", "   ", " bash ", "some_future_claw_tool", "Bash", "mcpsomething"} {
		if placement, reason := SandboxPlacementOf(name); placement != PlacementRefused {
			t.Errorf("SandboxPlacementOf(%q) = %s (%s); an unclassified name must be refused", name, placement, reason)
		}
	}
	// A name that merely STARTS like the MCP family is not an MCP tool: the
	// server or the tool segment is missing, so it must not inherit the
	// launcher placement a real MCP tool gets.
	for _, name := range []string{
		"mcp_", "mcp.", "mcp__", "mcp_bash", "mcp_connect", "mcp.x", "mcp.x.", "mcp..x", "mcp__x",
		// A separator at the very start or end of the remainder leaves an
		// empty server or tool: only an INTERIOR one splits it.
		"mcp__bash", "mcp_bash_", "mcp..xy", "mcp.xy.",
	} {
		if placement, reason := SandboxPlacementOf(name); placement != PlacementRefused {
			t.Errorf("SandboxPlacementOf(%q) = %s (%s); a truncated MCP-looking name must be refused", name, placement, reason)
		}
	}
}

// TestSandboxPlacementRecognisesEveryRegisteredMCPName registers MCP tools
// through the real RegisterMCP for server names the config loaders accept —
// `.mcp.json` keys are free-form and the DSL lexer takes a leading `_` — and
// classifies the names the launcher actually advertises (ToDelegateDef) and
// qualifies. Every one is an MCP tool: refusing one would refuse every
// sandboxed node that server is ambient to.
func TestSandboxPlacementRecognisesEveryRegisteredMCPName(t *testing.T) {
	for _, tc := range []struct{ server, tool string }{
		{"github", "create_issue"},
		{"iterion_board", "create_issue"},
		{"firecrawl", "firecrawl_scrape"},
		{"my-server", "do-thing"},
		{"srv_", "x"},
		{"a__b", "c"},
		{"srv", "_private"},
		{"_scratch", "x"},
		{"_", "tool"},
		{"__x", "y"},
	} {
		reg := NewRegistry()
		if err := reg.RegisterMCP(tc.server, tc.tool, "", nil, func(context.Context, json.RawMessage) (string, error) {
			return "", nil
		}); err != nil {
			t.Fatalf("RegisterMCP(%q, %q): %v", tc.server, tc.tool, err)
		}
		def, err := reg.Resolve("mcp." + tc.server + "." + tc.tool)
		if err != nil {
			t.Fatalf("Resolve(mcp.%s.%s): %v", tc.server, tc.tool, err)
		}
		for _, name := range []string{def.QualifiedName, def.ToDelegateDef().Name} {
			if placement, reason := SandboxPlacementOf(name); placement != PlacementLauncher {
				t.Errorf("server %q tool %q: SandboxPlacementOf(%q) = %s (%s); a registered MCP tool is served launcher-side",
					tc.server, tc.tool, name, placement, reason)
			}
		}
	}
}

// TestSandboxPlacementRecognisesEveryMCPSpelling covers the three forms one
// MCP tool travels under: the registry's dotted qualified name, the sanitized
// name the model is given, and the claude_code FQN a prompt may carry.
// Missing one would refuse a board call on a sandboxed node.
func TestSandboxPlacementRecognisesEveryMCPSpelling(t *testing.T) {
	for _, name := range []string{
		"mcp.iterion_board.create_issue",
		"mcp_iterion_board_create_issue",
		"mcp__iterion_board__create_issue",
		"mcp.github.create_issue",
		"mcp_github_create_issue",
		"mcp___scratch__x",
	} {
		if placement, reason := SandboxPlacementOf(name); placement != PlacementLauncher {
			t.Errorf("SandboxPlacementOf(%q) = %s (%s); MCP tools are served launcher-side", name, placement, reason)
		}
	}
	// `mcp_auth` is a built-in whose name starts with the MCP prefix; its
	// explicit entry is what classifies it.
	if placement, _ := SandboxPlacementOf("mcp_auth"); placement != PlacementLauncher {
		t.Errorf("mcp_auth must resolve through the explicit table, got %s", placement)
	}
}

// TestSandboxPlacementExactTableWinsOverTheMCPShape pins the precedence: a
// built-in whose name has the full MCP shape (`mcp_<a>_<b>`) is classified by
// its table entry, never by the shape rule — or a claw built-in named that
// way would be routed to the launcher whatever its entry says.
func TestSandboxPlacementExactTableWinsOverTheMCPShape(t *testing.T) {
	const name = "mcp_probe_builtin"
	if !isMCPFamilyName(name) {
		t.Fatalf("%q must have the MCP shape for this test to mean anything", name)
	}
	sandboxPlacements[name] = placementRule{PlacementSandbox, "probe entry"}
	t.Cleanup(func() { delete(sandboxPlacements, name) })

	if placement, reason := SandboxPlacementOf(name); placement != PlacementSandbox {
		t.Errorf("SandboxPlacementOf(%q) = %s (%s); the explicit table entry must win over the MCP shape", name, placement, reason)
	}
}

// TestSandboxPlacementReasonsAreUsable keeps the refusal messages worth
// printing: every reason is a non-empty clause that reads after "which a
// sandboxed runner cannot execute in-container (…)".
func TestSandboxPlacementReasonsAreUsable(t *testing.T) {
	for _, p := range []SandboxPlacement{PlacementSandbox, PlacementLauncher, PlacementRefused} {
		for _, name := range SandboxPlacedTools(p) {
			_, reason := SandboxPlacementOf(name)
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%q (%s) has no reason; the operator's error would say nothing", name, p)
			}
		}
	}
}

// placementWatchStore satisfies WatchStore without a run store — these tests
// only care that the watch tools REGISTER under their names.
type placementWatchStore struct{}

func (placementWatchStore) AddWatchedIssues(_ context.Context, _ string, ids []string) ([]string, error) {
	return ids, nil
}

func (placementWatchStore) RemoveWatchedIssues(_ context.Context, _ string, _ []string) ([]string, error) {
	return nil, nil
}

// assertEveryOptionalFamilyIsOn fails if any bool switch on ClawDefaults is
// left false, or any registry/provider field left nil. Reflection, so a field
// added to the struct is covered without touching this test.
func assertEveryOptionalFamilyIsOn(t *testing.T, defaults ClawDefaults) {
	t.Helper()
	v := reflect.ValueOf(defaults)
	ty := v.Type()
	// Fields a host legitimately leaves unset for THIS harness: they change
	// how a tool behaves, not whether its family registers.
	allowedUnset := map[string]string{
		"Workspace":    "registration does not touch the disk; where tools would run is not what these tests are about",
		"BashExtraEnv": "extra env for bash, not a family switch",
		"Subagent":     "nil keeps claw's metadata-only agent tool, which IS the registered form",
		"AskUser":      "nil keeps the pause/resume handler, which IS the registered form",
		"Config":       "an empty config map still registers the `config` tool",
		// RegisterClawAll allocates a fresh empty registry for each of
		// these when nil, so the family registers either way. Their doc
		// comment says so; if that ever changes, the family stops
		// registering and this list is the line to revisit.
		"Tasks":       "RegisterClawAll allocates one when nil",
		"Workers":     "RegisterClawAll allocates one when nil",
		"Teams":       "RegisterClawAll allocates one when nil",
		"Crons":       "RegisterClawAll allocates one when nil",
		"LSP":         "RegisterClawAll allocates one when nil",
		"MCPProvider": "RegisterClawAll wires an empty Registry-backed provider when nil",
	}
	for i := 0; i < ty.NumField(); i++ {
		f, name := v.Field(i), ty.Field(i).Name
		if _, ok := allowedUnset[name]; ok {
			continue
		}
		switch f.Kind() {
		case reflect.Bool:
			if !f.Bool() {
				t.Fatalf("ClawDefaults.%s is false, so its family is not registered and "+
					"TestSandboxPlacementCoversEveryRegisteredClawTool never sees its tools — set it here, or "+
					"add it to allowedUnset with a reason", name)
			}
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
			if f.IsNil() {
				t.Fatalf("ClawDefaults.%s is nil, so its family may not register — wire it here, or add it to "+
					"allowedUnset with a reason", name)
			}
		}
	}
}
