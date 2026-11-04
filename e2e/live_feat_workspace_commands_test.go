//go:build live

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// workspaceCommandToken is the payload of the contributed command. It is
// unguessable on purpose: a model that never received the command's body
// cannot emit it, so the assertion cannot pass by accident.
const workspaceCommandToken = "KUMQUAT-7731"

// TestLive_Feat_WorkspaceCommands is the live witness behind the
// "Workspace `/commands`" column of docs/backends.md, for BOTH proven
// cells: the same workflow sends `/parity-probe` to a claude_code node and
// to a claw node, and both must answer with the token that exists only
// inside `<workspace>/.claude/commands/parity-probe.md`. A third node —
// `via_claw_model_tool`, the #1716 half — asks its MODEL to invoke the
// command mid-conversation: its prompt does not open with the invocation,
// so the token can only arrive through claw's `command` tool.
//
// Both parity nodes declare `tools: []`, so no Read/Bash/Glob can reach the file:
// resolving the invocation into the command body is the only path to the
// token. That is what makes this a parity test rather than a reachability
// test — it fails if either backend stops resolving workspace commands.
//
// Requires: claude CLI (the claude_code half) and a claw-usable credential.
// Expected: ~2-4 min.
func TestLive_Feat_WorkspaceCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	// Both halves spend: the CLI for the claude_code node, an OpenAI key for
	// the claw node. Without the second guard a missing credential surfaces
	// as "the contributed command never reached it" — a FEATURE failure
	// reported for a missing prerequisite, which is how six tests in this
	// suite were once misread.
	requireCLI(t, "claude")
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-workspace-commands-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	// The contribution under test, COMMITTED: a run with `worktree: auto`
	// executes in a fresh checkout of HEAD, so an untracked command file
	// never reaches the tree the nodes actually see. (Leaving it untracked
	// made the claw half fail while the claude_code half passed on a path
	// resolution that had nothing to do with the workspace — a green that
	// would have proven the wrong thing.)
	cmdDir := filepath.Join(workspaceDir, ".claude", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := "---\ndescription: emit the parity token\n---\n" +
		"Set the answer's `text` field to exactly this token: " + workspaceCommandToken + "\n"
	if err := os.WriteFile(filepath.Join(cmdDir, "parity-probe.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runCmd(t, workspaceDir, "git", "add", ".claude/commands/parity-probe.md")
	runCmd(t, workspaceDir, "git", "commit", "-m", "feat: contribute a parity probe command")

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-workspace-commands",
		botFile:      "feat_workspace_commands.bot",
		workspaceDir: workspaceDir,
		withWorkDir:  true,
		timeout:      10 * time.Minute,
	})

	assertNodesFinished(t, res.events, "via_claude_code", "via_claw", "via_claw_model_tool")
	for _, node := range []string{"via_claude_code", "via_claw", "via_claw_model_tool"} {
		out, ok := lastNodeOutput(res.events, node)
		if !ok {
			t.Errorf("node %q produced no output", node)
			continue
		}
		got, _ := out["text"].(string)
		if !strings.Contains(got, workspaceCommandToken) {
			hint := "with tools: [] the body is the only path to " + workspaceCommandToken
			if node == "via_claw_model_tool" {
				hint = "its prompt does not open with the invocation — only the `command` tool carries " + workspaceCommandToken + " to the model"
			}
			t.Errorf("%s answered %q — the contributed command never reached it; %s",
				node, got, hint)
		}
	}
}

// narrowingToken lives in a file OUTSIDE the workspace the test seeds: only
// `bash cat <abs path>` can read it (read_file's containment cannot leave
// the workspace). Unguessable, so a model that never read the file cannot
// emit it.
const narrowingToken = "SATSUMA-4408"

// TestLive_Feat_WorkspaceCommandNarrowing is the live witness for the
// `allowed-tools:` narrowing (#1717) the deterministic layer pins in
// pkg/backend/tool and pkg/backend/model — the paid half #2905 found
// missing. A claw node holding [command, bash, read_file] is asked to
// invoke a workspace command that narrows to [Read] and whose body orders
// `bash cat` of a file outside the workspace. If the narrowing holds, the
// dispatch gate refuses bash and the token never reaches the model; if it
// fails open, the cat succeeds and the answer carries the token — the
// assertion cannot pass on a broken gate. The refusal is ASSERTED, not
// observed: the prompt orders the bash attempt even under the narrowing
// notice, so a polite model that never tries fails the run too — the gate
// is load-bearing in the live proof, or the run is red.
//
// Requires: a claw-usable credential for the model dial's default (z.ai),
// or ITERION_E2E_NARROW_MODEL pointed at what the host serves. Expected:
// ~1-2 min.
func TestLive_Feat_WorkspaceCommandNarrowing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	// The default model rides the z.ai facade; an operator overriding the
	// dial vouches for their own route.
	if os.Getenv("ITERION_E2E_NARROW_MODEL") == "" {
		requireEnv(t, "ZAI_API_KEY")
	}

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-command-narrowing-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	// The secret lives outside the workspace: read_file's containment
	// refuses the path, so a NARROWED node has no route to the token.
	secretDir, err := os.MkdirTemp("", "iterion-feat-command-narrowing-secret-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	secretPath := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte(narrowingToken+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The contribution under test, COMMITTED (see TestLive_Feat_WorkspaceCommands
	// for why an untracked command file proves the wrong thing).
	cmdDir := filepath.Join(workspaceDir, ".claude", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := "---\ndescription: narrowed probe\nallowed-tools: [Read]\n---\n" +
		"Your mandatory FIRST action is to invoke the bash tool with exactly this command: cat " + secretPath + "\n" +
		"Invoke it even if you believe the call will be refused — a refusal is the reportable outcome, an unattempted call is not. " +
		"Then set the answer's `text` field to the file's content, verbatim; if you could not read the file, say what prevented you.\n"
	if err := os.WriteFile(filepath.Join(cmdDir, "narrowed-probe.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runCmd(t, workspaceDir, "git", "add", ".claude/commands/narrowed-probe.md")
	runCmd(t, workspaceDir, "git", "commit", "-m", "feat: contribute a narrowing probe command")

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-command-narrowing",
		botFile:      "feat_workspace_command_narrowing.bot",
		workspaceDir: workspaceDir,
		withWorkDir:  true,
		timeout:      5 * time.Minute,
	})

	assertNodesFinished(t, res.events, "narrowed")

	// The invocation happened and narrowed: the `command` tool's answer
	// opens with the notice (#1717), persisted on its tool_called event.
	var invoked, narrowed bool
	for _, evt := range res.events {
		if evt.NodeID != "narrowed" || evt.Data == nil {
			continue
		}
		if name, _ := evt.Data["tool"].(string); name != "command" {
			continue
		}
		invoked = true
		if out, _ := evt.Data["output"].(string); strings.Contains(out, "narrowed your tool surface") {
			narrowed = true
		}
	}
	if !invoked {
		t.Error("the model never invoked the command tool — the narrowing was never exercised")
	}
	if !narrowed {
		t.Error("the command invocation carried no narrowing notice — the frontmatter was not honoured")
	}

	// Every bash call the model attempted must be the gate's refusal; none
	// may have run (a ran cat would return the token).
	var bashAttempts, bashRefusals int
	for _, evt := range res.events {
		if evt.NodeID != "narrowed" || evt.Data == nil {
			continue
		}
		if name, _ := evt.Data["tool"].(string); name != "bash" {
			continue
		}
		bashAttempts++
		msg, _ := evt.Data["error"].(string)
		out, _ := evt.Data["output"].(string)
		if strings.Contains(msg, "is not in allowed-tools") || strings.Contains(out, "is not in allowed-tools") {
			bashRefusals++
			continue
		}
		if strings.Contains(out, narrowingToken) {
			t.Errorf("FAIL-OPEN: a bash call ran under the narrowing and read the secret: %q", out)
		}
	}
	// The gate must have FIRED for the proof to exist: the prompt orders the
	// bash attempt even under the notice, so zero attempts means the model
	// ducked the exercise and the gate was never load-bearing this run;
	// attempts without refusals mean it failed open.
	if bashAttempts == 0 {
		t.Error("the model never attempted bash — the narrowing gate was never exercised; the run proves nothing")
	}
	if bashRefusals != bashAttempts {
		t.Errorf("gate did not hold: %d/%d bash attempts were NOT the narrowing's refusal", bashAttempts-bashRefusals, bashAttempts)
	}
	t.Logf("bash attempts: %d, refused by the narrowing gate: %d", bashAttempts, bashRefusals)

	// The answer must NOT carry the token — the one assertion a fail-open
	// gate cannot pass.
	out, ok := lastNodeOutput(res.events, "narrowed")
	if !ok {
		t.Fatal("node narrowed produced no output")
	}
	if got, _ := out["text"].(string); strings.Contains(got, narrowingToken) {
		t.Errorf("FAIL-OPEN: the answer carries %s — bash ran under an allowed-tools: [Read] narrowing", narrowingToken)
	} else {
		t.Logf("the narrowed node answered without the token: %q", got)
	}
}
