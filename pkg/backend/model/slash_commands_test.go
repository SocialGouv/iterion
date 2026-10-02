package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

const probeBody = "Reply with exactly this token and nothing else: KUMQUAT-7731"

// commandWorkspace builds a workspace carrying one `.claude/commands/`
// command and returns its root.
func commandWorkspace(t *testing.T, rel, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "commands", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// commandExecutor is an executor whose single prompt is `promptBody`, rooted
// at workDir.
func commandExecutor(workDir, promptBody string) *ClawExecutor {
	return &ClawExecutor{
		logger:  iterlog.Nop(),
		workDir: workDir,
		prompts: map[string]*ir.Prompt{"u": {Name: "u", Body: promptBody}},
	}
}

func promptParts(t *testing.T, e *ClawExecutor, input map[string]any, backend string) (string, []delegate.ContentBlock) {
	t.Helper()
	text, content := e.buildUserPromptParts(context.Background(), backendFields{id: "n", userPrompt: "u"}, input, nil, backend, &nodeBuildSession{})
	return text, content
}

// The capability itself: a `claw` node's `/name` prompt becomes the command
// body, the way claude_code gets it from --setting-sources project.
//
// Mutation: delete the expandWorkspaceSlashCommand call from
// buildUserPromptParts — the node sends the literal "/probe-secret", which
// is precisely the measured defect this change closes.
func TestBuildUserPromptPartsExpandsWorkspaceCommandForClaw(t *testing.T) {
	e := commandExecutor(commandWorkspace(t, "probe-secret.md", probeBody+"\n"), "/probe-secret")

	got, _ := promptParts(t, e, map[string]any{}, delegate.BackendClaw)
	if got != probeBody {
		t.Errorf("user prompt = %q, want the command body %q", got, probeBody)
	}
}

// claude_code expands these natively. Mutation: drop the backend gate and
// the CLI receives a body where it expects an invocation — the command runs
// twice or not at all.
func TestBuildUserPromptPartsLeavesClaudeCodeInvocationAlone(t *testing.T) {
	e := commandExecutor(commandWorkspace(t, "probe-secret.md", probeBody+"\n"), "/probe-secret")

	for _, backend := range []string{delegate.BackendClaudeCode, delegate.BackendPi, delegate.BackendCodex} {
		t.Run(backend, func(t *testing.T) {
			got, _ := promptParts(t, e, map[string]any{}, backend)
			if got != "/probe-secret" {
				t.Errorf("user prompt = %q, want the invocation untouched", got)
			}
		})
	}
}

// Turn 2 of a node that paused for ask_user: prependPriorAskUser puts the
// prior interaction FIRST, so the prompt no longer opens with the slash.
// Expanding after it would silently stop working on the second turn.
//
// Mutation: move the expansion below the prependPriorAskUser call — the
// command body disappears and the literal "/probe-secret" travels instead.
func TestBuildUserPromptPartsExpandsBeforeThePriorAskUserPrepend(t *testing.T) {
	e := commandExecutor(commandWorkspace(t, "probe-secret.md", probeBody+"\n"), "/probe-secret")
	input := map[string]any{
		delegate.PriorAskUserQuestionKey: "which branch?",
		delegate.PriorAskUserAnswerKey:   "main",
	}

	got, _ := promptParts(t, e, input, delegate.BackendClaw)
	if !strings.Contains(got, probeBody) {
		t.Errorf("resumed prompt lost the command body: %q", got)
	}
	if strings.Contains(got, "/probe-secret") {
		t.Errorf("resumed prompt still carries the raw invocation: %q", got)
	}
	if !strings.Contains(got, "[PRIOR INTERACTION]") {
		t.Errorf("resumed prompt lost the ask_user reminder: %q", got)
	}
}

// A prompt can both invoke a workspace command and reference an image
// attachment (`/analyze {{attachments.shot}}`). The blocks were split around
// the INVOCATION, so their text is what the substitution replaces — but the
// image bytes are the operator's attachment, and claude_code keeps those.
// Dropping the blocks wholesale sent a `tools: []` node an image PATH it
// could not read, and said nothing.
//
// Mutation: go back to `userContent = nil` on a hit — the image block
// disappears and this reddens on its own assertion.
func TestBuildUserPromptPartsKeepsTheInlinedImageOnACommandHit(t *testing.T) {
	ws := commandWorkspace(t, "probe-secret.md", probeBody+"\n")
	e := commandExecutor(ws, "/probe-secret {{attachments.shot}}")
	e.imageAttachs = map[string]bool{"shot": true}

	// A 1x1 PNG, so imageContentBlock really inlines bytes and the
	// multimodal branch really fires — a fixture whose blocks came back
	// empty would prove nothing about what a command hit keeps.
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	shot := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(shot, png, 0o644); err != nil {
		t.Fatal(err)
	}
	td := &TemplateData{Attachments: map[string]AttachmentInfo{
		"shot": {Name: "shot", Path: shot, HostPath: shot, MIME: "image/png"},
	}}

	// The control: this prompt DOES produce an image block, so the
	// assertions below are about the command hit and not an inert fixture.
	_, before := e.buildUserContent("u", map[string]any{}, td, e.imageAttachs)
	if countBlocks(before, "image") == 0 {
		t.Fatal("fixture is inert: buildUserContent produced no image block")
	}

	got, content := e.buildUserPromptParts(context.Background(),
		backendFields{id: "n", userPrompt: "u"}, map[string]any{}, td, delegate.BackendClaw, &nodeBuildSession{})

	if n := countBlocks(content, "image"); n != countBlocks(before, "image") {
		t.Errorf("image blocks after the substitution = %d, want %d — the attachment was dropped",
			n, countBlocks(before, "image"))
	}
	if n := countBlocks(content, "text"); n != 1 {
		t.Errorf("text blocks = %d, want exactly the substituted body", n)
	}
	// The instruction leads: the body first, then the bytes it talks about.
	// Mutation: append the text block last and this reddens.
	if len(content) == 0 || content[0].Type != "text" {
		t.Errorf("blocks = %v, want the substituted body first", content)
	}
	for _, b := range content {
		if b.Type == "text" && b.Text != got {
			t.Errorf("text block = %q, want the expanded prompt %q", b.Text, got)
		}
		if b.Type == "image" && b.Data == "" && b.URL == "" {
			t.Error("the image block lost its bytes")
		}
	}
	if !strings.HasPrefix(got, probeBody) {
		t.Errorf("user prompt = %q, want it to open with the command body", got)
	}
	// The attachment reference was the command's ARGUMENT, so its path
	// survives as text too — the agent can still name the file.
	if !strings.Contains(got, shot) {
		t.Errorf("user prompt = %q, want the attachment path kept as an argument", got)
	}
}

// countBlocks counts the content blocks of a given type.
func countBlocks(blocks []delegate.ContentBlock, typ string) int {
	n := 0
	for _, b := range blocks {
		if b.Type == typ {
			n++
		}
	}
	return n
}

// An unresolved name is not refused: claude_code answers an unknown command
// locally and reports success, and refusing here would break an ordinary
// prompt that merely opens with a slash-shaped word.
//
// Mutation: return an error on !found — "/tmp is full, clean it up" turns a
// working node into a hard failure.
func TestExpandWorkspaceSlashCommandPassesAnUnknownNameThrough(t *testing.T) {
	ws := commandWorkspace(t, "present.md", "body\n")

	for _, prompt := range []string{
		"/absent",
		"/tmp is full, clean it up and report",
		"/status",
	} {
		got, hit := expandWorkspaceSlashCommand(prompt, ws, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, &nodeBuildSession{})
		if hit || got != prompt {
			t.Errorf("expand(%q) = (%q, %v), want the text unchanged", prompt, got, hit)
		}
	}
}

// A prose prompt opening with a filesystem path is not an invocation at all.
// Mutation: widen the name charset and "/usr/bin/foo is broken" resolves as
// the command "usr".
func TestExpandWorkspaceSlashCommandIgnoresAProsePathPrompt(t *testing.T) {
	ws := commandWorkspace(t, "usr.md", "SHOULD NOT BE USED\n")

	got, hit := expandWorkspaceSlashCommand("/usr/bin/foo is broken, fix it", ws, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, &nodeBuildSession{})
	if hit {
		t.Fatal("a prose path prompt was expanded")
	}
	if got != "/usr/bin/foo is broken, fix it" {
		t.Errorf("prompt = %q, want it untouched", got)
	}
}

// The operator escape hatch restores the pre-capability behaviour, and it is
// read HOST-side — in the executor, not inside the sandboxed claw runner
// whose environment is an explicit allowlist.
//
// Mutation: ignore the env var and the switch does nothing.
func TestExpandWorkspaceSlashCommandHonoursTheEscapeHatch(t *testing.T) {
	ws := commandWorkspace(t, "probe-secret.md", probeBody+"\n")

	for _, off := range []string{"off", "0", "false", "OFF"} {
		t.Setenv(SlashCommandsEnv, off)
		got, hit := expandWorkspaceSlashCommand("/probe-secret", ws, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, &nodeBuildSession{})
		if hit || got != "/probe-secret" {
			t.Errorf("%s=%s: got (%q, %v), want the raw invocation", SlashCommandsEnv, off, got, hit)
		}
	}
	// The control: any other value leaves the capability on.
	t.Setenv(SlashCommandsEnv, "on")
	if got, hit := expandWorkspaceSlashCommand("/probe-secret", ws, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, &nodeBuildSession{}); !hit || got != probeBody {
		t.Errorf("on: got (%q, %v), want the expansion", got, hit)
	}
}

// A workspace with no .claude/commands/ at all never enters the path. The
// observable difference is the DIAGNOSTIC: with the fast path, nothing is
// logged, because there was no command directory to disappoint anyone. Drop
// the fast path and the same prompt produces a "does not define" warning
// naming a directory that does not exist.
//
// Mutation: delete the HasWorkspaceCommands branch — this reddens on the log
// count, which is the only thing that distinguishes the two.
func TestExpandWorkspaceSlashCommandSkipsAWorkspaceWithoutCommands(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)

	got, hit := expandWorkspaceSlashCommand("/anything", t.TempDir(), delegate.BackendClaw, "n", 0, logger, nil, &nodeBuildSession{})
	if hit || got != "/anything" {
		t.Errorf("= (%q, %v), want an untouched no-op", got, hit)
	}
	if buf.Len() != 0 {
		t.Errorf("a workspace with no commands directory logged %q, want silence", buf.String())
	}
}

// A namespaced command reaches its subdirectory, end to end through the
// executor seam.
func TestBuildUserPromptPartsResolvesANamespacedCommand(t *testing.T) {
	ws := commandWorkspace(t, filepath.Join("sub", "nested.md"), "Reply NESTED-4412\n")
	e := commandExecutor(ws, "/sub:nested")

	got, _ := promptParts(t, e, map[string]any{}, delegate.BackendClaw)
	if got != "Reply NESTED-4412" {
		t.Errorf("user prompt = %q, want the nested command body", got)
	}
}

// The arguments reach the body. Mutation: pass "" as args and a command
// written to take input silently receives none.
func TestBuildUserPromptPartsSubstitutesArguments(t *testing.T) {
	ws := commandWorkspace(t, "args.md", "ARGS=[$ARGUMENTS] ONE=[$1]\n")
	e := commandExecutor(ws, "/args hello world")

	got, _ := promptParts(t, e, map[string]any{}, delegate.BackendClaw)
	if got != "ARGS=[hello world] ONE=[hello]" {
		t.Errorf("user prompt = %q", got)
	}
}

// events.jsonl, the run log and the studio all read the OnLLMPrompt event.
// It fires just after buildUserPromptParts, so the prompt an operator can
// audit is the one the model actually received — including the command body
// a repo contributed.
//
// Mutation: expand inside the claw backend instead (where the first design
// put it) and the event keeps showing "/probe-secret" for a run that sent
// something else entirely, which is exactly the signature this change is
// supposed to retire.
func TestLLMPromptEventCarriesTheExpandedPrompt(t *testing.T) {
	e := commandExecutor(commandWorkspace(t, "probe-secret.md", probeBody+"\n"), "/probe-secret")
	var emitted string
	e.hooks.OnLLMPrompt = func(_, _, userMessage string) { emitted = userMessage }

	node := &ir.AgentNode{}
	node.ID = "n"
	if _, err := e.buildTask(context.Background(), node, backendFields{id: "n", userPrompt: "u", model: "anthropic/claude-sonnet-4-6"}, map[string]any{}, delegate.BackendClaw, nil); err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if emitted != probeBody {
		t.Errorf("OnLLMPrompt user message = %q, want the expanded body %q", emitted, probeBody)
	}
}

// A schema re-ask restarts the turn with the validation feedback appended to
// the task's prompt. Because the prompt is expanded BEFORE the task is built,
// the feedback lands on the command body and survives.
//
// Mutation: expand later (in the backend, from task.UserPrompt) and the
// restart prompt becomes "/probe-secret\n\n[OUTPUT SCHEMA VALIDATION
// FAILED]…", whose whole tail is then swallowed as the command's $ARGUMENTS —
// the model re-reads a byte-identical prompt and the retry budget burns on a
// no-op.
func TestSchemaReaskFeedbackLandsOnTheExpandedBody(t *testing.T) {
	e := commandExecutor(commandWorkspace(t, "probe-secret.md", probeBody+"\n"), "/probe-secret")

	node := &ir.AgentNode{}
	node.ID = "n"
	task, err := e.buildTask(context.Background(), node, backendFields{id: "n", userPrompt: "u", model: "anthropic/claude-sonnet-4-6"}, map[string]any{}, delegate.BackendClaw, nil)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	restart := appendSchemaRetryFeedback(task.UserPrompt, formatSchemaRetryFeedback(errors.New(`missing required field "text"`)))

	if !strings.Contains(restart, probeBody) {
		t.Errorf("restart prompt lost the command body: %q", restart)
	}
	if !strings.Contains(restart, schemaRetryFeedbackMarker) {
		t.Errorf("restart prompt lost its validation feedback: %q", restart)
	}
	if strings.HasPrefix(restart, "/") {
		t.Errorf("restart prompt still opens with an invocation, so the feedback would expand as $ARGUMENTS: %q", restart)
	}
}

// A command body that takes no argument placeholder must still receive the
// arguments. Claude Code 2.1.220 appends them (measured: "/echo-noargs
// HELLO-ZEBRA-9001 second-word" reaches the model with "\n\nARGUMENTS:
// HELLO-ZEBRA-9001 second-word"); dropping them would DELETE the operator's
// question, which is strictly worse than the literal prompt claw sent
// before this capability existed.
//
// Mutation: drop the append — "/review the auth module, focus on session
// fixation" reaches the model as "Review the code and report." and the node
// has no idea what it was asked to review.
func TestExpandWorkspaceSlashCommandKeepsArgumentsABodyDoesNotConsume(t *testing.T) {
	ws := commandWorkspace(t, "review.md", "Review the code and report.\n")

	got, hit := expandWorkspaceSlashCommand(
		"/review the auth module, focus on session fixation", ws, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, &nodeBuildSession{})
	if !hit {
		t.Fatal("the command did not resolve")
	}
	if !strings.Contains(got, "the auth module, focus on session fixation") {
		t.Errorf("expanded prompt = %q, want it to keep the operator's arguments", got)
	}
	if !strings.HasPrefix(got, "Review the code and report.") {
		t.Errorf("expanded prompt = %q, want it to open with the command body", got)
	}
	// The exact shape is parity, not decoration: the CLI appends
	// "\n\nARGUMENTS: <args>" verbatim (measured on 2.1.220).
	if !strings.HasSuffix(got, "\n\nARGUMENTS: the auth module, focus on session fixation") {
		t.Errorf("expanded prompt = %q, want it to end with the CLI's ARGUMENTS block", got)
	}
}

// The arms of the same class: a body carrying a placeholder that LOOKS like
// it takes an argument and does not. `$0` is not substituted here (the
// reading is one-based) and an out-of-range `$N` stays literal — so the
// arguments must be appended, or they are deleted exactly as if no
// placeholder had been written.
//
// Mutation: decide the append from a predicate over the BODY instead of from
// what Expand did, and every one of these drops the operator's message.
func TestExpandWorkspaceSlashCommandKeepsArgumentsALookalikePlaceholderDoesNotTake(t *testing.T) {
	for _, body := range []string{
		"Handle the request: $0\n", // $0 is not a one-based index
		"Third is $3.\n",           // out of range for one argument
		"A price of $100.\n",       // never a placeholder at all
		"An id like $1_suffix.\n",  // a word prefix, not a placeholder
	} {
		ws := commandWorkspace(t, "c.md", body)
		// ONE argument, so $3 is genuinely out of range: with three words
		// it would substitute legitimately and prove nothing.
		got, hit := expandWorkspaceSlashCommand("/c auth-module", ws, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, &nodeBuildSession{})
		if !hit {
			t.Fatalf("%q did not resolve", body)
		}
		if !strings.Contains(got, "auth-module") {
			t.Errorf("body %q gave %q — the operator's message was deleted", body, got)
		}
	}
}

// …and a body that DOES consume them is not given them twice.
// Mutation: append unconditionally and every argument appears in the prompt
// two times, once substituted and once tacked on.
func TestExpandWorkspaceSlashCommandDoesNotDuplicateConsumedArguments(t *testing.T) {
	for _, body := range []string{"ARGS=[$ARGUMENTS]\n", "FIRST=[$1]\n"} {
		ws := commandWorkspace(t, "c.md", body)
		got, hit := expandWorkspaceSlashCommand("/c alpha", ws, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, &nodeBuildSession{})
		if !hit {
			t.Fatalf("%q did not resolve", body)
		}
		if strings.Count(got, "alpha") != 1 {
			t.Errorf("body %q gave %q — the argument appears %d times, want once",
				body, got, strings.Count(got, "alpha"))
		}
	}
}

// An empty (or frontmatter-only) command file resolves as FOUND with an
// empty body. Substituting it would send an empty prompt — the same failure
// this capability exists to remove, with the text destroyed instead of
// echoed.
//
// Mutation: drop the emptiness guard and the node's prompt becomes "".
func TestExpandWorkspaceSlashCommandRefusesToSendAnEmptyBody(t *testing.T) {
	for name, content := range map[string]string{
		"empty":  "",
		"blank":  "\n\n   \n",
		"fmonly": "---\ndescription: nothing else\n---\n",
	} {
		ws := commandWorkspace(t, name+".md", content)
		// Both forms: bare, and WITH arguments — appending the arguments
		// first would make an empty body look non-empty, and the model would
		// get a bare "ARGUMENTS: …" with no instruction at all.
		for _, prompt := range []string{"/" + name, "/" + name + " please do the thing"} {
			got, hit := expandWorkspaceSlashCommand(prompt, ws, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, &nodeBuildSession{})
			if hit || got != prompt {
				t.Errorf("%s: = (%q, %v), want the prompt left alone", prompt, got, hit)
			}
		}
	}
}

// A command file that exists but cannot be read must not fail the node: the
// prompt may well be prose that merely opens with a slash-shaped word.
//
// Mutation: return the error and let buildTask propagate it — an unreadable
// file in the workspace kills a run whose prompt was never a command.
func TestExpandWorkspaceSlashCommandPassesThroughAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file regardless")
	}
	ws := commandWorkspace(t, "tmp.md", "body\n")
	if err := os.Chmod(filepath.Join(ws, ".claude", "commands", "tmp.md"), 0o000); err != nil {
		t.Fatal(err)
	}
	prompt := "/tmp is full, clean it up and report"
	got, hit := expandWorkspaceSlashCommand(prompt, ws, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, &nodeBuildSession{})
	if hit || got != prompt {
		t.Errorf("= (%q, %v), want the prose prompt to survive", got, hit)
	}
}

// Every diagnostic this capability emits has to reach the operator, and the
// studio scopes its per-node Logs tab on the "[<node>#<iter>/" prefix that
// every other line in this package uses. A line tagged "[<node>/claw]"
// matches neither that prefix nor its "[<node>#" fallback, so the whole
// diagnostic story for a silent prompt substitution would be invisible
// exactly where an operator looks for it.
//
// Mutation: drop the "#%d" from the tag — all four lines stop matching.
func TestSlashCommandDiagnosticsCarryTheStudioNodePrefix(t *testing.T) {
	var buf bytes.Buffer
	// LevelInfo, not Debug: a diagnostic demoted to Debug is invisible in
	// production, and a Debug-level test logger would not notice.
	logger := iterlog.New(iterlog.LevelInfo, &buf)

	ws := commandWorkspace(t, "dyn.md", "run !`git status` and use $1\n")
	writeExtraCommand(t, ws, "empty.md", "")
	for _, prompt := range []string{"/dyn alpha", "/absent", "/empty"} {
		expandWorkspaceSlashCommand(prompt, ws, delegate.BackendClaw, "mynode", 3, logger, nil, &nodeBuildSession{})
	}
	out := buf.String()

	for _, want := range []string{
		"📎 workspace command /dyn", // the substitution itself
		"/absent, which",           // the unresolved name
		"/empty",                   // the empty body
		"/dyn uses",                // the divergence notice…
		"!`…`",                     // …naming the form
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostics are missing %q:\n%s", want, out)
		}
	}
	// The studio scopes its per-node Logs tab on "[<node>#<iteration>/" and
	// falls back to "[<node>#" only when that set is EMPTY — so a fixed 0
	// disappears the moment the node runs inside a loop.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "mynode") {
			continue
		}
		if !strings.Contains(line, "[mynode#3/claw]") {
			t.Errorf("diagnostic is invisible in the studio's per-node filter at iteration 3: %q", line)
		}
	}
}

// writeExtraCommand adds another command file to an existing workspace.
func writeExtraCommand(t *testing.T, ws, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, ".claude", "commands", rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The router builds its own task, outside buildUserPromptParts. Round 1
// showed the router half of this change had NO coverage: reverting it left
// the whole package green.
//
// Mutation: send the raw userText from the router's assemble closure — this
// reddens, and nothing else does.
func TestLLMRouterExpandsWorkspaceCommandForClawOnly(t *testing.T) {
	ws := commandWorkspace(t, "route.md", "Pick the route that fits the change.\n")

	for _, tc := range []struct {
		backend string
		want    string
	}{
		{delegate.BackendClaw, "Pick the route that fits the change."},
		{delegate.BackendClaudeCode, "/route"},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			captured := &capturingBackend{results: []delegate.Result{
				{Output: map[string]any{"selected_route": "a", "reasoning": "r"}},
			}}
			reg := delegate.NewRegistry()
			reg.Register(tc.backend, captured)
			wf := &ir.Workflow{
				Prompts: map[string]*ir.Prompt{"usr": {Body: "/route"}},
				Schemas: map[string]*ir.Schema{},
			}
			e := NewClawExecutor(NewRegistry(), wf,
				WithBackendRegistry(reg), WithWorkDir(ws), WithLogger(iterlog.Nop()))

			node := &ir.RouterNode{
				BaseNode:   ir.BaseNode{ID: "r"},
				LLMFields:  ir.LLMFields{Backend: tc.backend, UserPrompt: "usr", Model: "anthropic/claude-sonnet-4-6"},
				RouterMode: ir.RouterLLM,
			}
			if _, err := e.executeLLMRouterUnified(context.Background(),
				node, map[string]any{"_route_candidates": []string{"a", "b"}}); err != nil {
				t.Fatalf("router: %v", err)
			}
			if len(captured.tasks) != 1 {
				t.Fatalf("backend saw %d tasks, want 1", len(captured.tasks))
			}
			if got := captured.tasks[0].UserPrompt; got != tc.want {
				t.Errorf("router task UserPrompt = %q, want %q", got, tc.want)
			}
		})
	}
}

// The iteration in the diagnostics has to come from the RUN, not from a
// literal a test hands in. Both production call sites take it from the
// context; a test that only calls the helper directly exercises the site
// and leaves the wiring unwitnessed — mutating either call site back to a
// fixed 0 stayed green until this test existed.
//
// Mutation: replace LoopIterationFromContext(ctx) with 0 in
// executor_build_task.go or in executor_router.go — one subtest each.
func TestSlashCommandDiagnosticsTakeTheIterationFromTheRun(t *testing.T) {
	const iter = 4
	ws := commandWorkspace(t, "dyn.md", "run !`git status` now\n")

	t.Run("agent path", func(t *testing.T) {
		var buf bytes.Buffer
		e := commandExecutor(ws, "/dyn")
		e.logger = iterlog.New(iterlog.LevelInfo, &buf)
		ctx := WithLoopIteration(context.Background(), iter)

		e.buildUserPromptParts(ctx, backendFields{id: "n", userPrompt: "u"}, map[string]any{}, nil, delegate.BackendClaw, &nodeBuildSession{})
		assertTaggedWithIteration(t, buf.String(), "n", iter)
	})

	t.Run("router path", func(t *testing.T) {
		var buf bytes.Buffer
		captured := &capturingBackend{results: []delegate.Result{
			{Output: map[string]any{"selected_route": "a", "reasoning": "r"}},
		}}
		reg := delegate.NewRegistry()
		reg.Register(delegate.BackendClaw, captured)
		wf := &ir.Workflow{
			Prompts: map[string]*ir.Prompt{"usr": {Body: "/dyn"}},
			Schemas: map[string]*ir.Schema{},
		}
		e := NewClawExecutor(NewRegistry(), wf,
			WithBackendRegistry(reg), WithWorkDir(ws), WithLogger(iterlog.New(iterlog.LevelInfo, &buf)))
		node := &ir.RouterNode{
			BaseNode:   ir.BaseNode{ID: "r"},
			LLMFields:  ir.LLMFields{Backend: delegate.BackendClaw, UserPrompt: "usr", Model: "anthropic/claude-sonnet-4-6"},
			RouterMode: ir.RouterLLM,
		}
		ctx := WithLoopIteration(context.Background(), iter)
		if _, err := e.executeLLMRouterUnified(ctx, node, map[string]any{"_route_candidates": []string{"a", "b"}}); err != nil {
			t.Fatalf("router: %v", err)
		}
		assertTaggedWithIteration(t, buf.String(), "r", iter)
	})
}

// assertTaggedWithIteration requires every slash-command line for nodeID to
// carry the run's iteration, and requires there to be some.
func assertTaggedWithIteration(t *testing.T, out, nodeID string, iter int) {
	t.Helper()
	want := fmt.Sprintf("[%s#%d/claw]", nodeID, iter)
	seen := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "workspace command") && !strings.Contains(line, "uses ") {
			continue
		}
		seen++
		if !strings.Contains(line, want) {
			t.Errorf("diagnostic does not carry the run's iteration (want %s): %q", want, line)
		}
	}
	if seen == 0 {
		t.Errorf("no slash-command diagnostic was emitted at all:\n%s", out)
	}
}

// On a review run the workspace is a checkout the run does not control, so a
// command body is untrusted input that turns straight into a BILLED request.
// The ceiling abstains rather than truncates: half a command body is an
// instruction nobody wrote.
//
// Mutation: drop the ceiling and a one-megabyte file committed by the
// repository under review becomes the node's prompt.
func TestExpandWorkspaceSlashCommandRefusesAnOversizedBody(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	big := strings.Repeat("A", defaultSlashCommandMaxBytes+1)
	ws := commandWorkspace(t, "huge.md", big)

	got, hit := expandWorkspaceSlashCommand("/huge", ws, delegate.BackendClaw, "n", 0, logger, nil, &nodeBuildSession{})
	if hit || got != "/huge" {
		t.Errorf("= (%d bytes, %v), want the prompt left unchanged", len(got), hit)
	}
	out := buf.String()
	for _, want := range []string{"/huge", "larger than the", SlashCommandMaxBytesEnv} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, out)
		}
	}
	// Never a truncation: nothing of the body may reach the prompt.
	if strings.Contains(got, "AAAA") {
		t.Error("the oversized body leaked into the prompt")
	}

	// The control: one byte under the ceiling still expands, so the bound
	// bounds the abuse and not the use.
	okWS := commandWorkspace(t, "fits.md", strings.Repeat("B", defaultSlashCommandMaxBytes-1))
	if _, hit := expandWorkspaceSlashCommand("/fits", okWS, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, &nodeBuildSession{}); !hit {
		t.Error("a body one byte under the ceiling was refused")
	}
}

// The operator can move the ceiling; a typo must not silently remove it.
func TestSlashCommandMaxBytesHonoursTheOverride(t *testing.T) {
	t.Setenv(SlashCommandMaxBytesEnv, "10")
	if got := slashCommandMaxBytes(); got != 10 {
		t.Errorf("slashCommandMaxBytes() = %d, want the override", got)
	}
	t.Setenv(SlashCommandMaxBytesEnv, "0")
	if got := slashCommandMaxBytes(); got != 0 {
		t.Errorf("slashCommandMaxBytes() = %d, want the ceiling removed", got)
	}
	for _, bad := range []string{"lots", "-5", "1e6"} {
		t.Setenv(SlashCommandMaxBytesEnv, bad)
		if got := slashCommandMaxBytes(); got != defaultSlashCommandMaxBytes {
			t.Errorf("%s=%q gave %d, want the default kept — a typo must not remove the bound",
				SlashCommandMaxBytesEnv, bad, got)
		}
	}
}

// The audit trail must carry what was SENT. On the router path the prompt
// event fires before any backend is chosen, so it carries the invocation;
// when claw substitutes a command, the event has to be re-emitted with the
// body claw actually received. A log line is not an audit trail —
// `events.jsonl`, `iterion report` and the studio all read this event.
//
// Mutation: drop the re-emission in the router's assemble — the recorded
// prompt stays `/route` while claw receives the body, and this reddens.
func TestLLMRouterRecordsThePromptItActuallySent(t *testing.T) {
	ws := commandWorkspace(t, "route.md", "Pick the route that fits the change.\n")

	for _, tc := range []struct{ backend, wantLast string }{
		{delegate.BackendClaw, "Pick the route that fits the change."},
		{delegate.BackendClaudeCode, "/route"},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			captured := &capturingBackend{results: []delegate.Result{
				{Output: map[string]any{"selected_route": "a", "reasoning": "r"}},
			}}
			reg := delegate.NewRegistry()
			reg.Register(tc.backend, captured)
			wf := &ir.Workflow{
				Prompts: map[string]*ir.Prompt{"usr": {Body: "/route"}},
				Schemas: map[string]*ir.Schema{},
			}
			var prompts []string
			e := NewClawExecutor(NewRegistry(), wf,
				WithBackendRegistry(reg), WithWorkDir(ws), WithLogger(iterlog.Nop()),
				WithEventHooks(EventHooks{
					OnLLMPrompt: func(_, _, userMessage string) { prompts = append(prompts, userMessage) },
				}))
			node := &ir.RouterNode{
				BaseNode:   ir.BaseNode{ID: "r"},
				LLMFields:  ir.LLMFields{Backend: tc.backend, UserPrompt: "usr", Model: "anthropic/claude-sonnet-4-6"},
				RouterMode: ir.RouterLLM,
			}
			if _, err := e.executeLLMRouterUnified(context.Background(),
				node, map[string]any{"_route_candidates": []string{"a", "b"}}); err != nil {
				t.Fatalf("router: %v", err)
			}
			if len(prompts) == 0 {
				t.Fatal("no prompt event emitted")
			}
			// The LAST event is the one that describes what was sent.
			if got := prompts[len(prompts)-1]; got != tc.wantLast {
				t.Errorf("recorded prompt = %q, want %q (what the backend received)", got, tc.wantLast)
			}
			if got := captured.tasks[0].UserPrompt; got != prompts[len(prompts)-1] {
				t.Errorf("recorded %q but sent %q — the audit trail does not match the request",
					prompts[len(prompts)-1], got)
			}
			// ONE event per node, substitution or not: every reader that
			// starts an LLM step per `llm_prompt` (iterion inspect --node,
			// iterion report, the studio's LLM Trace) would otherwise render
			// a phantom step that never resolves.
			if len(prompts) != 1 {
				t.Errorf("%d prompt events for one call, want exactly 1: %q", len(prompts), prompts)
			}
		})
	}
}

// The body is the untrusted half AND it sets the multiplier: `$ARGUMENTS`
// repeated N times turns a body UNDER the ceiling into N times the
// arguments. A bound applied only before expansion is not a bound —
// measured, a 256 KiB body of `$ARGUMENTS` with a 4 KiB argument reaches
// 536 MB, which dies in the builder before anything is billed.
//
// Mutation: ignore the expander's typed error and this body sails through
// (dropping the post-APPEND check does NOT redden it — that guard has its
// own witness below, which is the point of having both).
func TestExpandWorkspaceSlashCommandRefusesABodyThatAMPLIFIESPastTheCeiling(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)

	// Under the ceiling on its own, far over it once expanded.
	repeats := 2000
	body := strings.Repeat("$ARGUMENTS ", repeats)
	if len(body) > defaultSlashCommandMaxBytes {
		t.Fatalf("fixture is inert: the body is %d bytes, already over the ceiling", len(body))
	}
	ws := commandWorkspace(t, "amp.md", body)
	args := strings.Repeat("x", 1024)

	got, hit := expandWorkspaceSlashCommand("/amp "+args, ws, delegate.BackendClaw, "n", 0, logger, nil, &nodeBuildSession{})
	if hit {
		t.Errorf("an amplifying body expanded to %d bytes and was accepted", len(got))
	}
	if !strings.HasPrefix(got, "/amp ") {
		t.Errorf("prompt = %.40q…, want it left unchanged", got)
	}
	out := buf.String()
	// The refusal comes from the EXPANDER (it aborted as it produced), so it
	// says "expands past" and cannot report a size — reporting one would
	// mean having materialised the thing.
	for _, want := range []string{"expands past", "/amp", SlashCommandMaxBytesEnv} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, out)
		}
	}
	// It must NOT report how big the expansion got: knowing that would mean
	// having materialised it, which is the defect this bound removes.
	if strings.Contains(out, "expands to ") || strings.Contains(out, "reached ") {
		t.Errorf("the refusal reports a size it could not have measured:\n%s", out)
	}
}

// A backend holding multimodal content builds its wire message from the
// BLOCKS alone, so the ask_user prefix has to reach them: added only to the
// text, the operator's answer never reaches the model and the node re-asks
// the same question — while the prompt event records it as sent.
//
// Mutation: prepend only to userText (the shape before this fix) and the
// blocks lose the answer.
func TestBuildUserPromptPartsCarriesThePriorAnswerIntoTheBlocks(t *testing.T) {
	ws := commandWorkspace(t, "analyze.md", "Describe the screenshot.\n")
	e := commandExecutor(ws, "/analyze {{attachments.shot}}")
	e.imageAttachs = map[string]bool{"shot": true}

	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	shot := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(shot, png, 0o644); err != nil {
		t.Fatal(err)
	}
	td := &TemplateData{Attachments: map[string]AttachmentInfo{
		"shot": {Name: "shot", Path: shot, HostPath: shot, MIME: "image/png"},
	}}
	input := map[string]any{
		delegate.PriorAskUserQuestionKey: "which branch?",
		delegate.PriorAskUserAnswerKey:   "release/4.2 — and keep the tag",
	}

	got, content := e.buildUserPromptParts(context.Background(),
		backendFields{id: "n", userPrompt: "u"}, input, td, delegate.BackendClaw, &nodeBuildSession{})

	if !strings.Contains(got, "release/4.2") {
		t.Fatalf("the text lost the prior answer: %q", got)
	}
	if countBlocks(content, "image") == 0 {
		t.Fatal("fixture is inert: no image block to carry anything alongside")
	}
	var blockText string
	for _, b := range content {
		if b.Type == "text" {
			blockText += b.Text
		}
	}
	if !strings.Contains(blockText, "release/4.2") {
		t.Errorf("the blocks lost the operator's answer — the model would re-ask.\nblocks: %q", blockText)
	}
	if !strings.Contains(blockText, "Describe the screenshot.") {
		t.Errorf("the blocks lost the command body: %q", blockText)
	}
}

// The third quantity: the arguments appended to a body that consumes none.
// They cannot amplify — they are the prompt's own bytes, appended once — so
// a length check is the right instrument here, unlike the expansion. Without
// its own witness the guard was dead weight nobody would have missed.
//
// Mutation: delete the post-append ceiling block and this reddens; nothing
// else in the package does.
func TestExpandWorkspaceSlashCommandRefusesWhenTheArgumentsPushItOver(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)

	// A body with no placeholder at all, just under the ceiling, so the
	// arguments are APPENDED rather than substituted.
	body := strings.Repeat("c", defaultSlashCommandMaxBytes-64)
	ws := commandWorkspace(t, "tail.md", body)
	args := strings.Repeat("d", 256) // pushes the total over

	got, hit := expandWorkspaceSlashCommand("/tail "+args, ws, delegate.BackendClaw, "n", 0, logger, nil, &nodeBuildSession{})
	if hit {
		t.Errorf("the appended arguments pushed it to %d bytes and it was accepted", len(got))
	}
	if !strings.HasPrefix(got, "/tail ") {
		t.Errorf("prompt = %.30q…, want it unchanged", got)
	}
	if out := buf.String(); !strings.Contains(out, "with its arguments") {
		t.Errorf("the refusal does not name the arguments as the cause:\n%s", out)
	}
}

// The frontmatter divergence is the one with a security consequence — a
// command that narrows itself to `allowed-tools: Read, Grep` runs here with
// the node's full set — and it was the only divergence with no runtime
// signal at all. Documenting it in bold is not a diagnostic.
//
// Mutation: drop the DiscardedFrontmatter warn block (or have claw surface
// an empty list) and the operator gets nothing.
func TestExpandWorkspaceSlashCommandWarnsAboutDiscardedFrontmatter(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	ws := commandWorkspace(t, "narrow.md",
		"---\ndescription: narrowed\nallowed-tools: Read, Grep\nmodel: claude-3-5-haiku\n---\nAudit the diff.\n")

	got, hit := expandWorkspaceSlashCommand("/narrow", ws, delegate.BackendClaw, "n", 0, logger, nil, &nodeBuildSession{})
	if !hit || got != "Audit the diff." {
		t.Fatalf("= (%q, %v), want the command body", got, hit)
	}
	out := buf.String()
	for _, want := range []string{"allowed-tools", "model", "#1717", "narrow"} {
		if !strings.Contains(out, want) {
			t.Errorf("the frontmatter warning does not name %q:\n%s", want, out)
		}
	}
	// It has to be visible at production level, like every other abstention.
	if !strings.Contains(out, "[n#0/claw]") {
		t.Errorf("the warning is invisible in the studio's per-node filter:\n%s", out)
	}

	// A description-only command owes no warning — a diagnostic that always
	// fires is one an author learns to ignore.
	var quiet bytes.Buffer
	plainWS := commandWorkspace(t, "plain.md", "---\ndescription: plain\n---\nJust do it.\n")
	if _, hit := expandWorkspaceSlashCommand("/plain", plainWS, delegate.BackendClaw, "n", 0,
		iterlog.New(iterlog.LevelInfo, &quiet), nil, &nodeBuildSession{}); !hit {
		t.Fatal("the plain command did not resolve")
	}
	if strings.Contains(quiet.String(), "frontmatter") {
		t.Errorf("a description-only command warned anyway:\n%s", quiet.String())
	}
}

// A `fallbacks:` route can cross backends (ADR-087), and the user prompt is
// now backend-dependent: a workspace command expands for claw and not for
// claude_code. So a node whose primary is claude_code and whose fallback is
// claw records the INVOCATION in its single llm_prompt and sends the BODY —
// the run succeeds and every reader (events.jsonl, iterion report, inspect
// --node, the studio LLM Trace) shows the primary's text.
//
// One llm_prompt per node stays the invariant, so the divergence travels as
// a FACT on the delegate event instead of as a second prompt — and it is
// computed against the element that SERVED, not against every element that
// ever differed.
//
// Mutation: drop the comparison in describeDivergence (or stop handing
// claimPrompt the text) and the divergence goes unreported while the two
// prompts still differ.
func TestCrossBackendFallbackReportsThePromptDivergence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		builds   [][2]string // {text, backend}, in build order
		want     bool
		wantOn   string
		wantNote string
	}{
		{
			name: "claude_code then claw: the server got the body, the event says the invocation",
			builds: [][2]string{
				{"/probe-secret", delegate.BackendClaudeCode},
				{probeBody, delegate.BackendClaw},
			},
			want: true, wantOn: delegate.BackendClaw,
		},
		{
			name: "single backend: nothing to diverge from",
			builds: [][2]string{
				{probeBody, delegate.BackendClaw},
			},
			want: false,
		},
		{
			name: "identical text on both elements",
			builds: [][2]string{
				{"same", delegate.BackendClaudeCode},
				{"same", delegate.BackendClaw},
			},
			want: false,
		},
		{
			// The false positive a sticky flag produces: the middle element
			// differed, the LAST one received exactly the recorded prompt.
			name: "claw, claude_code, claw: the server got the recorded prompt",
			builds: [][2]string{
				{probeBody, delegate.BackendClaw},
				{"/probe-secret", delegate.BackendClaudeCode},
				{probeBody, delegate.BackendClaw},
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := &nodeBuildSession{}
			for i, b := range tc.builds {
				claimed := sess.claimPrompt(b[0], b[1])
				if (i == 0) != claimed {
					t.Fatalf("build %d claimed=%v — exactly the first build owns the event", i, claimed)
				}
			}
			var di DelegateInfo
			sess.describeDivergence(&di)
			if di.PromptDiverged != tc.want {
				t.Errorf("PromptDiverged = %v, want %v (the recorded prompt %s the one that ran)",
					di.PromptDiverged, tc.want, map[bool]string{true: "is not", false: "is"}[tc.want])
			}
			if tc.want && di.PromptDivergedOn != tc.wantOn {
				t.Errorf("PromptDivergedOn = %q, want the backend that SERVED (%q)", di.PromptDivergedOn, tc.wantOn)
			}
		})
	}
}

// The router builds its own task outside buildUserPromptParts, so it needs
// the same build session or everything the session carries is lost there:
// `describeDivergence` becomes a no-op and the delegate event says nothing.
// A router chain cannot cross backends today (`resolveProviderChain` never
// sets `chainElement.Backend`), so the prompt divergence itself is not
// reachable from a router — which is exactly why this asserts through the
// session's OTHER payload, on the real router, rather than simulating the
// shape and proving nothing.
//
// Mutation: pass nil as the session from the router (its previous shape) —
// the delegate event loses the ignored frontmatter and this reddens.
func TestLLMRouterCarriesTheBuildSessionOntoItsDelegateEvent(t *testing.T) {
	ws := commandWorkspace(t, "route.md",
		"---\ndescription: routing\nallowed-tools: Read\n---\nPick the route that fits.\n")

	captured := &capturingBackend{results: []delegate.Result{
		{Output: map[string]any{"selected_route": "a", "reasoning": "r"}},
	}}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, captured)
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{"usr": {Body: "/route"}},
		Schemas: map[string]*ir.Schema{},
	}
	var finished []DelegateInfo
	e := NewClawExecutor(NewRegistry(), wf,
		WithBackendRegistry(reg), WithWorkDir(ws), WithLogger(iterlog.Nop()),
		WithEventHooks(EventHooks{
			OnDelegateFinished: func(_ string, di DelegateInfo) { finished = append(finished, di) },
		}))
	node := &ir.RouterNode{
		BaseNode:   ir.BaseNode{ID: "r"},
		LLMFields:  ir.LLMFields{Backend: delegate.BackendClaw, UserPrompt: "usr", Model: "anthropic/claude-sonnet-4-6"},
		RouterMode: ir.RouterLLM,
	}
	if _, err := e.executeLLMRouterUnified(context.Background(),
		node, map[string]any{"_route_candidates": []string{"a", "b"}}); err != nil {
		t.Fatalf("router: %v", err)
	}
	if len(finished) != 1 {
		t.Fatalf("%d delegate_finished events, want 1", len(finished))
	}
	if !slices.Contains(finished[0].CommandFrontmatterIgnored, "allowed-tools") {
		t.Errorf("the router's delegate event carries %v — the build session never reached it",
			finished[0].CommandFrontmatterIgnored)
	}
	// And the command really was substituted, so this is not a vacuous pass.
	if got := captured.tasks[0].UserPrompt; got != "Pick the route that fits." {
		t.Errorf("router task UserPrompt = %q, want the command body", got)
	}
}

// The frontmatter divergence is keyed per NODE and rides the EVENT: a second
// node invoking the same command file may grant a broader `tools:` set, so
// its exposure is its own, and a deterministic gate reads events.jsonl, not
// the log.
//
// Mutation: key the dedupe on the command file path (as the `$N` noise
// suppression does) and the second node is told nothing.
func TestFrontmatterDivergenceIsPerNodeAndOnTheEvent(t *testing.T) {
	ws := commandWorkspace(t, "narrow.md",
		"---\ndescription: narrowed\nallowed-tools: Read, Grep\n---\nAudit the diff.\n")
	once := &slashWarnOnce{} // shared across nodes, as the run-scoped one is

	var events []DelegateInfo
	for _, node := range []string{"first", "second"} {
		var buf bytes.Buffer
		sess := &nodeBuildSession{}
		if _, hit := expandWorkspaceSlashCommand("/narrow", ws, delegate.BackendClaw, node, 0,
			iterlog.New(iterlog.LevelInfo, &buf), once, sess); !hit {
			t.Fatalf("%s: the command did not resolve", node)
		}
		if !strings.Contains(buf.String(), "allowed-tools") {
			t.Errorf("%s: no frontmatter line — a second node with a broader tools: set gets no warning:\n%s",
				node, buf.String())
		}
		var di DelegateInfo
		sess.describeDivergence(&di)
		events = append(events, di)
	}
	for i, di := range events {
		if !slices.Contains(di.CommandFrontmatterIgnored, "allowed-tools") {
			t.Errorf("node %d: delegate event does not carry the ignored frontmatter (%v) — a gate reading events.jsonl cannot see it",
				i, di.CommandFrontmatterIgnored)
		}
	}

	// A command that declares only a description puts nothing on the event.
	plainWS := commandWorkspace(t, "plain.md", "---\ndescription: plain\n---\nJust do it.\n")
	sess := &nodeBuildSession{}
	expandWorkspaceSlashCommand("/plain", plainWS, delegate.BackendClaw, "n", 0, iterlog.Nop(), nil, sess)
	var di DelegateInfo
	sess.describeDivergence(&di)
	if len(di.CommandFrontmatterIgnored) != 0 {
		t.Errorf("a description-only command put %v on the event", di.CommandFrontmatterIgnored)
	}
}

// Every value a diagnostic interpolates from untrusted input is bounded by
// ONE helper: the command name is the whole whitespace-delimited token of a
// prompt that is routinely model- or repo-derived, and these lines fire per
// node per iteration into the process log AND the run log stream.
//
// Mutation: drop safeDiag from the unresolved-name branch and a 4 MB prompt
// token becomes a 4 MB log line.
func TestSlashCommandDiagnosticsAreBounded(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	ws := commandWorkspace(t, "present.md", "body\n")

	huge := strings.Repeat("z", 4<<20)
	got, hit := expandWorkspaceSlashCommand("/"+huge, ws, delegate.BackendClaw, "n", 0, logger, nil, &nodeBuildSession{})
	if hit || got != "/"+huge {
		t.Errorf("the prompt was not left unchanged")
	}
	if n := buf.Len(); n > 4096 {
		t.Errorf("the refusal logged %d bytes for a 4 MiB command name, want it bounded", n)
	}
	if !strings.Contains(buf.String(), "zzz") {
		t.Errorf("the refusal stopped naming the command at all:\n%s", buf.String())
	}
}

// A divergence notice is worth one line per command file per run. The
// divergence is REAL on every invocation — claw's `$1` is the first argument
// and the CLI's is the second — so suppressing it by "does it diverge" would
// suppress it always; suppressing the REPEAT is what keeps the other
// warnings their audience.
//
// Mutation: drop the once-guard and a node in a loop repeats the line every
// iteration.
func TestSlashCommandDivergenceWarnsOncePerCommand(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	once := &slashWarnOnce{}
	ws := commandWorkspace(t, "review.md", "Review $1 and report.\n")

	for i := range 3 {
		if _, hit := expandWorkspaceSlashCommand("/review alpha beta", ws,
			delegate.BackendClaw, "n", i, logger, once, &nodeBuildSession{}); !hit {
			t.Fatalf("iteration %d did not resolve", i)
		}
	}
	if n := strings.Count(buf.String(), "uses "); n != 1 {
		t.Errorf("the divergence warned %d times over 3 iterations, want once:\n%s", n, buf.String())
	}

	// A different command file gets its own line.
	writeExtraCommand(t, ws, "other.md", "Other $2 here.\n")
	if _, hit := expandWorkspaceSlashCommand("/other a b", ws, delegate.BackendClaw, "n", 0, logger, once, &nodeBuildSession{}); !hit {
		t.Fatal("the second command did not resolve")
	}
	if n := strings.Count(buf.String(), "uses "); n != 2 {
		t.Errorf("a second command file was suppressed too: %d lines", n)
	}
}

// os.Root refuses an ABSOLUTE symlink wherever it points — including inside
// the workspace — so the raw "path escapes from parent" would tell the
// operator the repository tried to escape when it did not. The refusal names
// the boundary instead, and keeps the raw error alongside.
func TestSlashCommandRefusalNamesTheContainmentBoundary(t *testing.T) {
	got := readFailureReason(errors.New("openat x.md: path escapes from parent"))
	for _, want := range []string{"containment boundary", "absolute symlink", "path escapes from parent"} {
		if !strings.Contains(got, want) {
			t.Errorf("readFailureReason = %q, want it to mention %q", got, want)
		}
	}
	// An ordinary failure is not dressed up as a containment refusal.
	if got := readFailureReason(errors.New("permission denied")); got != "permission denied" {
		t.Errorf("readFailureReason on a plain error = %q, want it unchanged", got)
	}
}
