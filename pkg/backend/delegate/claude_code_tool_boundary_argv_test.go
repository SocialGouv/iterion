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
//
// When $ENV_LOG is set it also records, one line per spawn, the values that
// spawn saw for the variables every spawn pins, each bracketed so an empty
// value still shows: "bg=[1] mem=[1] bdef=[405000] bmax=[810000]", with
// "<unset>" for an absent one.
//
// When $SETTINGS_LOG is set it also records, one line per spawn, the content
// of the file its --settings flag names — the flag settings object as the CLI
// reads it — or a "<no settings file: …>" marker.
//
// When $STDIN_DIR is set it also records, in $STDIN_DIR/<spawn number>, the
// control lines it READ on stdin — which is where a hook-borne bound travels,
// and the only place a spawn's gate can be observed at all. With $HOOK_PROBE
// set it additionally plays the CLI's half of one PreToolUse callback on the
// structured-output spawn and records the verdict it gets back: a registration
// nobody answers is not a gate. That spawn is the one carrying --resume, not
// the one carrying --json-schema: pass 1 asks for the schema too (the CLI
// takes it together with --allowedTools since 2.1), and probing pass 1 pauses
// an `ask` run before pass 2 exists.
const fakeClaudeArgv = `#!/bin/sh
printf '%s' "$*" | tr '\n' ' ' >> "$ARGV_LOG"; printf '\n' >> "$ARGV_LOG"
spawn=$(wc -l < "$ARGV_LOG" | tr -d ' ')
if [ -n "$ENV_LOG" ]; then printf 'bg=[%s] mem=[%s] bdef=[%s] bmax=[%s] rtkdb=[%s] rtkrecall=[%s] sse=[%s] rr=[%s] exitdelay=[%s] bgceil=[%s]\n' "${CLAUDE_CODE_DISABLE_BACKGROUND_TASKS-<unset>}" "${CLAUDE_CODE_DISABLE_AUTO_MEMORY-<unset>}" "${BASH_DEFAULT_TIMEOUT_MS-<unset>}" "${BASH_MAX_TIMEOUT_MS-<unset>}" "${RTK_DB_PATH-<unset>}" "${RTK_RECALL-<unset>}" "${CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS-<unset>}" "${CLAUDE_CODE_BG_TASKS_REPORT_RUNNING-<unset>}" "${CLAUDE_CODE_EXIT_AFTER_STOP_DELAY-<unset>}" "${CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS-<unset>}" >> "$ENV_LOG"; fi
if [ -n "$SETTINGS_LOG" ]; then
	settings=; prev=
	for a in "$@"; do if [ "$prev" = --settings ]; then settings=$a; fi; prev=$a; done
	if [ -n "$settings" ] && [ -f "$settings" ]; then tr '\n' ' ' < "$settings" >> "$SETTINGS_LOG"; else printf '<no settings file: %s>' "$settings" >> "$SETTINGS_LOG"; fi
	printf '\n' >> "$SETTINGS_LOG"
fi

case "$*" in *--input-format*)
	while read -r line; do
		if [ -n "$STDIN_DIR" ]; then printf '%s\n' "$line" >> "$STDIN_DIR/$spawn"; fi
		case "$line" in *'"type":"user"'*) break ;; esac
	done ;;
esac
if [ -n "$HOOK_PROBE" ]; then
	case "$*" in *--resume*)
		printf '%s\n' '{"type":"control_request","request_id":"probe1","request":{"subtype":"hook_callback","callback_id":"hook_PreToolUse_0","input":{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"rm -rf /nope"}}}}'
		read -r verdict
		if [ -n "$STDIN_DIR" ] && [ -n "$verdict" ]; then printf '%s\n' "$verdict" >> "$STDIN_DIR/$spawn"; fi ;;
	esac
fi
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"s1"}'
`

// spawnArgv runs the REAL ClaudeCodeBackend.Execute against the stand-in CLI
// and returns one string per spawn.
func spawnArgv(t *testing.T, task Task) []string {
	t.Helper()
	argv, _ := spawnArgvEnv(t, task)
	return argv
}

// spawnArgvEnv is spawnArgv plus, per spawn and in the same order, the pinned
// variables the spawned process saw, keyed bg/mem/bdef/bmax.
func spawnArgvEnv(t *testing.T, task Task) (argv []string, pinned []map[string]string) {
	t.Helper()
	rec := spawnRecorded(t, task)
	return rec.argv, rec.pinned
}

// spawnRecord is what the stand-in CLI recorded for each spawn of one Execute,
// in spawn order.
type spawnRecord struct {
	argv     []string
	pinned   []map[string]string
	settings []string // the content of the file --settings names
	logs     []string // what the backend logged during this Execute, level-prefixed
}

func spawnRecorded(t *testing.T, task Task) spawnRecord {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte(fakeClaudeArgv), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "argv.log")
	t.Setenv("ARGV_LOG", log)
	envLog := filepath.Join(dir, "env.log")
	t.Setenv("ENV_LOG", envLog)
	settingsLog := filepath.Join(dir, "settings.log")
	t.Setenv("SETTINGS_LOG", settingsLog)

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
	var rec spawnRecord
	// Copied under the lock: the SDK's own goroutines reach the hook, and a
	// reader that only happens to run after them is not thread-safe.
	mu.Lock()
	t.Logf("Execute: err=%v, formatting pass used=%v; backend log:\n%s",
		execErr, res.FormattingPassUsed, strings.Join(lines, "\n"))
	rec.logs = append([]string(nil), lines...)
	mu.Unlock()

	rec.argv = readSpawnLog(t, log)
	for _, line := range readSpawnLog(t, envLog) {
		vars := map[string]string{}
		for _, field := range strings.Fields(line) {
			if name, value, ok := strings.Cut(field, "="); ok {
				vars[name] = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
			}
		}
		rec.pinned = append(rec.pinned, vars)
	}
	rec.settings = readSpawnLog(t, settingsLog)
	if len(rec.pinned) != len(rec.argv) || len(rec.settings) != len(rec.argv) {
		t.Fatalf("the stand-in recorded %d argv line(s), %d env line(s) and %d settings line(s)",
			len(rec.argv), len(rec.pinned), len(rec.settings))
	}
	return rec
}

func readSpawnLog(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
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
	// These rows assert what the node DECLARES: the orchestration knob is
	// the environment's and would add its own withholdings on top.
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
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

	// An UNDECLARED list keeps the legacy unrestricted surface: only the
	// tools no headless session can use are withheld, by their own rule and
	// not by the declaration — on EVERY spawn. This is the row that carries a
	// schema on purpose: `Workflow` is the one tool the CLI arms on the word
	// "ultracode" appearing anywhere in the prompt, and the formatting pass
	// resumes the transcript the node's own content is in, so a spawn that
	// drops the withholding lets the DATA grant an orchestration no session
	// here can collect.
	undeclared := spawnArgv(t, Task{
		NodeID:       "n",
		OutputSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	})
	if len(undeclared) < 2 {
		t.Fatalf("expected two spawns for the undeclared row, got %d", len(undeclared))
	}
	for i, argv := range undeclared {
		if got := disallowed(argv); !slices.Equal(got, headlessWithheldNames) {
			t.Errorf("spawn #%d of an undeclared list disallowed %v, want only the headless-withheld %v", i+1, got, headlessWithheldNames)
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
	// two spawns, and the one two earlier versions of this fix got backwards
	// in opposite directions.
	//
	// The gate is a PreToolUse hook, so it holds on the spawn it is
	// registered on — and since #1672.2 BOTH spawns register it through the
	// one constructor (claudePermissionGate), so both read the same answer.
	// What that means on argv is that the DECLARATION reaches both spawns
	// unchanged: `tools:` bounds what EXISTS, the policy bounds what RUNS,
	// and joining them deletes a gated node's own tools (measured on three
	// shipped bots — whats-next losing Read/Glob/Grep, feed-watch losing the
	// WebFetch its whole job is, copilot losing Read/Glob/Bash/Skill).
	// That the gate itself reaches the second spawn is not visible here at
	// all — it travels on stdin, and TestTheGatedFormattingSpawnCarriesTheGateOnItsStdin
	// is where it is read.
	// Both ENABLED modes, not just deny: the package has a neighbouring
	// ask-flavoured predicate (CanAsk), and reaching for the wrong one would
	// leave every `permission: ask` node's formatting spawn ungated — a
	// deny-only fixture cannot see it.
	//
	// DiagnosticShell is set because it is the other input that decides this
	// answer, and it must decide it the SAME way on both spawns now: the
	// opt-in is a grant, and a grant that survives one spawn and not the
	// other is the bug this row exists for.
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
		for i, argv := range gated {
			if got := disallowed(argv); slices.Contains(got, "Read") || slices.Contains(got, "Bash") {
				t.Errorf("mode %v: spawn #%d of a gated node removed the tools it declared (disallowed=%v)", mode, i+1, got)
			}
			if f := strings.Fields(argv); !slices.Contains(f, "--allowedTools") {
				t.Errorf("mode %v: spawn #%d of a gated node carries no declaration: %s", mode, i+1, argv)
			}
			// The bounds that are NOT the declaration reach both spawns from
			// statements OUTSIDE any gated/ungated branch. Asserted on the
			// gated arm too: moving `claudeSpawnBounds` one indentation
			// level, into a branch, stripped all three from every gated node
			// — measured, with the whole suite green.
			f := strings.Fields(argv)
			if !slices.Contains(f, "--strict-mcp-config") {
				t.Errorf("mode %v: spawn #%d lost --strict-mcp-config — the operator's ~/.claude.json servers boot inside this bot node (#506)", mode, i+1)
			}
			if j := slices.Index(f, "--max-turns"); j < 0 || j+1 >= len(f) || f[j+1] != "25" {
				t.Errorf("mode %v: spawn #%d runs uncapped although the node set tool_max_steps: 25 (argv: %s)", mode, i+1, argv)
			}
			if got := disallowed(argv); !slices.Contains(got, "Workflow") {
				t.Errorf("mode %v: spawn #%d keeps Workflow — the one tool the DATA in the resumed transcript can arm (disallowed=%v)", mode, i+1, got)
			}
			// One name, once. Both flags are sets and both options append, so
			// a caller composing several bounds onto one spawn must still put
			// each name on the argv once.
			if got := disallowed(argv); len(got) != len(uniq(got)) {
				t.Errorf("mode %v: spawn #%d repeats a name on --disallowedTools: %v", mode, i+1, got)
			}
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
	// from "when a call may run". Ultracode keys what a spawn KEEPS; the gate
	// decides what may RUN, and since #1672.2 it decides it on both spawns.
	// So the argv answer is the same on both: `Workflow` goes (the CLI arms
	// it on the word "ultracode" appearing anywhere in the prompt — which is
	// the transcript the second pass resumes, content included, and it only
	// ever runs in the background), the declaration's own withholding goes,
	// and everything else is the gate's business rather than this flag's.
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
	for i, argv := range gatedUltra {
		got := disallowed(argv)
		if slices.Contains(got, "Read") {
			t.Errorf("spawn #%d of a gated ultracode node removed the one tool it declared (disallowed=%v)", i+1, got)
		}
		// headlessWithheldTools holds on EVERY spawn, ultracode included:
		// each name hands work to a later turn this session will never have.
		// `Agent` and `Task` are withheld here by the DECLARATION (the roster
		// carries both since #1651, and `read_file` grants neither) — which
		// is why the two groups are asserted apart.
		for _, name := range []string{
			"Workflow", "ScheduleWakeup", "CronCreate", "CronDelete", "CronList",
			"RemoteTrigger", "AskUserQuestion", "SlashCommand", "BashOutput", "KillShell",
		} {
			if !slices.Contains(got, name) {
				t.Errorf("spawn #%d keeps %q, which can only hand work to a turn this session will not have (disallowed=%v)", i+1, name, got)
			}
		}
		for _, name := range []string{"Agent", "Task", "Bash", "Write"} {
			if !slices.Contains(got, name) {
				t.Errorf("spawn #%d keeps %q, which `tools: [read_file]` does not declare (disallowed=%v)", i+1, name, got)
			}
		}
	}
	// NEITHER pass is told to call no tools now. The sentence a gated pass
	// used to carry was the advisory half of a withholding that no longer
	// exists, and it is false where the policy allows a tool: this pass is
	// the `--max-turns` fallback for a first pass cut off mid-work, which is
	// the one case where it must still be able to act (54 of the catalog's 57
	// two-pass nodes are ungated, and the gated ones are exactly the ones
	// whose work matters most). StructuredOutput is exempt in the policy, so
	// the tool the pass RETURNS its result with is never refused.
	ungatedPass := ungated2ndPassProbe(t)
	for name, spawns := range map[string][]string{"gated": gatedUltra, "ungated": ungatedPass} {
		if last := spawns[len(spawns)-1]; strings.Contains(last, "Do not call any tool") {
			t.Errorf("the %s formatting spawn was told to call no tools — the gate decides that now, call by call: %s", name, last)
		}
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

// The one tool the formatting pass needs must never be refused: it is how the
// agent RETURNS its result. The withholding that used to answer for this is
// gone (#1672.2), so the question moves to the gate — and the gate answers it
// with an exemption rather than with a list, which is why it holds for every
// spelling and every mode.
func TestTheFormattingPassCanAlwaysReturnItsResult(t *testing.T) {
	for _, mode := range []permission.Mode{permission.ModeAsk, permission.ModeDeny} {
		// The strictest shape an author can write: deny everything by mode,
		// and name the tool in a deny rule as well.
		pol, err := permission.NewPolicy(mode, nil, nil, []string{"StructuredOutput", "Bash"})
		if err != nil {
			t.Fatal(err)
		}
		if dec, rule := pol.Evaluate("StructuredOutput", nil); dec != permission.Allow {
			t.Errorf("mode %v: StructuredOutput evaluates to %v (rule %q) — every schema'd gated node would fall back to text parsing", mode, dec, rule)
		}
		// …and the withholding is not hiding somewhere on the argv either.
		for i, argv := range spawnArgv(t, Task{
			NodeID:        "n",
			Permission:    pol,
			AllowedTools:  []string{"read_file"},
			ToolsDeclared: true,
			OutputSchema:  []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
		}) {
			if got := disallowed(argv); slices.Contains(got, "StructuredOutput") {
				t.Errorf("mode %v: spawn #%d withholds StructuredOutput on the command line (disallowed=%v)", mode, i+1, got)
			}
		}
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
