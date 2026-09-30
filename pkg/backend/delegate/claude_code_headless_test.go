package delegate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// headlessWithheldNames is what every claude_code spawn must carry on
// --disallowedTools, spelled out here rather than read from the product's own
// list: a name dropped from that list must redden a test, not move both sides.
var headlessWithheldNames = []string{
	"Workflow",
	"ScheduleWakeup", "CronCreate", "CronDelete", "CronList",
	"RemoteTrigger",
}

const schemaOK = `{"type":"object","properties":{"ok":{"type":"boolean"}}}`

// subagentRuleHeading marks headlessSubagentRule on a recorded argv line. The
// stand-in flattens newlines to spaces, and the heading survives that intact.
const subagentRuleHeading = "## Subagents in this session"

// The Bash timeouts every spawn must pin under the watchdogs pinWatchdogs
// sets (hot idle tier 15m, no-progress tier 25m): the maximum sits 90 s under
// the tighter one, the default is half of it. Literals, so a change to the
// derivation reddens here instead of moving both sides.
const (
	wantBashDefaultMs = "405000"
	wantBashMaxMs     = "810000"
)

// pinWatchdogs fixes the session watchdogs the Bash timeouts derive from, and
// the background switches (the default mode: foreground, lifecycle on), so
// the expected values do not depend on the environment the tests run in.
func pinWatchdogs(t *testing.T) {
	t.Helper()
	t.Setenv("ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT", "15m")
	t.Setenv("ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT", "25m")
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_TASKS", "")
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE", "")
}

// Every spawn a claude_code task makes runs with the pinned environment in
// its process — background work off, the node's auto-memory decision, Bash
// timeouts that fit the session's watchdogs — and without the tools that hand
// work to a later turn: the Session spawn, every formatting pass, the resume,
// ultracode or not, gated or not.
//
// Asserted on the process the REAL Execute starts, through the stand-in CLI:
// an environment variable proves nothing until the spawned process sees it.
// The test process sets every pinned variable to a wrong value first, so a
// spawn that forgets one inherits the wrong value and reddens, instead of
// passing on an ambient value nobody set.
func TestEverySpawnRunsWithThePinnedEnvironment(t *testing.T) {
	pinWatchdogs(t)
	t.Setenv(backgroundTasksOffEnv, "0")
	t.Setenv(autoMemoryDisableEnv, "2")
	t.Setenv(bashDefaultTimeoutEnv, "7")
	t.Setenv(bashMaxTimeoutEnv, "8")
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	rows := []struct {
		name   string
		task   Task
		memory string // the auto-memory switch the node decides
	}{
		{"plain node", Task{NodeID: "n", OutputSchema: []byte(schemaOK)}, "1"},
		{"ultracode node", Task{NodeID: "n", Ultracode: true, OutputSchema: []byte(schemaOK)}, "1"},
		{"memory on", Task{NodeID: "n", AutoMemoryDir: t.TempDir(), OutputSchema: []byte(schemaOK)}, "0"},
		{"declared tools: the two-pass formatting path", Task{NodeID: "n", AllowedTools: []string{"read_file"}, ToolsDeclared: true, OutputSchema: []byte(schemaOK)}, "1"},
		{"gated ultracode node", Task{NodeID: "n", Ultracode: true, Permission: mustPolicy(t, permission.ModeDeny), OutputSchema: []byte(schemaOK)}, "1"},
		{"resumed session", Task{NodeID: "n", SessionID: "prev", OutputSchema: []byte(schemaOK)}, "1"},
		// A provisioning layer cannot move a pinned variable. ExtraEnv is
		// applied twice per spawn, the second time by the credential env that
		// comes last, so the keys have to be dropped from both.
		{"ExtraEnv tries every pinned variable", Task{NodeID: "n", ExtraEnv: []string{
			backgroundTasksOffEnv + "=0", autoMemoryDisableEnv + "=0",
			bashDefaultTimeoutEnv + "=1", bashMaxTimeoutEnv + "=2",
		}, OutputSchema: []byte(schemaOK)}, "1"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			argv, env := spawnArgvEnv(t, row.task)
			if len(argv) < 2 {
				t.Fatalf("expected the Session spawn and at least one formatting pass, got %d spawn(s)", len(argv))
			}
			if row.task.SessionID != "" && !strings.Contains(argv[0], "--resume "+row.task.SessionID) {
				t.Fatalf("the first spawn does not resume %q: %s", row.task.SessionID, argv[0])
			}
			for i := range argv {
				if env[i]["bg"] != "1" {
					t.Errorf("spawn #%d ran with %s=%s, want 1: a background result reaches a one-shot session only while its turn is still running",
						i+1, backgroundTasksOffEnv, env[i]["bg"])
				}
				if env[i]["mem"] != row.memory {
					t.Errorf("spawn #%d ran with %s=%s, want %s: the node's auto-memory decision",
						i+1, autoMemoryDisableEnv, env[i]["mem"], row.memory)
				}
				if env[i]["bdef"] != wantBashDefaultMs {
					t.Errorf("spawn #%d ran with %s=%s, want %s", i+1, bashDefaultTimeoutEnv, env[i]["bdef"], wantBashDefaultMs)
				}
				if env[i]["bmax"] != wantBashMaxMs {
					t.Errorf("spawn #%d ran with %s=%s, want %s: under the session's silence watchdog", i+1, bashMaxTimeoutEnv, env[i]["bmax"], wantBashMaxMs)
				}
				got := disallowed(argv[i])
				for _, name := range headlessWithheldNames {
					if !slices.Contains(got, name) {
						t.Errorf("spawn #%d keeps %s, a tool that hands work to a turn this session never has (disallowed=%v)", i+1, name, got)
					}
				}
			}
		})
	}
}

// flagSettings parses the flag settings object a spawn read from the file its
// `--settings` flag names (spawnRecord.settings).
func flagSettings(t *testing.T, content string) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		t.Fatalf("the --settings file is not a JSON object (%s): %v", content, err)
	}
	return out
}

// The CLI rewrites its own environment at startup from the `env` block of
// every settings source it loads, in the order user, project, local, flag,
// policy, and reads the pinned variables live afterwards. A target
// repository's committed .claude/settings.json (loaded under --setting-sources
// project), or the operator's user settings, would override the process
// environment: switch background work back on, turn auto-memory back on
// against the operator's personal memory, move the Bash timeouts. So every
// spawn pins them in the flag layer too, and that layer is the one --settings
// object: the pins must MERGE with the auto-memory keys that ride it, never
// replace them or be replaced by them. One assertion per variable, so a
// variable dropped from the layer reddens on its own line.
func TestEverySpawnPinsTheEnvironmentInTheFlagSettingsLayer(t *testing.T) {
	pinWatchdogs(t)
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	memoryDir := t.TempDir()
	rows := []struct {
		name   string
		task   Task
		memory bool
	}{
		{name: "memory off", task: Task{NodeID: "n", OutputSchema: []byte(schemaOK)}},
		{name: "memory on", task: Task{NodeID: "n", AutoMemoryDir: memoryDir, OutputSchema: []byte(schemaOK)}, memory: true},
		{name: "gated ultracode, memory on", task: Task{NodeID: "n", Ultracode: true, AutoMemoryDir: memoryDir, Permission: mustPolicy(t, permission.ModeDeny), OutputSchema: []byte(schemaOK)}, memory: true},
		{name: "declared tools: the two-pass formatting path", task: Task{NodeID: "n", AllowedTools: []string{"read_file"}, ToolsDeclared: true, OutputSchema: []byte(schemaOK)}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rec := spawnRecorded(t, row.task)
			argv := rec.argv
			if len(argv) < 2 {
				t.Fatalf("expected the Session spawn and at least one formatting pass, got %d spawn(s)", len(argv))
			}
			wantMemory := "1"
			if row.memory {
				wantMemory = "0"
			}
			for i, a := range argv {
				if n := strings.Count(a, "--settings "); n != 1 {
					t.Fatalf("spawn #%d carries %d --settings flag(s), want exactly one: %s", i+1, n, a)
				}
				settings := flagSettings(t, rec.settings[i])
				var env map[string]string
				_ = json.Unmarshal(settings["env"], &env)
				if env[backgroundTasksOffEnv] != "1" {
					t.Errorf("spawn #%d does not pin %s=1 in the flag settings layer (env=%v): a repository's settings env could switch background work back on",
						i+1, backgroundTasksOffEnv, env)
				}
				if env[autoMemoryDisableEnv] != wantMemory {
					t.Errorf("spawn #%d does not pin %s=%s in the flag settings layer (env=%v): a repository's settings env could turn the operator's personal memory back on",
						i+1, autoMemoryDisableEnv, wantMemory, env)
				}
				if env[bashDefaultTimeoutEnv] != wantBashDefaultMs || env[bashMaxTimeoutEnv] != wantBashMaxMs {
					t.Errorf("spawn #%d does not pin the Bash timeouts %s/%s in the flag settings layer (env=%v)",
						i+1, wantBashDefaultMs, wantBashMaxMs, env)
				}
				var dir string
				_ = json.Unmarshal(settings["autoMemoryDirectory"], &dir)
				if row.memory && dir != memoryDir {
					t.Errorf("spawn #%d lost the memory directory from the merged settings (autoMemoryDirectory=%q, want %q)", i+1, dir, memoryDir)
				}
				if !row.memory && settings["autoMemoryDirectory"] != nil {
					t.Errorf("spawn #%d pins a memory directory on a node whose memory is off: %s", i+1, settings["autoMemoryDirectory"])
				}
			}
		})
	}
}

// The sandboxed spawns carry the pins too: there the environment is not
// inherited at all, only the map the builder hands to the sandbox reaches the
// process. Captured at the sandbox seam for the Session spawn Execute makes
// and for the formatting pass; the double never runs the command it is given.
func TestSandboxedSpawnsRunWithThePinnedEnvironment(t *testing.T) {
	resetClaudeCredEnv(t)
	pinWatchdogs(t)
	want := map[string]string{
		backgroundTasksOffEnv: "1",
		autoMemoryDisableEnv:  "1",
		bashDefaultTimeoutEnv: wantBashDefaultMs,
		bashMaxTimeoutEnv:     wantBashMaxMs,
	}
	for _, formatting := range []bool{false, true} {
		fake := &captureSandboxCmdRun{}
		task := Task{
			NodeID:       "n",
			UserPrompt:   "x",
			Sandbox:      fake,
			ExtraEnv:     []string{backgroundTasksOffEnv + "=0", bashMaxTimeoutEnv + "=2"},
			OutputSchema: []byte(schemaOK),
		}
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
		if formatting {
			_, _, _, _ = b.formatOutput(context.Background(), task, "sid")
		} else {
			_, _ = b.Execute(context.Background(), task)
		}
		spawns := 0
		for i, argv := range fake.argvs {
			if !slices.Contains(argv, "--print") {
				continue // the pidfile cleanup, not a CLI spawn
			}
			spawns++
			for key, value := range want {
				if got := fake.envs[i][key]; got != value {
					t.Errorf("formatting=%v: the sandboxed spawn ran with %s=%q, want %q", formatting, key, got, value)
				}
			}
		}
		if spawns == 0 {
			t.Fatalf("formatting=%v: no CLI spawn reached the sandbox (argvs=%v)", formatting, fake.argvs)
		}
	}
}

// With background work off, a Bash command that outlives its timeout is
// killed, and a foreground command is silent to this backend's watchdogs while
// it runs: the Bash maximum must sit strictly under the tighter enabled
// watchdog, or a legitimate long command aborts the whole session. The
// literals are the contract; the invariant is checked on every row.
func TestBashTimeoutsFitTheSessionWatchdogs(t *testing.T) {
	rows := []struct {
		hot, noProgress string
		bound           time.Duration // the tighter enabled watchdog, 0 when none
		wantDefault     int64
		wantMax         int64
	}{
		{hot: "15m", noProgress: "25m", bound: 15 * time.Minute, wantDefault: 405000, wantMax: 810000},
		{hot: "0", noProgress: "25m", bound: 25 * time.Minute, wantDefault: 675000, wantMax: 1350000},
		{hot: "5m", noProgress: "3m", bound: 3 * time.Minute, wantDefault: 75000, wantMax: 150000},
		{hot: "40s", noProgress: "0", bound: 40 * time.Second, wantDefault: 10000, wantMax: 20000},
		// Both watchdogs off: nothing can abort the session over a silent
		// command, so a command without a timeout of its own may run the hour
		// the maximum allows — the CLI's 2 min would kill it for nothing.
		{hot: "0", noProgress: "0", wantDefault: 3600000, wantMax: 3600000},
	}
	for _, row := range rows {
		t.Setenv("ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT", row.hot)
		t.Setenv("ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT", row.noProgress)
		def, maxMs := claudeBashTimeouts()
		if def != row.wantDefault || maxMs != row.wantMax {
			t.Errorf("hot=%s no-progress=%s: Bash timeouts %d/%d ms, want %d/%d", row.hot, row.noProgress, def, maxMs, row.wantDefault, row.wantMax)
		}
		if row.bound > 0 && maxMs >= row.bound.Milliseconds() {
			t.Errorf("hot=%s no-progress=%s: the Bash maximum %d ms is not under the session watchdog (%s)", row.hot, row.noProgress, maxMs, row.bound)
		}
		if def <= 0 || def > maxMs {
			t.Errorf("hot=%s no-progress=%s: the Bash default %d ms must be positive and within the maximum %d ms", row.hot, row.noProgress, def, maxMs)
		}
	}
}

// An ExtraEnv entry for a pinned key is dropped on every spawn, and the Session
// spawn says so: a provisioning layer that set it learns why it did not apply,
// instead of watching it vanish. An entry for any other key applies, unreported.
func TestExtraEnvThatTriesAPinnedKeyIsReported(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
	var warnings []string
	b.Logger.SetHook(func(level iterlog.Level, msg string, _ map[string]any) {
		if level == iterlog.LevelWarn {
			warnings = append(warnings, msg)
		}
	})
	task := Task{NodeID: "n", WorkDir: t.TempDir(), ExtraEnv: []string{
		backgroundTasksOffEnv + "=0", "ITERION_TEST_UNPINNED=1", bashMaxTimeoutEnv + "=2",
	}}
	_, cleanup := b.buildTransportOptions(task)
	if cleanup != nil {
		cleanup()
	}
	for _, key := range []string{backgroundTasksOffEnv, bashMaxTimeoutEnv} {
		found := false
		for _, w := range warnings {
			if strings.Contains(w, "ExtraEnv sets "+key) && strings.Contains(w, "pins") {
				found = true
			}
		}
		if !found {
			t.Errorf("no warning names the pinned key %s that ExtraEnv tried to set (warnings=%q)", key, warnings)
		}
	}
	for _, w := range warnings {
		if strings.Contains(w, "ITERION_TEST_UNPINNED") {
			t.Errorf("an unpinned ExtraEnv key was reported as overridden: %q", w)
		}
	}
}

// The subagent rule reaches the system prompt of every spawn that keeps a
// subagent tool — ultracode or not, exactly once — and no spawn that does not.
// Read off the argv the real Execute builds, next to the --disallowedTools
// that decides it.
func TestTheSubagentRuleReachesEverySpawnThatKeepsTheSubagentTool(t *testing.T) {
	pinWatchdogs(t)
	rows := []struct {
		name  string
		knob  string
		task  Task
		rule  int  // occurrences of the rule on the Session spawn
		ultra bool // the ultracode section is on the Session spawn too
	}{
		{name: "plain node keeps the tool", task: Task{NodeID: "n"}, rule: 1},
		{name: "ultracode node, once", task: Task{NodeID: "n", Ultracode: true}, rule: 1, ultra: true},
		{name: "resumed session", task: Task{NodeID: "n", SessionID: "prev"}, rule: 1},
		{name: "knob withholds the tool", knob: "1", task: Task{NodeID: "n"}, rule: 0},
		{name: "knob leaves an ultracode node its tool", knob: "1", task: Task{NodeID: "n", Ultracode: true}, rule: 1, ultra: true},
		// A declared list without `agent` puts `Task` on --disallowedTools, and
		// the CLI resolves that legacy name to `Agent`.
		{name: "declared list without agent", task: Task{NodeID: "n", AllowedTools: []string{"read_file"}, ToolsDeclared: true}, rule: 0},
		{name: "declared list with agent", task: Task{NodeID: "n", AllowedTools: []string{"read_file", "agent"}, ToolsDeclared: true}, rule: 1},
		{name: "gated ultracode node", task: Task{NodeID: "n", Ultracode: true, Permission: mustPolicy(t, permission.ModeDeny)}, rule: 1, ultra: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", row.knob)
			task := row.task
			task.SystemPrompt = "author prompt"
			task.OutputSchema = []byte(schemaOK)
			argv := spawnArgv(t, task)
			if len(argv) < 2 {
				t.Fatalf("expected the Session spawn and a formatting pass, got %d spawn(s)", len(argv))
			}
			got := disallowed(argv[0])
			keeps := !slices.Contains(got, "Agent") && !slices.Contains(got, "Task")
			if keeps != (row.rule == 1) {
				t.Fatalf("the Session spawn keeps a subagent tool=%v, the row expects %v (disallowed=%v)", keeps, row.rule == 1, got)
			}
			if n := strings.Count(argv[0], subagentRuleHeading); n != row.rule {
				t.Errorf("the Session spawn carries the subagent rule %d time(s), want %d (disallowed=%v)", n, row.rule, got)
			}
			if row.ultra && !strings.Contains(argv[0], "## Workflow Orchestration") {
				t.Errorf("the ultracode section is missing from the Session spawn")
			}
			// The formatting passes carry no appended system prompt at all: the
			// pins, not the text, are what hold there.
			for i, a := range argv[1:] {
				if strings.Contains(a, subagentRuleHeading) {
					t.Errorf("formatting spawn #%d carries the subagent rule", i+2)
				}
			}
		})
	}
}

// The rule states what the spawn does, and nothing the spawn does not: no
// waiting tool (TaskOutput is removed from the pinned CLI), no background
// parameter (the switch removes it from the schema), parallelism the way it
// works once subagents run in the foreground — several calls in one message —
// and what happens to a Bash command that outlives its timeout.
func TestTheSubagentRuleSaysWhatTheSpawnDoes(t *testing.T) {
	for _, want := range []string{
		"not interactive",
		"no completion notification, no scheduled wake-up",
		"run in the foreground",
		"returns that agent's report as its tool result",
		"several Agent calls in ONE message",
		"run concurrently",
		"before your next step",
		"Never end your turn to wait",
		"outlives its timeout is killed",
		"nohup",
	} {
		if !strings.Contains(headlessSubagentRule, want) {
			t.Errorf("the subagent rule does not say %q", want)
		}
	}
	for _, stale := range []string{"TaskOutput", "run_in_background", "background"} {
		if strings.Contains(headlessSubagentRule, stale) {
			t.Errorf("the subagent rule names %q, which the spawn no longer offers", stale)
		}
	}
}

// An operator who wants background work opts in:
// ITERION_CLAUDE_CODE_BACKGROUND_TASKS=on pins the CLI's switch EMPTY in both
// layers — a repository's settings "1" would win without the flag layer — and
// the system prompt no longer says subagents run in the foreground: the
// background lifecycle keeps the session open until their work comes back.
func TestBackgroundTasksOnLiftsTheForegroundPin(t *testing.T) {
	pinWatchdogs(t)
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_TASKS", "on")
	task := Task{NodeID: "n", OutputSchema: []byte(schemaOK)}
	argv, env := spawnArgvEnv(t, task)
	if len(argv) < 2 {
		t.Fatalf("expected the Session spawn and at least one formatting pass, got %d spawn(s)", len(argv))
	}
	for i := range argv {
		if got, ok := env[i]["bg"]; !ok || got != "" {
			t.Errorf("spawn #%d ran with %s=%q, want it set empty: background work was asked for", i+1, backgroundTasksOffEnv, got)
		}
		var layer map[string]string
		_ = json.Unmarshal(flagSettings(t, argv[i])["env"], &layer)
		if got, ok := layer[backgroundTasksOffEnv]; !ok || got != "" {
			t.Errorf("spawn #%d's flag settings layer has %s=%q (present %v), want it empty: a repository's settings env would switch background work off", i+1, backgroundTasksOffEnv, got, ok)
		}
	}
	if prompt := claudeCodeSystemPrompt(task); strings.Contains(prompt, subagentRuleHeading) {
		t.Errorf("the system prompt says subagents run in the foreground while background work is on:\n%s", prompt)
	}
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_TASKS", "")
	if prompt := claudeCodeSystemPrompt(task); !strings.Contains(prompt, subagentRuleHeading) {
		t.Fatal("scenario broken: the foreground rule is missing with background work off")
	}
}

// The rewriter chain's run env is pinned like the switches are: in the process
// environment and in the flag settings layer (a repository's or the
// operator's settings env would otherwise put rtk's history back on), on both
// spawns, and a provisioning layer's value for one of its names is dropped.
func TestTheRewritersRunEnvIsPinnedInBothLayers(t *testing.T) {
	pinWatchdogs(t)
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	rw := filepath.Join(t.TempDir(), "fakerw")
	if err := os.WriteFile(rw, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := Task{NodeID: "n", OutputSchema: []byte(schemaOK), ExtraEnv: []string{"RTK_RECALL=1"},
		Rewriters: []plugin.RewriterSpec{{ID: "rtk", Locate: plugin.LocateSpec{Paths: []string{rw}},
			Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}},
			RunEnv: map[string]string{"RTK_DB_PATH": "/dev/null/iterion-rtk-history.db", "RTK_RECALL": "0"}}}}
	argv, env := spawnArgvEnv(t, task)
	if len(argv) < 2 {
		t.Fatalf("expected the Session spawn and at least one formatting pass, got %d spawn(s)", len(argv))
	}
	for i := range argv {
		if env[i]["rtkdb"] != "/dev/null/iterion-rtk-history.db" || env[i]["rtkrecall"] != "0" {
			t.Errorf("spawn #%d ran with RTK_DB_PATH=%s RTK_RECALL=%s, want the run env over the provisioning layer's", i+1, env[i]["rtkdb"], env[i]["rtkrecall"])
		}
		var layer map[string]string
		_ = json.Unmarshal(flagSettings(t, argv[i])["env"], &layer)
		if layer["RTK_DB_PATH"] != "/dev/null/iterion-rtk-history.db" || layer["RTK_RECALL"] != "0" {
			t.Errorf("spawn #%d's flag settings layer lacks the run env (env=%v): a settings env would put rtk's history back on", i+1, layer)
		}
	}
}

// With background work on, the system prompt says what this session waits
// for — a background subagent, never a background command or a monitor —
// where the CLI's own guidance says every background task notifies the agent
// when it completes. With the lifecycle off, nothing is waited for, and the
// section says that. A spawn without the subagent tool carries it too: a
// background shell needs none.
func TestBackgroundWorkOnSaysWhatTheSessionWaitsFor(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_TASKS", "on")
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE", "")
	const heading = "## Background work in this session"
	task := Task{NodeID: "n", SystemPrompt: "author"}
	for _, withheld := range []string{"", "1"} {
		t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", withheld)
		if claudeKeepsSubagents(task) == (withheld == "1") {
			t.Fatalf("scenario broken: DISALLOW_ORCHESTRATION_TOOLS=%q, subagents kept %v", withheld, claudeKeepsSubagents(task))
		}
		prompt := claudeCodeSystemPrompt(task)
		for _, want := range []string{heading, "A background subagent is waited for", "`run_in_background`", "is killed", "do not end your turn to wait for a background command"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("subagents kept %v: the prompt does not say %q:\n%s", withheld == "", want, prompt)
			}
		}
		if strings.Contains(prompt, subagentRuleHeading) {
			t.Errorf("subagents kept %v: the prompt says subagents run in the foreground while background work is on", withheld == "")
		}
	}
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE", "off")
	prompt := claudeCodeSystemPrompt(task)
	if !strings.Contains(prompt, heading) || !strings.Contains(prompt, "nothing that runs in the background is waited for") {
		t.Errorf("lifecycle off: the prompt does not say nothing is waited for:\n%s", prompt)
	}
	if strings.Contains(prompt, "A background subagent is waited for") {
		t.Errorf("lifecycle off: the prompt promises a background subagent is waited for:\n%s", prompt)
	}
}

// The background lifecycle reads the CLI's own signals — its session state,
// the tasks still running, no idle exit of its own. A user's or a
// repository's settings env could switch them off and leave the lifecycle
// waiting for an idle the CLI never reports, so every spawn pins them in both
// layers while the lifecycle is on, over the test process's own values.
func TestTheLifecycleSignalsArePinnedInBothLayers(t *testing.T) {
	pinWatchdogs(t)
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	t.Setenv("CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS", "0")
	t.Setenv("CLAUDE_CODE_BG_TASKS_REPORT_RUNNING", "0")
	t.Setenv("CLAUDE_CODE_EXIT_AFTER_STOP_DELAY", "1")
	task := Task{NodeID: "n", OutputSchema: []byte(schemaOK)}
	argv, env := spawnArgvEnv(t, task)
	if len(argv) < 2 {
		t.Fatalf("expected the Session spawn and at least one formatting pass, got %d spawn(s)", len(argv))
	}
	want := map[string]string{"sse": "1", "rr": "1", "exitdelay": ""}
	for i := range argv {
		for k, v := range want {
			if got, ok := env[i][k]; !ok || got != v {
				t.Errorf("spawn #%d ran with %s=%q (present %v), want %q", i+1, k, got, ok, v)
			}
		}
		var layer map[string]string
		_ = json.Unmarshal(flagSettings(t, argv[i])["env"], &layer)
		for k, v := range bgLifecycleEnv {
			if got, ok := layer[k]; !ok || got != v {
				t.Errorf("spawn #%d's flag settings layer has %s=%q (present %v), want %q: a settings env would blind the lifecycle", i+1, k, got, ok, v)
			}
		}
	}
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE", "off")
	pins, _ := claudeSpawnPins(task)
	for k := range bgLifecycleEnv {
		if _, ok := pins[k]; ok {
			t.Errorf("the lifecycle is off, yet %s is pinned", k)
		}
	}
}

// A rewriter's run_env goes under the pins: one naming a pinned variable moves
// none of them, in either layer.
func TestARewritersRunEnvMovesNoPin(t *testing.T) {
	pinWatchdogs(t)
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	rw := filepath.Join(t.TempDir(), "fakerw")
	if err := os.WriteFile(rw, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := Task{NodeID: "n", OutputSchema: []byte(schemaOK),
		Rewriters: []plugin.RewriterSpec{{ID: "rw", Locate: plugin.LocateSpec{Paths: []string{rw}},
			Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}},
			RunEnv: map[string]string{backgroundTasksOffEnv: "", bashMaxTimeoutEnv: "1", "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS": "0", "RW_OWN": "kept"}}}}
	env, settings := claudeSpawnPins(task)
	var layer struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(settings, &layer); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]map[string]string{"process": env, "flag layer": layer.Env} {
		if got[backgroundTasksOffEnv] != "1" || got[bashMaxTimeoutEnv] != wantBashMaxMs || got["CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS"] != "1" {
			t.Errorf("%s: a run_env moved a pin: %s=%q %s=%q EMIT_SESSION_STATE_EVENTS=%q", name, backgroundTasksOffEnv, got[backgroundTasksOffEnv], bashMaxTimeoutEnv, got[bashMaxTimeoutEnv], got["CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS"])
		}
		if got["RW_OWN"] != "kept" {
			t.Errorf("%s: the run_env's own variable is missing: RW_OWN=%q", name, got["RW_OWN"])
		}
	}
}

// The flag settings layer carries the pinned environment, a rewriter's
// run_env among it — resolved from its plugin's config, a `secret` field
// included. The spawn's log line names its keys and never prints a value.
func TestTheSpawnLogNamesTheSettingsAndPrintsNoValue(t *testing.T) {
	pinWatchdogs(t)
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	const secret = "rva19-sentinel-7f3a9c"
	rw := filepath.Join(t.TempDir(), "fakerw")
	if err := os.WriteFile(rw, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(fake, []byte(fakeClaudeArgv), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGV_LOG", filepath.Join(dir, "argv.log"))
	// The log's own output, every level: the logger's hook sees warnings only.
	var mu sync.Mutex
	var out strings.Builder
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return out.Write(p)
	}))}
	task := Task{NodeID: "n", Command: fake, WorkDir: dir, UserPrompt: "x", OutputSchema: []byte(schemaOK),
		Rewriters: []plugin.RewriterSpec{{ID: "rw", Locate: plugin.LocateSpec{Paths: []string{rw}},
			Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}},
			RunEnv: map[string]string{"RW_TOKEN": secret}}}}
	_, _ = b.Execute(context.Background(), task)
	argv := readSpawnLog(t, filepath.Join(dir, "argv.log"))
	if len(argv) == 0 || !strings.Contains(argv[0], secret) {
		t.Fatalf("scenario broken: the spawn's --settings does not carry the run_env value: %v", argv)
	}
	mu.Lock()
	lines := strings.Split(out.String(), "\n")
	mu.Unlock()
	named := false
	for _, l := range lines {
		if strings.Contains(l, secret) {
			t.Errorf("a log line prints a run_env value: %s", l)
		}
		if strings.Contains(l, "<redacted settings: ") && strings.Contains(l, "RW_TOKEN") {
			named = true
		}
	}
	if !named {
		t.Errorf("no spawn log line names the settings' env keys:\n%s", strings.Join(lines, "\n"))
	}
}

// With background work on and the lifecycle off, a session ends at its first
// result and kills the work still running: said, not silent.
func TestBackgroundTasksWithoutTheLifecycleIsWarned(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_TASKS", "on")
	t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE", "off")
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(fake, []byte(fakeClaudeArgv), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGV_LOG", filepath.Join(dir, "argv.log"))
	var mu sync.Mutex
	var warned []string
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
	b.Logger.SetHook(func(level iterlog.Level, msg string, _ map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		if level == iterlog.LevelWarn {
			warned = append(warned, msg)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := b.runSession(ctx, "go", Task{NodeID: "n"}, []claudesdk.Option{claudesdk.WithCLIPath(fake)}); err != nil {
		t.Fatalf("runSession: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, w := range warned {
		if strings.Contains(w, "ITERION_CLAUDE_CODE_BACKGROUND_TASKS=on") && strings.Contains(w, "LIFECYCLE off") {
			return
		}
	}
	t.Errorf("no warning for background work without the lifecycle; warnings: %q", warned)
}
