package delegate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// fakeClaudeArgv is a stand-in CLI that records every argv it is spawned with
// and answers the minimum stream-json the SDK needs to finish a turn. Each
// spawn appends one newline-terminated record to $ARGV_LOG (newlines inside
// an option — an ultracode --append-system-prompt runs to 13 lines — are
// flattened first, or one spawn would be counted as fourteen), so a test can assert on
// BOTH passes of one Execute.
const fakeClaudeArgv = `#!/bin/sh
printf '%s' "$*" | tr '\n' ' ' >> "$ARGV_LOG"; printf '\n' >> "$ARGV_LOG"
case "$*" in *--input-format*) read -r _ ;; esac
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
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, io.Discard)}
	_, _ = b.Execute(context.Background(), task)

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
