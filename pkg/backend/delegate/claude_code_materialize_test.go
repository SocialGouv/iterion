package delegate

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// A secret goes into the command byte for byte, whatever it holds: swapped in
// the JSON text of the tool input, a backslash sequence, a quote or a newline
// came out altered (the command ran with the wrong credential, and no redactor
// recognised the altered value) or broke the document (the command ran with
// the placeholder).
func TestTheMaterializedSecretIsExact(t *testing.T) {
	const placeholder = "__ITERION_SECRET_API_KEY__"
	for _, secret := range []string{`Tr0ub4dor\/3xyz`, `pa\\ss-w0rd-9`, "line1\nline2-secret", `quo"te-secret`} {
		materialize := func(s string) string { return strings.ReplaceAll(s, placeholder, secret) }
		out, err := materializeSecretsHandler(materialize)(context.Background(), claudesdk.HookCallbackInput{
			ToolInput: map[string]any{
				"command": "curl -u admin:" + placeholder + " https://example.invalid",
				"env":     []any{"KEY=" + placeholder, 7},
			},
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if got := out.UpdatedInput["command"]; got != "curl -u admin:"+secret+" https://example.invalid" {
			t.Fatalf("secret %q: command = %q", secret, got)
		}
		if env, _ := out.UpdatedInput["env"].([]any); len(env) != 2 || env[0] != "KEY="+secret || env[1] != 7 {
			t.Fatalf("secret %q: env = %#v", secret, out.UpdatedInput["env"])
		}
	}
	out, _ := materializeSecretsHandler(func(s string) string { return s })(context.Background(), claudesdk.HookCallbackInput{ToolInput: map[string]any{"command": "ls"}})
	if out.Decision != "" || out.UpdatedInput != nil {
		t.Fatalf("a tool input with no placeholder was rewritten: %+v", out)
	}
}

// longestLeak returns the longest run (8 bytes or more) of secret found in out.
func longestLeak(out, secret string) string {
	best := ""
	for i := range len(secret) {
		for j := len(secret); j-i > len(best) && j-i >= 8; j-- {
			if strings.Contains(out, secret[i:j]) {
				best = secret[i:j]
				break
			}
		}
	}
	return best
}

// The run log cuts what it shows — a header at 100 bytes, a result body at
// 4096, a todo item at 200 — and a secret cut at a bound is no longer
// recognisable: every value is redacted before it is cut.
func TestTheRunLogRedactsBeforeItCuts(t *testing.T) {
	const secret = "Tr0ub4dor-and-3-horses-correct-battery-staple-9f8e7d6c5b4a"
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_PASSWORD", Value: secret}}, secretguard.DefaultConfig())
	at := func(n int, prefix string) string { return prefix + strings.Repeat("-", n-len(prefix)) }
	var body strings.Builder
	for body.Len() < 4000 {
		body.WriteString("filler line of harmless build output\n")
	}
	body.WriteString(strings.Repeat("x", 4096-20-body.Len()))
	body.WriteString(secret + "\nmore\n")
	for name, blocks := range map[string][]claudesdk.ContentBlock{
		"result header": {&claudesdk.ToolResultBlock{ToolUseID: "t1", Content: at(50, `{"dsn":"postgres://app:`) + secret + `@db:5432/app"}`}},
		"result body":   {&claudesdk.ToolResultBlock{ToolUseID: "t2", Content: body.String()}},
		"tool header":   {&claudesdk.ToolUseBlock{ID: "t3", Name: "Bash", Input: map[string]any{"command": at(60, "curl -sS --fail https://registry.example/v2/ -u deploy:") + secret}}},
		"todo item": {&claudesdk.ToolUseBlock{ID: "t4", Name: "TodoWrite", Input: map[string]any{"todos": []any{
			map[string]any{"content": at(170, "Rotate the database password found in the CI logs; it reads ") + secret, "status": "pending"}}}}},
	} {
		var buf strings.Builder
		logger := iterlog.New(iterlog.LevelInfo, &buf)
		logAssistantContent(logger, "node", 1, blocks, g.Redact, g.LongestLiteral())
		if leak := longestLeak(buf.String(), secret); leak != "" {
			t.Errorf("%s: %d of %d secret bytes reached the run log: %q", name, len(leak), len(secret), leak)
		}
	}
}

// The CLI labels a background shell with its description — with its command
// when there is none — and relays that label to the model when the shell
// ends: it stays in placeholder form while the command runs with the value.
func TestAShellsLabelStaysInPlaceholderForm(t *testing.T) {
	const placeholder = "__ITERION_SECRET_DEPLOY_TOKEN__"
	const secret = "s3cr3t-VALUE-7f6e5d4c3b2a"
	materialize := func(s string) string { return strings.ReplaceAll(s, placeholder, secret) }
	run := func(tool string, input map[string]any) map[string]any {
		t.Helper()
		out, err := materializeSecretsHandler(materialize)(context.Background(), claudesdk.HookCallbackInput{ToolName: tool, ToolInput: input})
		if err != nil {
			t.Fatal(err)
		}
		return out.UpdatedInput
	}
	cmd := "./deploy.sh --token " + placeholder
	for _, tool := range []string{"Bash", "PowerShell", "Monitor"} {
		got := run(tool, map[string]any{"command": cmd, "run_in_background": true})
		if got["command"] != "./deploy.sh --token "+secret || got["description"] != cmd {
			t.Fatalf("%s without a description: %#v", tool, got)
		}
		got = run(tool, map[string]any{"command": cmd, "description": "deploy with " + placeholder})
		if got["command"] != "./deploy.sh --token "+secret || got["description"] != "deploy with "+placeholder {
			t.Fatalf("%s with a description: %#v", tool, got)
		}
	}
	// A tool the CLI does not label that way is materialised leaf by leaf.
	if got := run("Write", map[string]any{"content": placeholder, "description": placeholder}); got["description"] != secret {
		t.Fatalf("Write: %#v", got)
	}
}

// The CLI quotes back the input it ran in some tools' output — stopping a
// background shell names its command, a notebook edit its new source: the
// known values in it go back to placeholder form before the model sees it.
func TestAnEchoedInputComesBackInPlaceholderForm(t *testing.T) {
	const secret = "s3cr3t-VALUE-7f6e5d4c3b2a"
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	handler := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")
	run := func(tool string, response any) any {
		t.Helper()
		out, err := handler(context.Background(), claudesdk.HookCallbackInput{ToolName: tool, ToolResponse: response})
		if err != nil {
			t.Fatal(err)
		}
		return out.UpdatedToolOutput
	}
	stopped := run("TaskStop", map[string]any{"message": "Successfully stopped task: b1 (curl -u deploy:" + secret + ")", "task_id": "b1",
		"command": "curl -u deploy:" + secret})
	if m, _ := stopped.(map[string]any); m == nil || strings.Contains(m["message"].(string), secret) || m["command"] != "curl -u deploy:__ITERION_SECRET_DEPLOY_TOKEN__" || m["task_id"] != "b1" {
		t.Fatalf("TaskStop output = %#v", stopped)
	}
	edited := run("NotebookEdit", map[string]any{"new_source": "token = " + secret, "cell_id": "c1", "cell_type": "code", "language": "python", "edit_mode": "replace"})
	if m, _ := edited.(map[string]any); m == nil || m["new_source"] != "token = __ITERION_SECRET_DEPLOY_TOKEN__" || m["language"] != "python" {
		t.Fatalf("NotebookEdit output = %#v", edited)
	}
	// The CLI keeps the original output — the value in it — when the
	// replacement is off its schema: a notebook whose metadata names its
	// language with a number still gets an on-schema replacement.
	offSchema := run("NotebookEdit", map[string]any{"new_source": "token = " + secret, "cell_id": "c1", "cell_type": "code", "language": 3.0, "edit_mode": "replace"})
	if m, _ := offSchema.(map[string]any); m == nil || m["language"] != "3" || m["new_source"] != "token = __ITERION_SECRET_DEPLOY_TOKEN__" {
		t.Fatalf("NotebookEdit output with a numeric language = %#v", offSchema)
	}
	// An output with nothing to change is not replaced: the CLI applies
	// sibling hooks' replacements last-write-wins.
	if got := run("TaskStop", map[string]any{"message": "Successfully stopped task: b2 (sleep 30)"}); got != nil {
		t.Fatalf("an unchanged output was replaced: %#v", got)
	}
}

func TestEveryToolButTheWorkspaceReadersIsUnmaterialised(t *testing.T) {
	const secret = "s3cr3t-VALUE-7f6e5d4c3b2a"
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	handler := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")
	replaced := func(tool string) bool {
		t.Helper()
		out, err := handler(context.Background(), claudesdk.HookCallbackInput{ToolName: tool, ToolResponse: map[string]any{"text": "token=" + secret}})
		if err != nil {
			t.Fatal(err)
		}
		return out.UpdatedToolOutput != nil
	}
	for _, tool := range []string{"TaskStop", "KillShell", "NotebookEdit", "WebFetch", "WebSearch", "Write", "mcp__db__query",
		"TaskCreate", "TodoWrite", "SendMessage", "Agent", "CronCreate", "RemoteTrigger"} {
		if !replaced(tool) {
			t.Errorf("%s: the value it quoted went back to the model", tool)
		}
	}
	// An agent editing a line that holds a secret must see the value the
	// file holds.
	for _, tool := range []string{"Read", "Edit", "Glob", "Grep", "Bash", "PowerShell", "LSP"} {
		if replaced(tool) {
			t.Errorf("%s: the workspace's value was hidden from the agent", tool)
		}
	}
	both := installMaterializeSecretsHook(Task{MaterializeSecrets: func(s string) string { return s }, UnmaterializeSecrets: func(s string) string { return s }}, nil)
	one := installMaterializeSecretsHook(Task{MaterializeSecrets: func(s string) string { return s }}, nil)
	if len(both) != 2 || len(one) != 1 {
		t.Fatalf("hooks installed: %d with the mirror, %d without; want 2 and 1", len(both), len(one))
	}
}

// A value longer than the run log shows is cut there, redacted a margin past
// the cut: a secret straddling it is recognised whole.
func TestTheRunLogCutsAHugeValueRedactedPastTheCut(t *testing.T) {
	const secret = "Tr0ub4dor-and-3-horses-correct-battery-staple-9f8e7d6c5b4a"
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_PASSWORD", Value: secret}}, secretguard.DefaultConfig())
	cmd := strings.Repeat("x", runLogShowMax-20) + secret + strings.Repeat("y", 40<<10)
	var buf strings.Builder
	logAssistantContent(iterlog.New(iterlog.LevelInfo, &buf), "node", 1, []claudesdk.ContentBlock{
		&claudesdk.ToolUseBlock{ID: "t1", Name: "Bash", Input: map[string]any{"command": cmd}}}, g.Redact, g.LongestLiteral())
	if leak := longestLeak(buf.String(), secret); leak != "" {
		t.Fatalf("%d of %d secret bytes reached the run log: %q", len(leak), len(secret), leak)
	}
	if !strings.Contains(buf.String(), "… (truncated)") {
		t.Fatalf("a %d-byte command was not cut", len(cmd))
	}
}

// Redacting a whole multi-megabyte tool result (a base64 image) costs
// seconds on the stream loop: the run log redacts what it can show.
func TestTheRunLogRedactsAHugeResultInBoundedTime(t *testing.T) {
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_TOKEN", Value: "s3cr3t-VALUE-7f6e5d4c3b2a"}}, secretguard.DefaultConfig())
	raw := make([]byte, 3<<20)
	for i := range raw {
		raw[i] = byte(i*7919 + i>>7)
	}
	result := base64.StdEncoding.EncodeToString(raw)
	start := time.Now()
	logAssistantContent(iterlog.New(iterlog.LevelInfo, io.Discard), "node", 1, []claudesdk.ContentBlock{
		&claudesdk.ToolResultBlock{ToolUseID: "t1", Content: result}}, g.Redact, g.LongestLiteral())
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("logging a %d-byte result took %s", len(result), d)
	}
}

// Redaction of what precedes it shrinks the text: a secret the redacted
// window holds only in part must not slide under the cut.
func TestTheRunLogNeverShowsAPartOfASecretItCouldNotRecognise(t *testing.T) {
	const victim = "Tr0ub4dor-and-3-horses-correct-battery-staple-9f8e7d6c5b4a"
	tok := "sk_live_" + strings.Repeat("aB3dE5fG7h", 5) + "Zq9Xw1"
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_PASSWORD", Value: victim}, {Name: "K", Value: tok}}, secretguard.DefaultConfig())
	var b strings.Builder
	for i := range 380 {
		fmt.Fprintf(&b, "SVC_%04d_API_KEY=%s\n", i, tok)
	}
	pos := runLogShowMax + runLogRedactMargin - 20
	for b.Len() < pos {
		b.WriteString("#")
	}
	cmd := b.String()[:pos] + victim + "\nEOF\n"
	var buf strings.Builder
	logAssistantContent(iterlog.New(iterlog.LevelInfo, &buf), "node", 1, []claudesdk.ContentBlock{
		&claudesdk.ToolUseBlock{ID: "t1", Name: "Bash", Input: map[string]any{"command": cmd}}}, g.Redact, g.LongestLiteral())
	if leak := longestLeak(buf.String(), victim); leak != "" {
		t.Fatalf("%d of %d secret bytes reached the run log: %q", len(leak), len(victim), leak)
	}
}

// A value whose shown window redacts to less than the margin shows nothing
// but the cut.
func TestTheRunLogSurvivesAValueRedactedBelowItsMargin(t *testing.T) {
	long := strings.Repeat("0123456789abcdef", 64)
	g := secretguard.New([]secretguard.Secret{{Name: "BLOB", Value: long}}, secretguard.DefaultConfig())
	var buf strings.Builder
	logAssistantContent(iterlog.New(iterlog.LevelInfo, &buf), "node", 1, []claudesdk.ContentBlock{
		&claudesdk.ToolUseBlock{ID: "t1", Name: "Bash", Input: map[string]any{"command": strings.Repeat(long+"\n", 100)}}}, g.Redact, g.LongestLiteral())
	if !strings.Contains(buf.String(), "… (truncated)") || strings.Contains(buf.String(), long[:32]) {
		t.Fatalf("run log = %.300q", buf.String())
	}
}

// The run log cuts a long value on a rune boundary: what it shows is valid
// UTF-8 whatever the character at the cut.
func TestTheRunLogCutIsOnARuneBoundary(t *testing.T) {
	for _, off := range []int{0, 1, 2} {
		s := strings.Repeat("x", off) + strings.Repeat("é€", (runLogShowMax+runLogRedactMargin)/3)
		out := redactForRunLog(s, func(v string) string { return v }, 0)
		if !utf8.ValidString(out) {
			t.Fatalf("offset %d: the cut run-log value is not valid UTF-8 (…%q)", off, out[len(out)-20:])
		}
	}
}

// A secret straddling the END of the redaction window (show + margin) is cut
// unrecognisable there: the run log never shows that far.
func TestASecretStraddlingTheWindowsEndNeverReachesTheRunLog(t *testing.T) {
	const secret = "Tr0ub4dor-and-3-horses-correct-battery-staple-9f8e7d6c5b4a"
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_PASSWORD", Value: secret}}, secretguard.DefaultConfig())
	cmd := strings.Repeat("x", runLogShowMax+runLogRedactMargin-20) + secret + strings.Repeat("y", 1<<10)
	var buf strings.Builder
	logAssistantContent(iterlog.New(iterlog.LevelInfo, &buf), "node", 1, []claudesdk.ContentBlock{
		&claudesdk.ToolUseBlock{ID: "t1", Name: "Bash", Input: map[string]any{"command": cmd}},
		&claudesdk.ToolResultBlock{ToolUseID: "t1", Content: cmd}}, g.Redact, g.LongestLiteral())
	if leak := longestLeak(buf.String(), secret); leak != "" {
		t.Fatalf("%d of %d secret bytes reached the run log: %q", len(leak), len(secret), leak)
	}
}

// configLikeSecret: n bytes of config-like text (a kubeconfig, say) with a
// unique head.
func configLikeSecret(t *testing.T, n int) string {
	t.Helper()
	head := make([]byte, 16)
	if _, err := rand.Read(head); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("apiVersion: v1 # " + hex.EncodeToString(head) + "\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "- name: context-%05d\n  namespace: team-%05d\n", i, i)
	}
	return b.String()[:n]
}

// longestPrefixLeak: the longest prefix of secret, 16 bytes or more, present
// in out.
func longestPrefixLeak(out, secret string) int {
	lo, hi := 16, len(secret)
	if !strings.Contains(out, secret[:lo]) {
		return 0
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if strings.Contains(out, secret[:mid]) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// A known value longer than the default margin — a file secret — straddling
// the window's end is read whole: the redactor reads as far past the cut as
// the longest value it knows.
func TestTheRunLogReadsAsFarAsTheLongestSecret(t *testing.T) {
	secret := configLikeSecret(t, 20<<10)
	g := secretguard.New([]secretguard.Secret{{Name: "KUBECONFIG_PROD", Value: secret}}, secretguard.DefaultConfig())
	cmd := strings.Repeat("x", runLogShowMax-(2<<10)) + secret + strings.Repeat("y", 8<<10)
	for name, block := range map[string]claudesdk.ContentBlock{
		"tool input":  &claudesdk.ToolUseBlock{ID: "t1", Name: "Bash", Input: map[string]any{"command": cmd}},
		"tool result": &claudesdk.ToolResultBlock{ToolUseID: "t1", Content: cmd},
	} {
		var buf strings.Builder
		logAssistantContent(iterlog.New(iterlog.LevelInfo, &buf), "node", 1, []claudesdk.ContentBlock{block}, g.Redact, g.LongestLiteral())
		if leak := longestPrefixLeak(buf.String(), secret); leak > 0 {
			t.Fatalf("%s: %d of %d secret bytes reached the run log", name, leak, len(secret))
		}
	}
	// A tool result shows only its head, but a secret there longer than the
	// whole window is read whole too.
	huge := configLikeSecret(t, 100<<10)
	hg := secretguard.New([]secretguard.Secret{{Name: "TRUSTSTORE", Value: huge}}, secretguard.DefaultConfig())
	var buf strings.Builder
	logAssistantContent(iterlog.New(iterlog.LevelInfo, &buf), "node", 1, []claudesdk.ContentBlock{
		&claudesdk.ToolResultBlock{ToolUseID: "t2", Content: strings.Repeat("x", 1<<10) + "\n" + huge + strings.Repeat("y", 8<<10)}}, hg.Redact, hg.LongestLiteral())
	if leak := longestPrefixLeak(buf.String(), huge); leak > 0 {
		t.Fatalf("tool result: %d of %d secret bytes reached the run log", leak, len(huge))
	}
}

// The same with a registered encoding longer than the margin: the hex form
// of a 9 KiB value is 18 KiB.
func TestTheRunLogReadsAsFarAsTheLongestEncoding(t *testing.T) {
	secret := configLikeSecret(t, 9<<10)
	enc := hex.EncodeToString([]byte(secret))
	g := secretguard.New([]secretguard.Secret{{Name: "SA_KEY", Value: secret}}, secretguard.DefaultConfig())
	cmd := strings.Repeat("x", runLogShowMax-(1<<10)) + enc + strings.Repeat("y", 8<<10)
	var buf strings.Builder
	logAssistantContent(iterlog.New(iterlog.LevelInfo, &buf), "node", 1, []claudesdk.ContentBlock{
		&claudesdk.ToolUseBlock{ID: "t1", Name: "Bash", Input: map[string]any{"command": cmd}}}, g.Redact, g.LongestLiteral())
	if leak := longestPrefixLeak(buf.String(), enc); leak > 0 {
		t.Fatalf("%d of %d bytes of the secret's hex encoding reached the run log", leak, len(enc))
	}
}

// fileLike: n bytes of file-like text with a unique head.
func fileLike(t *testing.T, tag string, n int) string {
	t.Helper()
	head := make([]byte, 12)
	if _, err := rand.Read(head); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(tag + " " + hex.EncodeToString(head) + "\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "line %05d of %s\n", i, tag)
	}
	return b.String()[:n]
}

// A file secret longer than the default margin, held only in part at the
// window's end after redaction shrank what precedes it: the cut keeps the
// secret's whole span out of what the run log shows.
func TestAPartialLongSecretAtTheWindowsEndIsCutByItsSpan(t *testing.T) {
	victim := fileLike(t, "VICTIMFILE", 20<<10)
	filler := fileLike(t, "FILLERFILE", 1000)
	g := secretguard.New([]secretguard.Secret{{Name: "KUBECONFIG_PROD", Value: victim}, {Name: "FILLER", Value: filler}}, secretguard.DefaultConfig())
	span := g.LongestLiteral()
	if span <= runLogRedactMargin+(4<<10) {
		t.Fatalf("scenario broken: span %d is not past the default margin", span)
	}
	pos := runLogShowMax + span - 19<<10 // 19 KiB of the 20 KiB secret inside the window
	var b strings.Builder
	for b.Len()+len(filler)+1 <= pos {
		b.WriteString(filler + "\n")
	}
	for b.Len() < pos {
		b.WriteString("#")
	}
	cmd := b.String() + victim + strings.Repeat("y", 8<<10)
	var buf strings.Builder
	logAssistantContent(iterlog.New(iterlog.LevelInfo, &buf), "node", 1, []claudesdk.ContentBlock{
		&claudesdk.ToolUseBlock{ID: "t1", Name: "Bash", Input: map[string]any{"command": cmd}}}, g.Redact, span)
	if leak := longestPrefixLeak(buf.String(), victim); leak > 0 {
		t.Fatalf("%d of %d secret bytes reached the run log (span %d)", leak, len(victim), span)
	}
}

// What the run log shows of a value holding no secret: its bound, whatever
// the span — whole when it is no longer than that.
func TestTheRunLogShowsItsBoundOfAValueWithNoSecret(t *testing.T) {
	big := fileLike(t, "TRUSTSTORE", 60<<10)
	g := secretguard.New([]secretguard.Secret{{Name: "TRUSTSTORE", Value: big}}, secretguard.DefaultConfig())
	plain := func(v string) string { return v }
	for _, c := range []struct {
		name string
		red  func(string) string
		span int
		n    int
		want int
	}{
		{"a 60 KiB file secret registered, a 100 KiB value", g.Redact, g.LongestLiteral(), 100 << 10, runLogShowMax},
		{"a 60 KiB file secret registered, a 200 KiB value", g.Redact, g.LongestLiteral(), 200 << 10, runLogShowMax},
		{"default margin, a 65 KiB value", plain, 0, 65 << 10, runLogShowMax},
		{"a value at the bound", plain, 0, runLogShowMax, runLogShowMax},
	} {
		s := strings.Repeat("0123456789 abcdef\n", c.n/18+1)[:c.n]
		out := redactForRunLog(s, c.red, c.span)
		if cut := strings.HasSuffix(out, "\n… (truncated)"); cut != (c.n > runLogShowMax) {
			t.Errorf("%s: cut = %v", c.name, cut)
		}
		if got := len(strings.TrimSuffix(out, "\n… (truncated)")); got != c.want {
			t.Errorf("%s: the run log shows %d of %d bytes (span %d), want %d", c.name, got, c.n, c.span, c.want)
		}
	}
}

// Redaction may lengthen the text (a placeholder longer than the value it
// replaces): the run log still shows at most its bound, and no value.
func TestARedactionThatLengthensTheTextStillShowsAtMostTheBound(t *testing.T) {
	g := secretguard.New([]secretguard.Secret{{Name: "PW", Value: "hunter2!"}}, secretguard.DefaultConfig())
	s := strings.Repeat("hunter2! ", (200<<10)/9)
	out := strings.TrimSuffix(redactForRunLog(s, g.Redact, g.LongestLiteral()), "\n… (truncated)")
	if len(out) > runLogShowMax || strings.Contains(out, "hunter2!") {
		t.Fatalf("the run log shows %d bytes (bound %d)", len(out), runLogShowMax)
	}
}

// The notebook-language coercion is NotebookEdit's own: another tool's output
// keeps its types — a replacement off that tool's schema would make the CLI
// keep the original, the value in it.
func TestOnlyANotebookEditsLanguageIsCoerced(t *testing.T) {
	const secret = "s3cr3t-VALUE-7f6e5d4c3b2a"
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	out, err := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")(context.Background(), claudesdk.HookCallbackInput{
		ToolName: "mcp__lint__analyze", ToolResponse: map[string]any{"language": 3.0, "report": "token " + secret}})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := out.UpdatedToolOutput.(map[string]any)
	if m == nil || m["language"] != 3.0 || strings.Contains(m["report"].(string), secret) {
		t.Fatalf("output = %#v", out.UpdatedToolOutput)
	}
}

// The tools whose input is kept rather than run see it in placeholder form:
// the node's report (the CLI stores the input StructuredOutput is called
// with), the session's task list, iterion's own MCP tools (a question to the
// operator, a board issue, a run query). A tool that runs gets the value.
func TestAKeptInputStaysInPlaceholderForm(t *testing.T) {
	const placeholder = "__ITERION_SECRET_DEPLOY_TOKEN__"
	const secret = "s3cr3t-VALUE-7f6e5d4c3b2a"
	materialize := func(s string) string { return strings.ReplaceAll(s, placeholder, secret) }
	updated := func(tool string, input map[string]any) map[string]any {
		t.Helper()
		out, err := materializeSecretsHandler(materialize)(context.Background(), claudesdk.HookCallbackInput{ToolName: tool, ToolInput: input})
		if err != nil {
			t.Fatal(err)
		}
		return out.UpdatedInput
	}
	for _, tool := range []string{"StructuredOutput", "TodoWrite", "TaskCreate", "TaskUpdate", "CronCreate", "memory_write", "Skill", "ScheduleWakeup",
		"mcp__iterion__ask_user", "mcp__iterion_board__create_issue", "mcp__iterion_runs__get_run"} {
		if got := updated(tool, map[string]any{"body": "deployed with " + placeholder}); got != nil {
			t.Errorf("%s: its kept input was materialised: %#v", tool, got)
		}
	}
	if got := updated("Bash", map[string]any{"command": "./deploy.sh " + placeholder}); got["command"] != "./deploy.sh "+secret {
		t.Fatalf("control: Bash = %#v", got)
	}
}

// A workspace reader whose call carried a secret — a command using a
// credential, which may print it back — has its output unmaterialised, in
// either form the hook sees the input: with the placeholder, or with the
// value it was materialised into.
func TestAReaderWhoseCallCarriedASecretIsUnmaterialised(t *testing.T) {
	const secret = "s3cr3t-VALUE-7f6e5d4c3b2a"
	g := secretguard.New([]secretguard.Secret{{Name: "DEPLOY_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	handler := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")
	for _, cmd := range []string{"curl -v -H 'Authorization: Bearer __ITERION_SECRET_DEPLOY_TOKEN__' api", "curl -v -H 'Authorization: Bearer " + secret + "' api"} {
		out, err := handler(context.Background(), claudesdk.HookCallbackInput{ToolName: "Bash",
			ToolInput: map[string]any{"command": cmd}, ToolResponse: map[string]any{"stdout": "> Authorization: Bearer " + secret}})
		if err != nil {
			t.Fatal(err)
		}
		if m, _ := out.UpdatedToolOutput.(map[string]any); m == nil || strings.Contains(m["stdout"].(string), secret) {
			t.Errorf("command %q: the value it printed back went to the model: %#v", cmd, out.UpdatedToolOutput)
		}
	}
}

// A background command's output reaches the model through a later Read of
// the CLI's task output file (the CLI tells the agent to read it there): that
// call names no secret, the command that printed it did — the output returns
// placeholders. A Read of a workspace file stays raw.
func TestAReadOfABackgroundTasksOutputIsUnmaterialised(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	h := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")
	printed := "> Authorization: token " + secret + "\n"
	for _, c := range []struct {
		tool  string
		input map[string]any
		raw   bool
	}{
		{"Read", map[string]any{"file_path": "/tmp/claude-1000/-home-jo-proj/5f0c2a9e-1d2b-4c3d-9e8f-0a1b2c3d4e5f/tasks/b7k2m9q4x.output"}, false},
		{"Bash", map[string]any{"command": "tail -n 50 /tmp/claude-1000/-home-jo-proj/5f0c2a9e/tasks/b7k2m9q4x.output"}, false},
		{"Read", map[string]any{"file_path": "/work/config/tasks.yml"}, true},
	} {
		out, err := h(context.Background(), claudesdk.HookCallbackInput{ToolName: c.tool, ToolInput: c.input, ToolResponse: map[string]any{"stdout": printed}})
		if err != nil {
			t.Fatal(err)
		}
		if replaced := out.UpdatedToolOutput != nil; replaced == c.raw {
			t.Errorf("%s %v: output replaced = %v, want %v", c.tool, c.input, replaced, !c.raw)
		}
	}
}

// A command naming a secret is not compressed — the rewriter would run it
// with the value (rtk records every command it runs in its history); the
// materialisation hook's answer runs it. A command naming none still is.
func TestACommandNamingASecretIsNotCompressed(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	bin := filepath.Join(t.TempDir(), "fakerw")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'rtk %s' \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	chain := rewrite.NewChain([]plugin.RewriterSpec{{ID: "fake", Locate: plugin.LocateSpec{Paths: []string{bin}},
		Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}, ApplyExitCodes: []int{0}}}})
	h := rewriteCommandHandler(chain, rewrite.ParseMode("on"), g.Materialize)
	named := map[string]any{"command": "git push https://x:__ITERION_SECRET_GH_TOKEN__@github.com/o/r", "description": "push"}
	out, err := h(context.Background(), claudesdk.HookCallbackInput{ToolName: "Bash", ToolInput: named})
	if err != nil {
		t.Fatal(err)
	}
	if out.UpdatedInput != nil {
		t.Errorf("a command naming a secret was rewritten: %#v", out.UpdatedInput)
	}
	mat, _ := materializeSecretsHandler(g.Materialize)(context.Background(), claudesdk.HookCallbackInput{ToolName: "Bash", ToolInput: named})
	if cmd, _ := mat.UpdatedInput["command"].(string); !strings.Contains(cmd, secret) {
		t.Fatalf("scenario broken: the materialisation hook's command %q", cmd)
	}
	plain, err := h(context.Background(), claudesdk.HookCallbackInput{ToolName: "Bash", ToolInput: map[string]any{"command": "git status"}})
	if err != nil {
		t.Fatal(err)
	}
	if cmd, _ := plain.UpdatedInput["command"].(string); cmd != "rtk git status" {
		t.Errorf("a command naming no secret was not compressed: %#v", plain.UpdatedInput)
	}
}

// A read of the CLI's task outputs through their directory (a search, a glob,
// a cd into it) returns what every task printed — placeholders; so does a
// named agent's output file (its id carries the name). Workspace reads stay raw.
func TestReadsOfTheTaskOutputDirectoryAreUnmaterialised(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	h := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")
	const dir = "/tmp/claude-1000/claude-1000/-tmp-proj/198cb05b-c420-42d3-9c91-19d606a63388/tasks"
	for _, c := range []struct {
		tool  string
		input map[string]any
		raw   bool
	}{
		{"Bash", map[string]any{"command": "grep -r token= " + dir}, false},
		{"Bash", map[string]any{"command": "cat " + dir + "/*.output"}, false},
		{"Bash", map[string]any{"command": "cd " + dir + " && tail -n 3 *.output"}, false},
		{"Read", map[string]any{"file_path": dir + "/aresearcher-0123456789abcdef.output"}, false},
		{"Read", map[string]any{"file_path": "/work/config/tasks.yml"}, true},
		{"Bash", map[string]any{"command": "ls /work/repo/tasks && cat /work/repo/ci/build.output"}, true},
	} {
		out, err := h(context.Background(), claudesdk.HookCallbackInput{ToolName: c.tool, ToolInput: c.input,
			ToolResponse: map[string]any{"stdout": "token=" + secret + "\n"}})
		if err != nil {
			t.Fatal(err)
		}
		if replaced := out.UpdatedToolOutput != nil; replaced == c.raw {
			t.Errorf("%s %v: output replaced = %v, want %v", c.tool, c.input, replaced, !c.raw)
		}
	}
}

// W2 (M2): a Bash call reading a task's output names it in its command; its
// description is another leaf, visited in random order.
func TestATaskOutputReadIsFoundWhateverTheLeafOrder(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	h := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")
	for i := 0; i < 64; i++ {
		out, err := h(context.Background(), claudesdk.HookCallbackInput{ToolName: "Bash",
			ToolInput:    map[string]any{"command": "tail -n 50 /tmp/claude-1000/-home-jo-proj/5f0c2a9e/tasks/b7k2m9q4x.output", "description": "tail the build log"},
			ToolResponse: map[string]any{"stdout": "> Authorization: token " + secret + "\n"}})
		if err != nil {
			t.Fatal(err)
		}
		if out.UpdatedToolOutput == nil {
			t.Fatalf("run %d: the task output read was left raw", i)
		}
	}
}

// W3 (M3): a background agent's output file is named by its agentId ("a" and
// 16 hex digits).
func TestABackgroundAgentsOutputReadIsUnmaterialised(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	h := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")
	out, err := h(context.Background(), claudesdk.HookCallbackInput{ToolName: "Read",
		ToolInput:    map[string]any{"file_path": "/tmp/claude-1000/-home-jo-proj/5f0c2a9e-1d2b-4c3d-9e8f-0a1b2c3d4e5f/tasks/a0123456789abcdef.output"},
		ToolResponse: map[string]any{"stdout": "token " + secret}})
	if err != nil {
		t.Fatal(err)
	}
	if out.UpdatedToolOutput == nil {
		t.Errorf("a background agent's output file was read raw")
	}
}

// W4 (M4): a workspace file with an .output extension outside the CLI's
// tasks directory is a workspace read: left as is.
func TestAWorkspaceDotOutputFileStaysRaw(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	h := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")
	out, err := h(context.Background(), claudesdk.HookCallbackInput{ToolName: "Read",
		ToolInput:    map[string]any{"file_path": "/work/repo/ci/build.output"},
		ToolResponse: map[string]any{"stdout": "token " + secret}})
	if err != nil {
		t.Fatal(err)
	}
	if out.UpdatedToolOutput != nil {
		t.Errorf("a workspace file was unmaterialised: %v", out.UpdatedToolOutput)
	}
}

// A session's CLI — whose shell runs the commands — carries the chain's run
// env, over the inherited environment and the run's own, whatever the mode:
// the agent may run the rewriter itself, or an operator's own hook may (rtk's
// `rtk init -g`), and it keeps no store of what those commands ran or
// printed. A session with no rewriter available carries none.
func TestASessionsCLICarriesTheRunEnvWhateverTheMode(t *testing.T) {
	dir := t.TempDir()
	rw := filepath.Join(dir, "fakerw")
	if err := os.WriteFile(rw, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(dir, "env")
	cli := filepath.Join(dir, "fake-claude")
	script := `#!/bin/sh
printf 'db=%s recall=%s\n' "${RTK_DB_PATH-unset}" "${RTK_RECALL-unset}" > "$ITERION_TEST_CAPTURE"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{}","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"env-test"}'
`
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RTK_DB_PATH", "/home/op/.local/share/rtk/history.db")
	spec := plugin.RewriterSpec{ID: "rtk", Locate: plugin.LocateSpec{Paths: []string{rw}},
		Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}},
		RunEnv: map[string]string{"RTK_DB_PATH": "/dev/null", "RTK_RECALL": "0"}}
	absent := spec
	absent.Locate = plugin.LocateSpec{Paths: []string{filepath.Join(dir, "missing")}}
	for _, c := range []struct {
		mode string
		spec plugin.RewriterSpec
		want string
	}{
		{"on", spec, "db=/dev/null recall=0"},
		{"", spec, "db=/dev/null recall=0"},
		{"on", absent, "db=/home/op/.local/share/rtk/history.db recall=1"},
	} {
		if err := os.Remove(capture); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		task := Task{NodeID: "n", CompressMode: c.mode, Rewriters: []plugin.RewriterSpec{c.spec},
			ExtraEnv: []string{"RTK_RECALL=1", "ITERION_TEST_CAPTURE=" + capture}}
		// Execute's order: the spawn's per-task env, then the hooks.
		opts := installRewriteHook(task, append([]claudesdk.Option{claudesdk.WithCLIPath(cli)}, perTaskSpawnOpts(task)...))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := claudesdk.Prompt(ctx, "go", opts...)
		cancel()
		if err != nil {
			t.Fatalf("compress %q: %v", c.mode, err)
		}
		raw, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(raw)); got != c.want {
			t.Errorf("compress %q (rewriter at %v): the CLI's environment has %s, want %s", c.mode, c.spec.Locate.Paths, got, c.want)
		}
	}
}

// The formatting pass resumes the session with the CLI's native tools, Bash
// included: its spawn carries the run env as the main pass's does.
func TestTheFormattingPassCarriesTheRunEnv(t *testing.T) {
	resetClaudeCredEnv(t)
	t.Setenv("RTK_DB_PATH", "/home/op/.local/share/rtk/history.db")
	dir := t.TempDir()
	rw := filepath.Join(dir, "fakerw")
	if err := os.WriteFile(rw, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(dir, "env")
	cli := filepath.Join(dir, "fake-claude")
	script := `#!/bin/sh
printf 'db=%s recall=%s\n' "${RTK_DB_PATH-unset}" "${RTK_RECALL-unset}" > "$ITERION_TEST_CAPTURE"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{}","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"env-test"}'
`
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	task := Task{NodeID: "n", Command: cli, WorkDir: dir, OutputSchema: []byte(`{"type":"object"}`),
		ExtraEnv: []string{"ITERION_TEST_CAPTURE=" + capture},
		Rewriters: []plugin.RewriterSpec{{ID: "rtk", Locate: plugin.LocateSpec{Paths: []string{rw}},
			Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}},
			RunEnv: map[string]string{"RTK_DB_PATH": "/dev/null/x.db", "RTK_RECALL": "0"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	if _, _, _, err := b.formatOutput(ctx, task, "env-test"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "db=/dev/null/x.db recall=0" {
		t.Errorf("the formatting pass's CLI environment has %s", got)
	}
}

// A compressed command keeps the agent's label: the CLI labels a shell with
// its description, with its command when there is none, and relays the
// label to the agent when a background shell ends — the command the agent
// wrote, not the compressed one (which exports the run env first).
func TestACompressedCommandKeepsTheAgentsLabel(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fakerw")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'rtk %s' \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	chain := rewrite.NewChain([]plugin.RewriterSpec{{ID: "fake", Locate: plugin.LocateSpec{Paths: []string{bin}},
		Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}, ApplyExitCodes: []int{0}},
		RunEnv: map[string]string{"RTK_RECALL": "0"}}})
	h := rewriteCommandHandler(chain, rewrite.ParseMode("on"), nil)
	for _, c := range []struct {
		input map[string]any
		label string
	}{
		{map[string]any{"command": "npm test", "run_in_background": true}, "npm test"},
		{map[string]any{"command": "npm test", "description": ""}, "npm test"},
		{map[string]any{"command": "npm test", "description": "run the suite"}, "run the suite"},
	} {
		out, err := h(context.Background(), claudesdk.HookCallbackInput{ToolName: "Bash", ToolInput: c.input})
		if err != nil {
			t.Fatal(err)
		}
		if cmd, _ := out.UpdatedInput["command"].(string); cmd != "export RTK_RECALL=0; rtk npm test" {
			t.Fatalf("scenario broken: the compressed command %q", cmd)
		}
		if got, _ := out.UpdatedInput["description"].(string); got != c.label {
			t.Errorf("%v: the shell's label %q, want %q", c.input, got, c.label)
		}
	}
}

// A workspace reached through a symlink is the workspace too: the CLI
// reports its working directory resolved, and the agent reads its files
// under that path — raw.
func TestAWorkspaceReachedThroughASymlinkIsReadRaw(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	real := filepath.Join(t.TempDir(), "019f8a6c-1d2b-7c3d-9e8f-0a1b2c3d4e5f")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "worktree")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	h := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, link)
	for _, path := range []string{resolved + "/tasks/main.yml", link + "/tasks/main.yml"} {
		out, err := h(context.Background(), claudesdk.HookCallbackInput{ToolName: "Read",
			ToolInput:    map[string]any{"file_path": path},
			ToolResponse: map[string]any{"stdout": "token=" + secret + "\n"}})
		if err != nil {
			t.Fatal(err)
		}
		if out.UpdatedToolOutput != nil {
			t.Errorf("%s: a file of the workspace returned placeholders", path)
		}
	}
}

// Reads of task outputs the CLI's tmp root reaches — a search over it or over
// its project directory, a glob across sessions, a find, the Grep tool's path
// or its glob, an output file named through a variable — return what the
// tasks printed: placeholders.
func TestReadsThroughTheCLIsTmpRootAreUnmaterialised(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	h := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")
	const sess = "/tmp/claude-1000/-home-jo-proj/5f0c2a9e-1d2b-4c3d-9e8f-0a1b2c3d4e5f"
	for _, c := range []struct {
		tool  string
		input map[string]any
	}{
		{"Bash", map[string]any{"command": "grep -rn Authorization /tmp/claude-1000/-home-jo-proj/"}},
		{"Bash", map[string]any{"command": "grep -rn Authorization /tmp/claude-1000"}},
		{"Bash", map[string]any{"command": "tail -n 5 /tmp/claude-1000/-home-jo-proj/*/tasks/*.output"}},
		{"Bash", map[string]any{"command": "find /tmp/claude-1000 -name '*.output' -exec cat {} +"}},
		{"Grep", map[string]any{"pattern": "token", "path": "/tmp/claude-1000/-home-jo-proj", "output_mode": "content"}},
		{"Grep", map[string]any{"pattern": "token", "path": "/tmp/claude-1000", "glob": "*.output", "output_mode": "content"}},
		{"Bash", map[string]any{"command": "D=" + sess + "; cat \"$D\"/tasks/b7k2m9q4x.output"}},
		{"Bash", map[string]any{"command": "cat " + sess + "/tasks/$ID.output"}},
		{"Bash", map[string]any{"command": "cat /var/tmp/proj/tasks/b7k2.m9q4x.output"}},
	} {
		out, err := h(context.Background(), claudesdk.HookCallbackInput{ToolName: c.tool, ToolInput: c.input,
			ToolResponse: map[string]any{"stdout": "token=" + secret + "\n"}})
		if err != nil {
			t.Fatal(err)
		}
		if out.UpdatedToolOutput == nil {
			t.Errorf("%s %v: what the tasks printed went to the model raw", c.tool, c.input)
		}
	}
}

// The node's workspace is its own, whatever its name: a worktree's root and a
// cloud checkout are named by the run's UUID — like the CLI's session
// directory — and their files (an Ansible role's tasks/, a tasks.py) are read
// raw, so an edit of a line the agent read matches the file. The same reads
// with no workspace to tell them apart return placeholders; a task output
// read in a run with a workspace still does.
func TestAWorkspaceNamedByTheRunsUUIDIsReadRaw(t *testing.T) {
	const secret = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "GH_TOKEN", Value: secret}}, secretguard.DefaultConfig())
	const wt = "/home/jo/proj/.iterion/worktrees/019f8a6c-1d2b-7c3d-9e8f-0a1b2c3d4e5f"
	const cloud = "/var/lib/iterion/repos/019f8a6c-1d2b-7c3d-9e8f-0a1b2c3d4e5f"
	const tmpWS = "/tmp/claude-1000/runs/019f8a6c-1d2b-7c3d-9e8f-0a1b2c3d4e5f"
	for _, c := range []struct {
		ws, tool string
		input    map[string]any
	}{
		{wt, "Read", map[string]any{"file_path": wt + "/tasks/main.yml"}},
		{wt, "Read", map[string]any{"file_path": wt + "/tasks.py"}},
		{wt, "Edit", map[string]any{"file_path": wt + "/tasks/deploy.yml", "old_string": "a", "new_string": "b"}},
		{wt + "/", "Bash", map[string]any{"command": "cat '" + wt + "/tasks/main.yml'"}},
		{cloud, "Bash", map[string]any{"command": "cat " + cloud + "/tasks.json"}},
		{tmpWS, "Bash", map[string]any{"command": "grep -rn token " + tmpWS}},
	} {
		in := claudesdk.HookCallbackInput{ToolName: c.tool, ToolInput: c.input,
			ToolResponse: map[string]any{"stdout": "token=" + secret + "\n"}}
		out, err := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, c.ws)(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if out.UpdatedToolOutput != nil {
			t.Errorf("workspace %s: %s %v returned placeholders", c.ws, c.tool, c.input)
		}
		if out, _ := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, "")(context.Background(), in); out.UpdatedToolOutput == nil {
			t.Errorf("scenario broken: %s %v is no task output read even without a workspace", c.tool, c.input)
		}
	}
	out, err := unmaterializeOutputHandler(g.Materialize, g.Unmaterialize, wt)(context.Background(), claudesdk.HookCallbackInput{ToolName: "Read",
		ToolInput:    map[string]any{"file_path": "/tmp/claude-1000/-home-jo-proj/5f0c2a9e-1d2b-4c3d-9e8f-0a1b2c3d4e5f/tasks/b7k2m9q4x.output"},
		ToolResponse: map[string]any{"stdout": "token=" + secret + "\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.UpdatedToolOutput == nil {
		t.Error("a task output read in a run with a workspace went to the model raw")
	}
}
