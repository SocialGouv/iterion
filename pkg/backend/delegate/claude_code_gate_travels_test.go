package delegate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// The gate is a PreToolUse hook, so it does not travel on argv — it travels
// in an `initialize` control request on the spawn's STDIN, and the verdict
// travels back on the same stream. These tests therefore read the wire of
// each spawn of one Execute: a config assertion would prove the intention,
// and the defect #1672 measured was precisely an intention that reached no
// process.
//
// What they pin is #1672.2: the structured-output spawn of a GATED node is
// bounded by the task's own policy now, not by a withholding that stood in
// for it. The withholding was an enumeration of a roster this project does
// not own, and it removed the node's own declared tools from the one pass
// that exists to finish interrupted work.

// spawnRecordedWithStdin is spawnRecorded plus, per spawn and in the same
// order, the control lines the CHILD read on its stdin. The stand-in also
// plays the CLI's half of one PreToolUse callback on the structured-output
// spawn, so the recording carries the gate's VERDICT and not only its
// registration: a hook nobody answers is not a gate, only a slower one.
func spawnRecordedWithStdin(t *testing.T, task Task) (spawnRecord, [][]string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("STDIN_DIR", dir)
	t.Setenv("HOOK_PROBE", "1")
	rec := spawnRecorded(t, task)
	stdin := make([][]string, len(rec.argv))
	for i := range rec.argv {
		body, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(i+1)))
		if err != nil {
			if !os.IsNotExist(err) {
				t.Fatalf("reading the stdin the stand-in recorded for spawn #%d: %v", i+1, err)
			}
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.TrimSpace(line) != "" {
				stdin[i] = append(stdin[i], line)
			}
		}
	}
	return rec, stdin
}

// initializeHooks reports the hook events a spawn registered, read from the
// `initialize` control request its stdin carries. A spawn that registered
// nothing returns nil — which is what an ungated spawn looks like from here.
func initializeHooks(t *testing.T, stdin []string) map[string][]struct {
	Matcher         *string  `json:"matcher"`
	HookCallbackIDs []string `json:"hookCallbackIds"`
} {
	t.Helper()
	if len(stdin) == 0 {
		return nil
	}
	var req struct {
		Type    string `json:"type"`
		Request struct {
			Subtype string `json:"subtype"`
			Hooks   map[string][]struct {
				Matcher         *string  `json:"matcher"`
				HookCallbackIDs []string `json:"hookCallbackIds"`
			} `json:"hooks"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(stdin[0]), &req); err != nil {
		return nil
	}
	if req.Type != "control_request" || req.Request.Subtype != "initialize" {
		return nil
	}
	return req.Request.Hooks
}

// hookVerdict is the permission decision a spawn wrote back for the probe
// callback, read from its stdin recording.
type hookVerdict struct {
	found     bool
	subtype   string
	decision  string
	reason    string
	stopped   bool // the verdict told the CLI to stop generating
	hasOutput bool
}

func readHookVerdict(t *testing.T, stdin []string) hookVerdict {
	t.Helper()
	for _, line := range stdin {
		var resp struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Response  struct {
					Continue           *bool `json:"continue"`
					HookSpecificOutput struct {
						PermissionDecision       string `json:"permissionDecision"`
						PermissionDecisionReason string `json:"permissionDecisionReason"`
					} `json:"hookSpecificOutput"`
				} `json:"response"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}
		if resp.Type != "control_response" || resp.Response.RequestID != "probe1" {
			continue
		}
		hso := resp.Response.Response.HookSpecificOutput
		return hookVerdict{
			found:     true,
			subtype:   resp.Response.Subtype,
			decision:  hso.PermissionDecision,
			reason:    hso.PermissionDecisionReason,
			stopped:   resp.Response.Response.Continue != nil && !*resp.Response.Response.Continue,
			hasOutput: hso.PermissionDecision != "" || hso.PermissionDecisionReason != "",
		}
	}
	return hookVerdict{}
}

// gatedSchemaTask is the shape #1672 measured: a node that declares tools,
// carries a scoped deny rule, and has an output schema — so the CLI is
// spawned twice on one session under the same always-on bypassPermissions.
func gatedSchemaTask(t *testing.T, mode permission.Mode, deny ...string) Task {
	t.Helper()
	pol, err := permission.NewPolicy(mode, []string{"Read(**)"}, nil, deny)
	if err != nil {
		t.Fatal(err)
	}
	return Task{
		NodeID:        "n",
		Permission:    pol,
		AllowedTools:  []string{"read_file", "bash"},
		ToolsDeclared: true,
		OutputSchema:  []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	}
}

// The witness of #1672.2. The structured-output spawn of a gated node must
// carry the task's gate on its stdin AND answer its callback with the
// policy's verdict. Before this change its stdin was 0 lines: it resumed the
// transcript the first pass's untrusted content is in, under
// bypassPermissions, with the gate absent — and the withholding that stood in
// for the gate could only say "no native tools at all", never
// `deny: ["Bash(rm -rf:*)"]`.
func TestTheGatedFormattingSpawnCarriesTheGateOnItsStdin(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	rec, stdin := spawnRecordedWithStdin(t, gatedSchemaTask(t, permission.ModeDeny, "Bash(rm -rf:*)"))
	if len(rec.argv) < 2 {
		t.Fatalf("expected the work pass and the structured-output pass, got %d spawn(s): %v", len(rec.argv), rec.argv)
	}
	if !strings.Contains(rec.argv[1], "--resume") || !strings.Contains(rec.argv[1], "--json-schema") {
		t.Fatalf("spawn #2 is not the structured-output pass — the probe below keys on --resume: %s", rec.argv[1])
	}

	// Registration: the gate reached the process.
	hooks := initializeHooks(t, stdin[1])
	if len(hooks["PreToolUse"]) != 1 {
		t.Fatalf("the structured-output spawn registered %d PreToolUse matcher(s), want 1 — its gate never reached the process (stdin: %v)",
			len(hooks["PreToolUse"]), stdin[1])
	}
	if m := hooks["PreToolUse"][0]; m.Matcher != nil {
		t.Errorf("the gate registered a tool matcher %q — it must bound EVERY tool, including the names no list in this package enumerates", *m.Matcher)
	}

	// Verdict: the POLICY answered, not merely a hook. This is the half that
	// separates a gate from a registration, and the half the withholding
	// could never express — the probe calls `rm -rf`, which only a SCOPED
	// rule refuses.
	v := readHookVerdict(t, stdin[1])
	if !v.found {
		t.Fatalf("the CLI asked the structured-output spawn for a verdict and got nothing back (stdin: %v)", stdin[1])
	}
	if v.subtype != "success" {
		t.Fatalf("the verdict came back as %q, not success — the callback resolved to no handler: %v", v.subtype, stdin[1])
	}
	if v.decision != "deny" {
		t.Errorf("the structured-output spawn answered %q for `rm -rf` under deny: [\"Bash(rm -rf:*)\"] — want deny (reason: %q)", v.decision, v.reason)
	}
	if !strings.Contains(v.reason, "Bash(rm -rf:*)") {
		t.Errorf("the refusal does not name the rule that refused; the scoped rule is exactly what the withholding could not express: %q", v.reason)
	}
	if v.stopped {
		t.Errorf("the structured-output spawn was told to stop generating — this pass still owes its JSON: %q", v.reason)
	}

	// …and the node keeps what it DECLARED on this spawn, which the
	// withholding took away: `tools:` bounds what EXISTS, the policy bounds
	// what RUNS. `bots/whats-next` is the shipped node that lost its
	// finishing pass to the join.
	if f := strings.Fields(rec.argv[1]); !slices.Contains(f, "--allowedTools") {
		t.Errorf("the gated structured-output spawn carries no declaration: %s", rec.argv[1])
	}
	if got := disallowed(rec.argv[1]); slices.Contains(got, "Read") || slices.Contains(got, "Bash") {
		t.Errorf("the gated structured-output spawn removed the tools the node declared (disallowed=%v)", got)
	}
	// The bounds that are NOT the gate still hold on this spawn: a bound
	// carried on one spawn and not the other is no bound at all.
	if got := disallowed(rec.argv[1]); !slices.Contains(got, "Workflow") {
		t.Errorf("the gated structured-output spawn keeps Workflow — the one tool the DATA in the resumed transcript can arm (disallowed=%v)", got)
	}
	if got := disallowed(rec.argv[1]); !slices.Contains(got, "Write") {
		t.Errorf("the gated structured-output spawn keeps a write surface the node never declared (disallowed=%v)", got)
	}

	// The work pass is gated through the SAME constructor, so it registers
	// the same matcher shape. Its own `ask:` behaviour (pause the run) is
	// covered where the pause is.
	if workHooks := initializeHooks(t, stdin[0]); len(workHooks["PreToolUse"]) == 0 {
		t.Errorf("the work pass registered no PreToolUse matcher at all (stdin: %v)", stdin[0])
	}
}

// `ask` is the mode the withholding hid: this pass runs AFTER the turn that
// could pause for an operator, and its caller returns a result rather than a
// pending question. So an approval cannot be queued here — the call is
// refused, with a reason that names why, and the pass is NOT stopped: it
// still owes its JSON.
//
// A deny-only fixture cannot see this arm, which is how a previous round left
// every `permission: ask` node's second spawn ungated.
func TestTheFormattingPassRefusesAnApprovalItCannotAsk(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	rec, stdin := spawnRecordedWithStdin(t, gatedSchemaTask(t, permission.ModeAsk))
	if len(rec.argv) < 2 {
		t.Fatalf("expected two spawns, got %d: %v", len(rec.argv), rec.argv)
	}
	v := readHookVerdict(t, stdin[1])
	if !v.found {
		t.Fatalf("an `ask` node's structured-output spawn answered no verdict at all (stdin: %v)", stdin[1])
	}
	if v.decision != "deny" {
		t.Errorf("an off-policy call on an `ask` node's structured-output spawn was answered %q — nobody can approve it here, so it is refused (reason: %q)", v.decision, v.reason)
	}
	if !strings.Contains(v.reason, "structured-output pass") {
		t.Errorf("the refusal does not say why approval is impossible here, so the model has nothing to adapt to: %q", v.reason)
	}
	if v.stopped {
		t.Errorf("the `ask` refusal stopped the structured-output pass — the run would lose the JSON it exists to produce: %q", v.reason)
	}
	// An ask verdict is NOT reported as a deny RULE: the author never wrote
	// one, and sending them looking for it is the wrong trail.
	if strings.Contains(v.reason, "deny rule") {
		t.Errorf("an `ask` verdict is reported as a deny rule: %q", v.reason)
	}
}

// An UNGATED structured-output spawn must not change: no hooks, so the prompt
// stays in argv and stdin stays closed. Otherwise every ungated schema'd node
// (54 of the catalog's 57 two-pass nodes) pays for a stdin protocol it never
// writes to.
func TestTheUngatedFormattingSpawnStaysUnhooked(t *testing.T) {
	rec, stdin := spawnRecordedWithStdin(t, Task{
		NodeID:        "n",
		AllowedTools:  []string{"read_file"},
		ToolsDeclared: true,
		OutputSchema:  []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	})
	if len(rec.argv) < 2 {
		t.Fatalf("expected two spawns, got %d: %v", len(rec.argv), rec.argv)
	}
	if hooks := initializeHooks(t, stdin[1]); len(hooks) != 0 {
		t.Errorf("an ungated structured-output spawn registered hooks %v", hooks)
	}
	if strings.Contains(rec.argv[1], "--input-format") {
		t.Errorf("an ungated structured-output spawn asked for a stdin protocol it never writes to: %s", rec.argv[1])
	}
	if v := readHookVerdict(t, stdin[1]); v.hasOutput {
		t.Errorf("an ungated structured-output spawn answered a permission verdict: %+v", v)
	}
}

// The chokepoint, asserted as a property of the SOURCE rather than of one
// spawn: claude_code reads the permission policy in exactly ONE place.
//
// This is the class the ticket names. Two spawns disagreed about what was
// gated for five rounds because the reading lived inline in the wiring of the
// spawn that needed it first; a third spawn added tomorrow is gated by
// calling claudePermissionGate and ungated by not calling it, and there is no
// shape where it carries a second, kinder copy of the policy.
func TestClaudeCodeReadsThePermissionPolicyInOnePlace(t *testing.T) {
	files, err := filepath.Glob("claude_code*.go")
	if err != nil {
		t.Fatal(err)
	}
	// The glob must find the backend, or this guard passes by finding nothing
	// — the classic green no-op.
	if len(files) < 5 {
		t.Fatalf("the guard found %d claude_code source file(s), so it is reading the wrong tree: %v", len(files), files)
	}
	evaluate := regexp.MustCompile(`\.Evaluate\(`)
	var sites []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if evaluate.MatchString(line) {
				sites = append(sites, f+":"+strconv.Itoa(i+1))
			}
		}
	}
	if len(sites) != 1 {
		t.Errorf("claude_code evaluates the permission policy at %d site(s), want exactly 1 (claudePermissionGate): %v\n"+
			"A second reading is how the two spawns of one task came to disagree about what was gated (#1672).", len(sites), sites)
	}
	if len(sites) == 1 && !strings.HasPrefix(sites[0], "claude_code_hooks.go:") {
		t.Errorf("the single policy reading moved to %s — it belongs in claudePermissionGate", sites[0])
	}
}
