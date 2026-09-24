package delegate

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// fakeClaudeArgv is a stand-in CLI that records every argv it is spawned with
// and answers the minimum stream-json the SDK needs to finish a turn. Each
// spawn appends one newline-terminated record to $ARGV_LOG (newlines inside
// an option — an ultracode --append-system-prompt runs to 13 lines — are
// flattened first, or one spawn would be counted as fourteen), so a test can assert on
// BOTH passes of one Execute.
//
// It answers only once it has read the user message. The SDK writes its
// initialize control request first whenever the session carries a hook, and
// every claude_code first pass does: a stand-in answering after one line
// exits while the user message is still on its way, that write fails with
// EPIPE, and Execute reports a transient failure before the formatting pass
// (#1694).
const fakeClaudeArgv = `#!/bin/sh
printf '%s' "$*" | tr '\n' ' ' >> "$ARGV_LOG"; printf '\n' >> "$ARGV_LOG"
case "$*" in *--input-format*)
	while read -r line; do
		case "$line" in *'"type":"user"'*) break ;; esac
	done ;;
esac
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"s1"}'
`

// spawnArgv runs the REAL ClaudeCodeBackend.Execute against the stand-in CLI
// and returns one string per spawn.
func spawnArgv(t *testing.T, task Task) []string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte(fakeClaudeArgv), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "argv.log")
	t.Setenv("ARGV_LOG", log)

	task.Command = script
	task.WorkDir = dir
	if task.UserPrompt == "" {
		task.UserPrompt = "x"
	}
	// Execute's verdict is not asserted — the stand-in answers an empty
	// result, which no schema accepts — but it is what explains a spawn count:
	// the formatting pass is a fallback Execute decides on, and a failed first
	// pass returns before it. Logged here, the error and the backend's own
	// lines reach the output of any assertion below that fails.
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
	var mu sync.Mutex
	var lines []string
	b.Logger.SetHook(func(level iterlog.Level, msg string, _ map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, level.String()+": "+msg)
	})
	res, execErr := b.Execute(context.Background(), task)
	mu.Lock()
	t.Logf("Execute: err=%v, formatting pass used=%v; backend log:\n%s",
		execErr, res.FormattingPassUsed, strings.Join(lines, "\n"))
	mu.Unlock()

	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the stand-in CLI was never spawned: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// disallowed parses the FLAG's value rather than substring-matching the
// joined argv: `strings.Contains(argv, "Read")` is satisfied by any argv
// holding `--allowedTools read_file`, and `"--disallowedTools Read"` can
// never match because `Bash` sorts before it in the roster — an assertion
// that cannot fire is worse than none.
func disallowed(argv string) []string {
	f := strings.Fields(argv)
	for i, a := range f {
		if a == "--disallowedTools" && i+1 < len(f) {
			return strings.Split(f[i+1], ",")
		}
	}
	return nil
}

// The boundary asserted where the product puts it: on the command line of
// EVERY spawn Execute makes.
//
// Two measured reasons this is not the helper's own test. Deleting the one
// line that calls claudeToolOptions from Execute restored the #1615 bug (a
// `tools: []` node keeping all fourteen natives) with the whole Go test set
// green — a mutation that reddens inside a helper and not at its call site
// proves the helper, not the product. And a structured-output node spawns the
// CLI TWICE on one session under the same always-on bypassPermissions; the
// second pass carried no tool flags at all, so the boundary held on one spawn
// and not the other, which is no boundary.
func TestEverySpawnOfATaskCarriesTheNodesToolBoundary(t *testing.T) {
	roster := []string{
		"Bash", "Read", "Glob", "Grep", "Write", "Edit", "MultiEdit",
		"NotebookEdit", "Task", "WebFetch", "WebSearch", "ToolSearch",
		"TodoWrite", "Skill",
	}

	// A declared-empty list, with an output schema so the formatting pass
	// runs: BOTH spawns must remove the whole roster.
	spawns := spawnArgv(t, Task{
		NodeID:        "n",
		AllowedTools:  []string{},
		ToolsDeclared: true,
		OutputSchema:  []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	})
	if len(spawns) < 2 {
		t.Fatalf("expected the main pass and the formatting pass, got %d spawn(s): %v", len(spawns), spawns)
	}
	for i, argv := range spawns {
		got := disallowed(argv)
		for _, native := range roster {
			if !slices.Contains(got, native) {
				t.Errorf("spawn #%d runs a `tools: []` node without removing %q (disallowed=%v)", i+1, native, got)
			}
		}
	}

	// The diagnostic_shell opt-in is the one input that changes the answer,
	// and it reaches the CLI through this same chokepoint: Bash must survive.
	optIn := spawnArgv(t, Task{
		NodeID:          "n",
		AllowedTools:    []string{},
		ToolsDeclared:   true,
		DiagnosticShell: true,
		OutputSchema:    []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	})
	if len(optIn) < 2 {
		t.Fatalf("expected two spawns for the diagnostic_shell row, got %d", len(optIn))
	}
	for i, argv := range optIn {
		if got := disallowed(argv); slices.Contains(got, "Bash") {
			t.Errorf("spawn #%d: the diagnostic_shell opt-in was revoked on the way to the CLI (disallowed=%v)", i+1, got)
		}
	}

	// An UNDECLARED list keeps the legacy unrestricted surface: only
	// `Workflow` is withheld, and by a different rule (ultracode), not by
	// the declaration — on EVERY spawn. This is the row that carries a
	// schema on purpose: `Workflow` is the one tool the CLI arms on the word
	// "ultracode" appearing anywhere in the prompt, and the formatting pass
	// resumes the transcript the node's own content is in, so a spawn that
	// drops the withholding lets the DATA grant an orchestration the
	// operator's effort never did.
	undeclared := spawnArgv(t, Task{
		NodeID:       "n",
		OutputSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	})
	if len(undeclared) < 2 {
		t.Fatalf("expected two spawns for the undeclared row, got %d", len(undeclared))
	}
	for i, argv := range undeclared {
		if got := disallowed(argv); !slices.Equal(got, []string{"Workflow"}) {
			t.Errorf("spawn #%d of an undeclared list disallowed %v, want only the ultracode-withheld Workflow", i+1, got)
		}
	}

	// The bounds that are NOT the declaration follow the task too, and each
	// one was found missing on spawn #2 by the round that asked for the
	// complete enumeration rather than for a verdict on the last fix.
	for i, argv := range undeclared {
		if !slices.Contains(strings.Fields(argv), "--strict-mcp-config") {
			t.Errorf("spawn #%d lost --strict-mcp-config: the operator's personal ~/.claude.json servers boot inside this bot node (#506)", i+1)
		}
	}
	// The multi-line SystemPrompt is deliberate: it makes an OPTION carry
	// newlines, which is what the stand-in has to flatten — without that,
	// one spawn is recorded as five and the spawn-count guards read a
	// harness artefact as a second pass.
	capped := spawnArgv(t, Task{
		NodeID:       "n",
		SystemPrompt: "line one\nline two\nline three\nline four",
		ToolMaxSteps: 25,
		OutputSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	})
	if len(capped) < 2 {
		t.Fatalf("expected two spawns for the capped row, got %d", len(capped))
	}
	for i, argv := range capped {
		f := strings.Fields(argv)
		j := slices.Index(f, "--max-turns")
		if j < 0 || j+1 >= len(f) || f[j+1] != "25" {
			t.Errorf("spawn #%d runs uncapped although the node set tool_max_steps: 25 — a cap carried on one spawn is not a cap (argv: %s)", i+1, argv)
		}
	}

	// A GATED task, DECLARING tools — the one combination that separates the
	// two spawns, and the one an earlier version of this fix got backwards.
	//
	// The gate is a PreToolUse hook and hooks exist only on the Session path,
	// so only Execute's spawn can carry it. There the node keeps everything
	// it declared: `tools:` bounds what EXISTS, the policy bounds what RUNS,
	// and joining them deletes a gated node's own tools (measured on three
	// shipped bots — whats-next losing Read/Glob/Grep, feed-watch losing the
	// WebFetch its whole job is, copilot losing Read/Glob/Bash/Skill).
	// The formatting spawn cannot carry the hook, so THERE the native surface
	// is withheld instead, or a `permission: deny` node resumes its own
	// transcript ungated with the whole roster under bypassPermissions.
	// Both ENABLED modes, not just deny: the withholding keys on
	// Policy.Enabled(), and the package has a neighbouring ask-flavoured
	// predicate — reaching for the wrong one would leave every
	// `permission: ask` node's formatting spawn ungated, and a deny-only
	// fixture cannot see it.
	//
	// DiagnosticShell is set because it is the other input that decides this
	// answer, and it decides it OPPOSITE ways on the two spawns: the opt-in
	// is a grant, so spawn #1 keeps Bash; spawn #2 has no gate to bound it,
	// so it does not.
	for _, mode := range []permission.Mode{permission.ModeAsk, permission.ModeDeny} {
		pol, err := permission.NewPolicy(mode, nil, nil, []string{"Bash"})
		if err != nil {
			t.Fatal(err)
		}
		gated := spawnArgv(t, Task{
			NodeID:          "n",
			Permission:      pol,
			AllowedTools:    []string{"read_file", "bash"},
			ToolsDeclared:   true,
			DiagnosticShell: true,
			ToolMaxSteps:    25,
			OutputSchema:    []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
		})
		if len(gated) < 2 {
			t.Fatalf("mode %v: expected two spawns for the gated row, got %d", mode, len(gated))
		}
		if got := disallowed(gated[0]); slices.Contains(got, "Read") || slices.Contains(got, "Bash") {
			t.Errorf("mode %v: the gated spawn that CARRIES the hook removed the tools the node declared (disallowed=%v)", mode, got)
		}
		for _, native := range roster {
			if got := disallowed(gated[1]); !slices.Contains(got, native) {
				t.Errorf("mode %v: the ungated formatting spawn of a GATED task keeps %q (disallowed=%v)", mode, native, got)
			}
		}
		// …and the two lists stay DISJOINT there. Emitting the declaration
		// as well would name `Read` on --allowedTools and on
		// --disallowedTools in one argv, which makes the withholding rest on
		// the CLI resolving deny over allow — a precedence this repo has
		// never executed. Disjoint lists hold whatever it is.
		if f := strings.Fields(gated[1]); slices.Contains(f, "--allowedTools") {
			t.Errorf("mode %v: the gated formatting spawn emits an approval list beside its withholding: %s", mode, gated[1])
		}
		// The bounds that are NOT the declaration reach this spawn from a
		// statement OUTSIDE the gated/ungated branch. Asserted on the gated
		// arm too: moving `claudeSpawnBounds` one indentation level, into the
		// `else`, strips all three from every gated node — measured, with the
		// whole suite green.
		f := strings.Fields(gated[1])
		if !slices.Contains(f, "--strict-mcp-config") {
			t.Errorf("mode %v: the gated formatting spawn lost --strict-mcp-config — the operator's ~/.claude.json servers boot inside this bot node (#506)", mode)
		}
		if j := slices.Index(f, "--max-turns"); j < 0 || j+1 >= len(f) || f[j+1] != "25" {
			t.Errorf("mode %v: the gated formatting spawn runs uncapped although the node set tool_max_steps: 25 (argv: %s)", mode, gated[1])
		}
		if got := disallowed(gated[1]); !slices.Contains(got, "Workflow") {
			t.Errorf("mode %v: the gated formatting spawn keeps Workflow — the one tool the DATA in the resumed transcript can arm (disallowed=%v)", mode, got)
		}
		// The orchestration surface is OUTSIDE the roster the loop above
		// checks. `claudeNativeTools` happens to carry `Task`, so `Task` was
		// withheld by accident while `Agent` — the spelling the current CLI
		// uses — `TaskOutput` and `Monitor` were not: a gated node resumed its
		// own transcript under bypassPermissions, with no hook, and could
		// still spawn a subagent whose tool calls nothing it declared bounds.
		// claudeSpawnBounds removes these only under an opt-in knob that is
		// off by default, which is a question about the model family, not
		// about the gate.
		for _, orch := range []string{"Agent", "Task", "TaskOutput", "Monitor"} {
			if got := disallowed(gated[1]); !slices.Contains(got, orch) {
				t.Errorf("mode %v: the gated formatting spawn keeps %q — a subagent spawned there is bounded by nothing the node declared (disallowed=%v)", mode, orch, got)
			}
		}
		// One name, once. Both flags are sets and both options append, so the
		// roster (which carries `Task`) composed with the orchestration list
		// (which carries it too) must still put it on the argv once.
		if got := disallowed(gated[1]); len(got) != len(uniq(got)) {
			t.Errorf("mode %v: the gated formatting spawn repeats a name on --disallowedTools: %v", mode, got)
		}
	}

	// The same spawn for an UNGATED task still carries the declaration —
	// otherwise "skip it when gated" would have quietly become "never send
	// it", and the tools half of this change would stop reaching pass 2.
	ungated := spawnArgv(t, Task{
		NodeID:        "n",
		AllowedTools:  []string{"read_file"},
		ToolsDeclared: true,
		OutputSchema:  []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	})
	if len(ungated) < 2 {
		t.Fatalf("expected two spawns for the ungated declared row, got %d", len(ungated))
	}
	if f := strings.Fields(ungated[1]); !slices.Contains(f, "--allowedTools") {
		t.Errorf("the ungated formatting spawn lost the node's declaration: %s", ungated[1])
	}
	if got := disallowed(ungated[1]); slices.Contains(got, "Read") || !slices.Contains(got, "Bash") {
		t.Errorf("the ungated formatting spawn must keep read_file and remove Bash (disallowed=%v)", got)
	}

	// A GATED **ULTRACODE** node: the row that separates "who may orchestrate"
	// from "when a call may run". Ultracode grants the Workflow surface, and
	// claudeSpawnBounds therefore keeps it on every spawn of such a node —
	// keyed on ultracode, which has nothing to say about the gate. On THIS
	// spawn there is no hook, so the policy the author wrote cannot refuse a
	// single call; and `Workflow` is the tool the CLI arms on the word
	// "ultracode" appearing anywhere in the prompt — which is the transcript
	// this pass resumes, content included.
	gatedUltra := spawnArgv(t, Task{
		NodeID:        "n",
		Permission:    mustPolicy(t, permission.ModeDeny),
		Ultracode:     true,
		AllowedTools:  []string{"read_file"},
		ToolsDeclared: true,
		OutputSchema:  []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	})
	if len(gatedUltra) < 2 {
		t.Fatalf("expected two spawns for the gated ultracode row, got %d", len(gatedUltra))
	}
	if got := disallowed(gatedUltra[0]); slices.Contains(got, "Workflow") {
		t.Errorf("the gated ultracode spawn that CARRIES the hook lost Workflow — ultracode granted it and the gate is what bounds it (disallowed=%v)", got)
	}
	// The last group is what the LIVE CLI was measured still registering once
	// the three lists above are withheld. Two names an earlier pass guessed —
	// `SlashCommand`, `TaskList` — are not tools of this CLI at all, which is
	// why the list is read from the product rather than assembled from
	// neighbouring source files.
	for _, name := range []string{
		"Workflow", "Agent", "TaskOutput", "Monitor",
		"EnterWorktree", "ExitWorktree", "CronCreate", "CronDelete", "CronList",
		"ScheduleWakeup", "SendMessage", "RemoteTrigger", "BashOutput", "KillShell", "TaskStop",
	} {
		if got := disallowed(gatedUltra[1]); !slices.Contains(got, name) {
			t.Errorf("the gated ultracode formatting spawn keeps %q with no hook to bound it (disallowed=%v)", name, got)
		}
	}
	// The advisory sentence rides the GATED arm only, and it exempts
	// StructuredOutput by name. Both halves matter: an ungated
	// structured-output pass is the `--max-turns` fallback and legitimately
	// does tool work (54 of the catalog's 57 two-pass nodes are ungated), and
	// StructuredOutput is how the agent RETURNS its result — a blanket "call
	// no tools" would push every gated schema'd node onto the text fallback.
	const advisory = "Do not call any tool other than StructuredOutput"
	if last := gatedUltra[len(gatedUltra)-1]; !strings.Contains(last, advisory) {
		t.Errorf("the gated formatting spawn carries no instruction covering the names the roster misses: %s", last)
	}
	ungatedPass := ungated2ndPassProbe(t)
	if last := ungatedPass[len(ungatedPass)-1]; strings.Contains(last, "Do not call any tool") {
		t.Errorf("an UNGATED formatting spawn was told to call no tools — that pass is the --max-turns fallback and is where the node collects its work: %s", last)
	}

	// A named list removes what it does not name and KEEPS what it does.
	named := spawnArgv(t, Task{NodeID: "n", AllowedTools: []string{"read_file"}, ToolsDeclared: true})
	got := disallowed(named[0])
	if slices.Contains(got, "Read") {
		t.Errorf("`tools: [read_file]` removed the one tool it declared (disallowed=%v)", got)
	}
	if !slices.Contains(got, "Bash") || !slices.Contains(got, "Write") {
		t.Errorf("`tools: [read_file]` left a write surface alive (disallowed=%v)", got)
	}
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func mustPolicy(t *testing.T, mode permission.Mode) *permission.Policy {
	t.Helper()
	pol, err := permission.NewPolicy(mode, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pol
}

// ungated2ndPassProbe is an UNGATED structured-output spawn pair, so the
// assertion on the gated arm cannot pass by being true everywhere.
func ungated2ndPassProbe(t *testing.T) []string {
	t.Helper()
	spawns := spawnArgv(t, Task{
		NodeID:       "n",
		OutputSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	})
	if len(spawns) < 2 {
		t.Fatalf("expected two spawns for the ungated probe, got %d", len(spawns))
	}
	return spawns
}

// The one tool the formatting pass needs must never be withheld: it is how the
// agent RETURNS its result. A list assembled by enumeration is exactly where
// that gets swept in by accident.
func TestTheGatedFormattingWithholdingSparesTheToolThePassNeeds(t *testing.T) {
	for _, name := range gatedFormattingWithheld() {
		if name == "StructuredOutput" {
			t.Fatalf("the gated formatting spawn withholds StructuredOutput — every schema'd gated node would fall back to text parsing")
		}
	}
	// …and the enumeration carries no name twice, so the argv stays a witness.
	seen := map[string]bool{}
	for _, name := range gatedFormattingWithheld() {
		if seen[name] {
			t.Errorf("gatedFormattingWithheld repeats %q", name)
		}
		seen[name] = true
	}
}

// The stand-in reads the whole request before it answers — pinned without
// timing luck: the user message goes out after the stand-in has had ample time
// to answer and exit, which is the order a slow writer produces under -race.
func TestTheStandInCLIReadsTheWholeRequestBeforeAnswering(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte(fakeClaudeArgv), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGV_LOG", filepath.Join(dir, "argv.log"))

	cmd := exec.Command(script, "--input-format", "stream-json")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, `{"type":"control_request","request":{"subtype":"initialize"}}`+"\n"); err != nil {
		t.Fatalf("writing the initialize request: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := io.WriteString(stdin, `{"message":{"content":"x","role":"user"},"type":"user"}`+"\n"); err != nil {
		t.Fatalf("the user message, written after the initialize request, failed: %v — the stand-in answered "+
			"before it had read the whole request", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("stand-in: %v (output %q)", err, out.String())
	}
	if !strings.Contains(out.String(), `"type":"result"`) {
		t.Fatalf("the stand-in answered no result: %q", out.String())
	}
}
