package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// ---------------------------------------------------------------------------
// Sandbox boundary probe (#1945)
// ---------------------------------------------------------------------------
//
// These tests answer ONE question about [makeHybridToolDefs]: for a tool the
// launcher advertised to a sandboxed claw node, does the runner keep execution
// INSIDE the container, or does it hand the call back to the launcher — which
// runs the tool on the HOST (pkg/backend/model/claw_backend.go, OnToolCall →
// td.Execute)?
//
// Nothing is ever executed here. The launcher half is a recording stub: it
// reads the tool_call envelope, notes the name, and answers with a marker. A
// tool that reaches the stub is a tool that crossed the sandbox boundary.
// Every probe input is additionally one the tool's own implementation rejects
// on its first line (a missing required field), so even a tool that a future
// fix registers LOCALLY cannot run a process or touch a file from this test.

// boundaryProbeMarker is what the recording stub answers with. Seeing it in a
// tool's output means the call left the container.
const boundaryProbeMarker = "__recorded-by-stub-launcher__"

// probeInputs are inputs each named tool's implementation refuses before any
// side effect — verified against the implementations:
//   - bash / diagnostic_shell: "'command' input is required"
//     (vendor/.../internal/tools/bash.go, ExecuteBashWithEnv, first statement)
//   - repl: "'code' is required" (vendor/.../internal/tools/repl.go:30)
//   - notebook_edit: "'notebook_path' is required"
//     (vendor/.../internal/tools/notebook_edit.go:31, before its os.ReadFile)
//   - sleep: "'duration_ms' is required" (vendor/.../internal/tools/sleep.go:26)
//   - structured_output: "payload must not be empty"
//     (vendor/.../internal/tools/structured_output.go:30)
//   - send_user_message: "'message' is required"
//     (vendor/.../internal/tools/send_message.go:38, before any os.Stat)
//   - remote_trigger: "'url' is required"
//     (vendor/.../internal/tools/remote_trigger.go:87, before any request)
//   - read_file / workspace_grep / glob / write_file / file_edit / skill /
//     read_image / lsp: a required field is missing, so each returns its
//     validation error without opening anything.
//
// An empty object satisfies all of them, which is why every probe uses it.
var probeInput = json.RawMessage(`{}`)

// stubLauncher is the host half of the IPC, with the host removed: it records
// the tools the runner forwarded and never executes one.
type stubLauncher struct {
	mu        sync.Mutex
	forwarded map[string]int
}

func (s *stubLauncher) sawCall(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forwarded[name] > 0
}

// everythingForwarded returns every name the launcher was asked to run.
//
// sawCall answers a question keyed on the name the RUNNER chose to forward
// under, so a call that crossed the boundary spelled differently — an FQN
// rewrite, a wrapper prefix, a rename — reads there as "no escape". The
// escape witness asserts on this instead: nothing at all may cross, whatever
// it is called.
func (s *stubLauncher) everythingForwarded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.forwarded))
	for name := range s.forwarded {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// newStubLauncher wires a runner-side dispatcher to a recording stub launcher
// and returns both. Nothing the stub receives is executed.
func newStubLauncher(t *testing.T) (*proxyDispatcher, *stubLauncher) {
	t.Helper()

	runnerStdinR, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()

	dispatcher := newProxyDispatcher(runnerStdinR, runnerStdoutW)
	dispatcher.start()

	stub := &stubLauncher{forwarded: map[string]int{}}
	launcherDone := make(chan struct{})
	go func() {
		defer close(launcherDone)
		reader := delegate.NewEnvelopeReader(runnerStdoutR)
		writer := delegate.NewEnvelopeWriter(runnerStdinW)
		for {
			env, err := reader.Read()
			if err != nil {
				return
			}
			if env.Type != delegate.EnvelopeToolCall {
				continue
			}
			var call delegate.ToolCallData
			if err := json.Unmarshal(env.Data, &call); err != nil {
				return
			}
			stub.mu.Lock()
			stub.forwarded[call.Name]++
			stub.mu.Unlock()
			// Recorded, NOT executed. The real launcher would call
			// td.Execute here — on the host, in the host workspace.
			reply, err := delegate.NewToolResultEnvelope(env.ID, boundaryProbeMarker, "")
			if err != nil {
				return
			}
			if err := writer.Write(reply); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = runnerStdoutW.Close()
		_ = runnerStdinW.Close()
		<-launcherDone
	})
	return dispatcher, stub
}

// advertise builds the IOToolDefs a launcher would send for the given names.
func advertise(names []string) []delegate.IOToolDef {
	defs := make([]delegate.IOToolDef, 0, len(names))
	for _, name := range names {
		defs = append(defs, delegate.IOToolDef{
			Name:        name,
			Description: "advertised by the launcher",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}
	return defs
}

// newBoundaryProbe wires a runner-side dispatcher to a recording stub
// launcher, builds the hybrid tool defs for the advertised names through the
// REAL [makeHybridToolDefs], and returns them keyed by name.
//
// workspace is a t.TempDir(): it stands in for the container bind-mount the
// runner registers its local builtins against. Nothing is written to it.
func newBoundaryProbe(t *testing.T, advertised []string) (map[string]delegate.ToolDef, *stubLauncher) {
	t.Helper()

	dispatcher, stub := newStubLauncher(t)

	hybrid, err := makeHybridToolDefs(advertise(advertised), dispatcher, t.TempDir())
	if err != nil {
		t.Fatalf("makeHybridToolDefs refused the advertised set, so the probe cannot tell the paths apart: %v", err)
	}
	if len(hybrid) != len(advertised) {
		t.Fatalf("makeHybridToolDefs returned %d defs for %d advertised tools", len(hybrid), len(advertised))
	}
	byName := make(map[string]delegate.ToolDef, len(hybrid))
	for _, td := range hybrid {
		byName[td.Name] = td
	}
	return byName, stub
}

// crossesSandboxBoundary invokes a hybrid def with an input its
// implementation refuses on the first line, and reports whether the call was
// forwarded to the launcher.
func crossesSandboxBoundary(t *testing.T, defs map[string]delegate.ToolDef, stub *stubLauncher, name string) bool {
	t.Helper()
	td, ok := defs[name]
	if !ok {
		t.Fatalf("tool %q missing from the hybrid defs", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = td.Execute(ctx, probeInput)
	return stub.sawCall(name)
}

// TestHybridToolDefs_ExecutionCapableToolsNeverLeaveTheSandbox is named for
// what a fix for #1945 must make true: a tool that can start a process or
// write a file must never be handed back to the launcher, because the
// launcher runs it on the HOST — outside the container the operator asked for.
//
// The safe behaviour this asserts can be reached two ways, both acceptable:
// register the tool locally in the runner, or refuse it at task-build time for
// a sandboxed node. What must NOT remain is the silent host round-trip.
func TestHybridToolDefs_ExecutionCapableToolsNeverLeaveTheSandbox(t *testing.T) {
	// What each tool does once the launcher executes it, host-side:
	escapes := map[string]string{
		// pkg/backend/tool/claw_builtins.go:114 → executeWorkspaceBash →
		// bash -c "cd -- <LAUNCHER workspace> && <model command>".
		"diagnostic_shell": "spawns bash in the launcher's workspace",
		// vendor/.../internal/tools/repl.go:56 — exec.CommandContext with no
		// Dir, so the launcher process's cwd, and no command validation at all.
		"repl": "spawns python3/node/bash in the launcher process's cwd",
		// vendor/.../internal/tools/notebook_edit.go:36,191 — os.ReadFile /
		// os.WriteFile on the raw model-supplied path, uncontained.
		"notebook_edit": "reads and writes an arbitrary host path",
	}

	// bash is the control: the comment in makeHybridToolDefs records that it
	// HAD to be made local after it executed on the host. It must stay local.
	const control = "bash"

	advertised := []string{control}
	for name := range escapes {
		advertised = append(advertised, name)
	}
	sort.Strings(advertised)

	defs, stub := newBoundaryProbe(t, advertised)

	if crossesSandboxBoundary(t, defs, stub, control) {
		t.Fatalf("control tool %q was proxied to the launcher — the probe cannot distinguish the two paths", control)
	}

	for _, name := range advertised {
		if name == control {
			continue
		}
		if crossesSandboxBoundary(t, defs, stub, name) {
			t.Errorf("sandbox escape: %q was forwarded to the launcher, which %s; "+
				"an execution-capable tool advertised to a sandboxed claw node must run inside the container "+
				"or be refused at task-build time", name, escapes[name])
		}
	}

	// And the same question asked without a name. Every tool probed above is
	// contracted to stay in the container, so the launcher must have been
	// asked to run NOTHING — a call that crossed under another spelling is
	// invisible to the per-name check and is exactly the shape a refactor
	// produces.
	if crossed := stub.everythingForwarded(); len(crossed) > 0 {
		t.Errorf("sandbox escape: the launcher was asked to run %v; none of the probed tools may cross, "+
			"under any name", crossed)
	}
}

// TestHybridToolDefs_RoutesEveryAdvertisedToolByItsPlacement pins the split
// the runner is contracted to produce, in both directions: the tools that
// must stay in the container, and the ones only the launcher can serve.
//
// It was a characterization test while #1945 was open (it recorded the defect
// so the routing could not drift in silence); it now states the requirement.
func TestHybridToolDefs_RoutesEveryAdvertisedToolByItsPlacement(t *testing.T) {
	local := tool.SandboxPlacedTools(tool.PlacementSandbox)
	if len(local) == 0 {
		t.Fatal("no tool is classified PlacementSandbox — the classification lost its shape")
	}
	// Legitimately launcher-side: the runner has no MCP servers, no operator
	// to answer a question, and none of the launcher's in-memory registries.
	proxiedByDesign := []string{
		"ask_user",
		"ask_user_async",
		"await_answers",
		"mcp_iterion_board_create_issue",
		"mcp.github.create_issue",
		"mcp__iterion_board__create_issue",
		"todo_write",
		"task_create",
		"team_list",
		"cron_list",
		"config",
		"tool_search",
		"list_mcp_resources",
		"mcp_auth",
		"web_search",
	}

	advertised := append(append([]string{}, local...), proxiedByDesign...)
	defs, stub := newBoundaryProbe(t, advertised)

	for _, name := range local {
		if crossesSandboxBoundary(t, defs, stub, name) {
			t.Errorf("%q crossed the sandbox boundary; it is classified PlacementSandbox, so the runner must execute it in-container", name)
		}
	}
	for _, name := range proxiedByDesign {
		if !crossesSandboxBoundary(t, defs, stub, name) {
			t.Errorf("%q resolved locally; only the launcher can serve it", name)
		}
	}
}

// TestHybridToolDefs_RefusesWhatTheContainerCannotExecute covers the third
// branch: a tool with no in-container form, and a name nothing classifies.
// Both must fail the runner, because the alternative is the IPC proxy — i.e.
// execution on the host, which is exactly what the sandbox was asked to stop.
func TestHybridToolDefs_RefusesWhatTheContainerCannotExecute(t *testing.T) {
	dispatcher, _ := newStubLauncher(t)

	for _, tc := range []struct {
		name string
		want string
	}{
		{"lsp", "child processes of the launcher"},
		{"screenshot", "display"},
		{"worker_create", "working directory the model supplies"},
		// Fail closed: a name from a claw bump nobody classified yet is
		// refused, never proxied.
		{"some_future_claw_tool", "no sandbox placement is declared"},
	} {
		_, err := makeHybridToolDefs(advertise([]string{tc.name}), dispatcher, t.TempDir())
		if err == nil {
			t.Errorf("%q was accepted by the runner; it has no in-container form, so accepting it means proxying it to the host", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: error does not say why it cannot run in-container: %v", tc.name, err)
		}
	}
}

// TestHybridToolDefs_SandboxToolMissingLocallyIsAnError is the guard on the
// half of the routing that cannot be observed from the outside: when a tool
// the classification says MUST run in-container has no local registration,
// the only other option is the launcher — so the runner must refuse instead.
func TestHybridToolDefs_SandboxToolMissingLocallyIsAnError(t *testing.T) {
	dispatcher, stub := newStubLauncher(t)

	// A registrar that registers nothing: every PlacementSandbox tool is now
	// missing from the local registry.
	noLocalTools := func(*tool.Registry, string) error { return nil }

	for _, name := range tool.SandboxPlacedTools(tool.PlacementSandbox) {
		_, err := buildHybridToolDefs(advertise([]string{name}), dispatcher, t.TempDir(), noLocalTools)
		if err == nil {
			t.Errorf("%q with no local registration was accepted — the runner would have proxied it to the host", name)
			continue
		}
		if !strings.Contains(err.Error(), "must execute inside the sandbox") {
			t.Errorf("%q: unexpected error: %v", name, err)
		}
	}
	if stub.sawCall("bash") {
		t.Error("a refused build still forwarded a call to the launcher")
	}
}

// TestHybridToolDefs_LocalRegistrationFailureIsFatal is the witness on the
// fallback that used to live here: a failure to register the in-container set
// printed a warning and proxied EVERYTHING to the launcher — one slip turning
// into a total escape, silently. It must be a hard error.
func TestHybridToolDefs_LocalRegistrationFailureIsFatal(t *testing.T) {
	dispatcher, stub := newStubLauncher(t)

	failing := func(*tool.Registry, string) error { return errors.New("registrar is unavailable in this build") }

	defs, err := buildHybridToolDefs(advertise([]string{"bash", "read_file", "ask_user"}), dispatcher, t.TempDir(), failing)
	if err == nil {
		t.Fatalf("a failed local registration produced %d tool defs instead of an error — "+
			"every one of them would execute on the launcher's host", len(defs))
	}
	if !strings.Contains(err.Error(), "register the in-container tool set") {
		t.Errorf("error does not name what failed: %v", err)
	}
	if stub.sawCall("bash") {
		t.Error("a failed local registration still forwarded a call to the launcher")
	}
}

// TestRegisterSandboxLocalTools_CoversTheWholeSandboxSet is the join between
// the classification and the runner's registrars: every name classified
// PlacementSandbox must actually be registrable in-container, or the runner
// refuses a node it should have run.
func TestRegisterSandboxLocalTools_CoversTheWholeSandboxSet(t *testing.T) {
	reg := tool.NewRegistry()
	if err := registerSandboxLocalTools(reg, t.TempDir()); err != nil {
		t.Fatalf("registerSandboxLocalTools: %v", err)
	}

	registered := map[string]bool{}
	for _, td := range reg.List() {
		if td.Origin.Kind == tool.OriginBuiltin {
			registered[td.QualifiedName] = true
		}
	}
	for _, name := range tool.SandboxPlacedTools(tool.PlacementSandbox) {
		if !registered[name] {
			t.Errorf("%q is classified PlacementSandbox but the runner registers no local implementation for it — "+
				"every sandboxed node declaring it would fail", name)
		}
		delete(registered, name)
	}
	for name := range registered {
		placement, _ := tool.SandboxPlacementOf(name)
		t.Errorf("the runner registers %q locally but its placement is %s — a tool executed in-container "+
			"while the launcher believes it owns it is the mirror of the escape this routing exists to stop", name, placement)
	}
}
