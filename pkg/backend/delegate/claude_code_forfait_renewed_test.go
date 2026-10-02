package delegate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// revokedRender is the CLI's render of the provider refusing an access token
// the store's refresh worker has just rotated.
const revokedRender = `Failed to authenticate. API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked."}}`

func forfaitBlob(access string) []byte {
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"rt","expiresAt":%d}}`, access, time.Now().Add(8*time.Hour).UnixMilli()))
}

func writeForfait(t *testing.T, dir, access string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, secrets.ClaudeCodeCredentialsFileName), forfaitBlob(access), 0o600); err != nil {
		t.Fatal(err)
	}
}

func authEvidenceRecorder(task *Task) *[]usagecap.Reading {
	var mu sync.Mutex
	got := &[]usagecap.Reading{}
	task.Hooks.OnUsageWindow = func(r usagecap.Reading) error {
		mu.Lock()
		defer mu.Unlock()
		*got = append(*got, r)
		return nil
	}
	return got
}

// TestRenderedFailure_renewedTokenIsTransient: an auth render on a token
// the forfait file no longer carries is the store's rotation, not a dead
// credential — transient, and no auth evidence to bench a healthy forfait.
// The same render on the token the file still carries is a dead credential,
// and so is one from a spawn that carried no forfait token.
func TestRenderedFailure_renewedTokenIsTransient(t *testing.T) {
	dir := t.TempDir()
	writeForfait(t, dir, "at.after")
	task := Task{}
	got := authEvidenceRecorder(&task)
	b := &ClaudeCodeBackend{Logger: iterlog.Nop(), renewalWait: 50 * time.Millisecond}
	render := revokedRender
	rm := &claudesdk.ResultMessage{Result: &render}
	ctx := context.Background()

	var tr *ErrTransient
	if err := b.renderedFailure(ctx, rm, task, "pass 1", forfaitSpawn{dir: dir, token: "at.before"}); !errors.As(err, &tr) {
		t.Fatalf("a renewed token: err = %v, want ErrTransient", err)
	}
	if len(*got) != 0 {
		t.Fatalf("auth evidence filed for a renewed token: %+v", *got)
	}
	var auth *ErrAuthFailed
	if err := b.renderedFailure(ctx, rm, task, "pass 1", forfaitSpawn{dir: dir, token: "at.after"}); !errors.As(err, &auth) {
		t.Fatalf("the token the file still carries: err = %v, want ErrAuthFailed", err)
	}
	if len(*got) != 1 {
		t.Fatalf("a dead credential left %d readings, want 1", len(*got))
	}
	if err := b.renderedFailure(ctx, rm, task, "pass 1", forfaitSpawn{}); !errors.As(err, &auth) {
		t.Fatalf("a spawn without a forfait token: err = %v, want ErrAuthFailed", err)
	}
	// A rotation excuses an auth render only: a window notice on a renewed
	// token is still the window.
	limit := "You've hit your weekly limit · resets 9pm (Europe/Paris)"
	var rl *ErrRateLimited
	if err := b.renderedFailure(ctx, &claudesdk.ResultMessage{Result: &limit}, task, "pass 1", forfaitSpawn{dir: dir, token: "at.before"}); !errors.As(err, &rl) {
		t.Fatalf("a window notice on a renewed token: err = %v, want ErrRateLimited", err)
	}
	model := "There's an issue with the selected model (claude-x). It may not exist or you may not have access to it."
	if err := b.renderedFailure(ctx, &claudesdk.ResultMessage{Result: &model}, task, "pass 1", forfaitSpawn{dir: dir, token: "at.before"}); err == nil || errors.As(err, &tr) {
		t.Fatalf("a model-unavailable render on a renewed token: err = %v, want the model error", err)
	}
}

// TestRenderedFailure_theWaitEndsWithTheNode: a cancelled node does not sit
// out the renewal wait.
func TestRenderedFailure_theWaitEndsWithTheNode(t *testing.T) {
	dir := t.TempDir()
	writeForfait(t, dir, "at.before")
	b := &ClaudeCodeBackend{Logger: iterlog.Nop(), renewalWait: 30 * time.Second}
	render := revokedRender
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	var auth *ErrAuthFailed
	if err := b.renderedFailure(ctx, &claudesdk.ResultMessage{Result: &render}, Task{}, "pass 1", forfaitSpawn{dir: dir, token: "at.before"}); !errors.As(err, &auth) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a cancelled node waited %s for a renewal", took)
	}
}

// TestRenderedFailure_aRenewalShipsAnAnswer: a structured answer is the
// answer whatever words it contains, on a renewed spawn too — the renewal
// only re-types what would have been a dead credential.
func TestRenderedFailure_aRenewalShipsAnAnswer(t *testing.T) {
	dir := t.TempDir()
	writeForfait(t, dir, "at.after")
	b := &ClaudeCodeBackend{Logger: iterlog.Nop(), renewalWait: 50 * time.Millisecond}
	answer := `{"status":"failed","reason":"gh: not logged into any host"}`
	if !isAuthErrorResult(answer) {
		t.Fatal("the fixture must read as an auth render, or this proves nothing")
	}
	if err := b.renderedFailure(context.Background(), &claudesdk.ResultMessage{Result: &answer}, Task{}, "pass 1", forfaitSpawn{dir: dir, token: "at.before"}); err != nil {
		t.Fatalf("a structured answer on a renewed spawn was not shipped: %v", err)
	}
}

// TestRenderedFailure_waitsForTheRunnerToWriteTheRotation: the provider
// refuses a rotated token at once, and the runner writes the new one on its
// next follow pass. A rotation that reaches the file shortly after the
// render is still a rotation.
func TestRenderedFailure_waitsForTheRunnerToWriteTheRotation(t *testing.T) {
	dir := t.TempDir()
	writeForfait(t, dir, "at.before")
	task := Task{}
	got := authEvidenceRecorder(&task)
	b := &ClaudeCodeBackend{Logger: iterlog.Nop(), renewalWait: 5 * time.Second}
	render := revokedRender
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(300 * time.Millisecond)
		writeForfait(t, dir, "at.after")
	}()
	defer func() { <-done }()
	var tr *ErrTransient
	if err := b.renderedFailure(context.Background(), &claudesdk.ResultMessage{Result: &render}, task, "pass 1", forfaitSpawn{dir: dir, token: "at.before"}); !errors.As(err, &tr) {
		t.Fatalf("a rotation written after the render: err = %v, want ErrTransient", err)
	}
	if len(*got) != 0 {
		t.Fatalf("auth evidence filed for a rotated token: %+v", *got)
	}
}

// fakeClaudeRotating stands in for the claude CLI. Spawn N prints
// $LINES/N, after replacing the forfait file with $LINES/rotate-N when that
// file exists — the rotation the runner writes while a CLI runs — or with
// $LINES/late-rotate-N shortly AFTER it answered. Every spawn logs the
// forfait token its env carried.
const fakeClaudeRotating = `#!/bin/sh
case "$*" in *--input-format*)
	while read -r line; do
		case "$line" in *'"type":"user"'*) break ;; esac
	done ;;
esac
n=$(( $(cat "$LINES/count" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$LINES/count"
printf '%s\n' "$CLAUDE_CODE_OAUTH_TOKEN" >> "$LINES/tokens"
if [ -f "$LINES/rotate-$n" ]; then
	cp "$LINES/rotate-$n" "$FORFAIT_FILE.tmp" && mv "$FORFAIT_FILE.tmp" "$FORFAIT_FILE"
fi
if [ -f "$LINES/late-rotate-$n" ]; then
	( sleep 0.3; cp "$LINES/late-rotate-$n" "$FORFAIT_FILE.tmp" && mv "$FORFAIT_FILE.tmp" "$FORFAIT_FILE" ) >/dev/null 2>&1 &
fi
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
cat "$LINES/$n"
`

func resultLine(t *testing.T, text string, isError bool) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(`{"type":"result","subtype":"success","is_error":%t,"result":%q,"num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"s1"}`+"\n", isError, text))
}

// runRotating runs the REAL Execute against the stand-in CLI on a forfait
// holding at.before. spawns maps a spawn number to the line it prints;
// rotateOn names the spawns that rotate the file to at.after first.
func runRotating(t *testing.T, task Task, spawns map[int][]byte, rotateOn ...int) (Result, []string, *[]usagecap.Reading, error) {
	return runRotatingLate(t, task, spawns, nil, rotateOn...)
}

// runRotatingLate is runRotating with spawns that rotate the file only after
// they answered.
func runRotatingLate(t *testing.T, task Task, spawns map[int][]byte, lateOn []int, rotateOn ...int) (Result, []string, *[]usagecap.Reading, error) {
	t.Helper()
	resetClaudeCredEnv(t)
	forfait := t.TempDir()
	writeForfait(t, forfait, "at.before")
	lines := t.TempDir()
	for n, line := range spawns {
		if err := os.WriteFile(filepath.Join(lines, fmt.Sprint(n)), line, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range rotateOn {
		if err := os.WriteFile(filepath.Join(lines, fmt.Sprintf("rotate-%d", n)), forfaitBlob("at.after"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range lateOn {
		if err := os.WriteFile(filepath.Join(lines, fmt.Sprintf("late-rotate-%d", n)), forfaitBlob("at.after"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(script, []byte(fakeClaudeRotating), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINES", lines)
	t.Setenv("FORFAIT_FILE", filepath.Join(forfait, secrets.ClaudeCodeCredentialsFileName))
	task.Command = script
	task.WorkDir = t.TempDir()
	task.UserPrompt = "x"
	got := authEvidenceRecorder(&task)
	ctx, cancel := context.WithTimeout(secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfait},
	}), 30*time.Second)
	defer cancel()
	b := &ClaudeCodeBackend{Logger: iterlog.Nop(), renewalWait: 3 * time.Second}
	res, err := b.Execute(ctx, task)
	raw, _ := os.ReadFile(filepath.Join(lines, "tokens"))
	return res, strings.Fields(string(raw)), got, err
}

// TestExecute_forfaitRenewedUnderTheCLI: the first pass renders the refusal
// of a token rotated while it ran. The node fails transient — the executor
// retries on a spawn that reads the new token, resuming session s1 — and no
// auth evidence is filed. The same render with the token still on file is a
// dead credential: ErrAuthFailed, and the evidence.
func TestExecute_forfaitRenewedUnderTheCLI(t *testing.T) {
	res, tokens, got, err := runRotating(t, Task{}, map[int][]byte{1: resultLine(t, revokedRender, true)}, 1)
	var tr *ErrTransient
	if !errors.As(err, &tr) {
		t.Fatalf("a token rotated under the CLI: err = %v, want ErrTransient", err)
	}
	if len(*got) != 0 {
		t.Fatalf("auth evidence filed for a rotated token: %+v", *got)
	}
	if res.SessionID != "s1" {
		t.Fatalf("SessionID = %q, want s1 — the retry resumes it", res.SessionID)
	}
	if len(tokens) != 1 || tokens[0] != "at.before" {
		t.Fatalf("spawn tokens = %v, want [at.before]", tokens)
	}

	_, _, got, err = runRotating(t, Task{}, map[int][]byte{1: resultLine(t, revokedRender, true)})
	var auth *ErrAuthFailed
	if !errors.As(err, &auth) {
		t.Fatalf("a token still on file: err = %v, want ErrAuthFailed", err)
	}
	if len(*got) != 1 {
		t.Fatalf("a dead credential left %d readings, want 1", len(*got))
	}
}

// TestExecute_rotationWrittenAfterTheRefusal: the measured order — the
// provider refuses the token at once, the runner writes the rotation a
// moment later. The node still fails transient, without evidence.
func TestExecute_rotationWrittenAfterTheRefusal(t *testing.T) {
	_, _, got, err := runRotatingLate(t, Task{}, map[int][]byte{1: resultLine(t, revokedRender, true)}, []int{1})
	var tr *ErrTransient
	if !errors.As(err, &tr) {
		t.Fatalf("a rotation written after the refusal: err = %v, want ErrTransient", err)
	}
	if len(*got) != 0 {
		t.Fatalf("auth evidence filed for a rotated token: %+v", *got)
	}
}

// TestExecute_formattingPassRetriesOnTheRenewedToken: the rotation lands
// during the two-pass formatting pass. The pass is retried in place, on a
// spawn that reads the new token, and the node answers.
func TestExecute_formattingPassRetriesOnTheRenewedToken(t *testing.T) {
	task := Task{OutputSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`), AllowedTools: []string{"Read"}}
	res, tokens, got, err := runRotating(t, task, map[int][]byte{
		1: resultLine(t, "working notes, no answer yet", false),
		2: resultLine(t, revokedRender, true),
		3: resultLine(t, `{"ok": true}`, false),
	}, 2)
	if err != nil {
		t.Fatalf("Execute: %v (spawn tokens %v)", err, tokens)
	}
	if ok, _ := res.Output["ok"].(bool); !ok {
		t.Fatalf("output = %v, want the answer of the retried pass", res.Output)
	}
	if want := []string{"at.before", "at.before", "at.after"}; strings.Join(tokens, " ") != strings.Join(want, " ") {
		t.Fatalf("spawn tokens = %v, want %v — the retry must read the renewed token", tokens, want)
	}
	if len(*got) != 0 {
		t.Fatalf("auth evidence filed for a rotated token: %+v", *got)
	}
}

// TestExecute_recoveryPassSeesTheRenewedToken: the rotation lands during the
// single-pass recovery formatting pass, which does not retry in place — the
// node fails transient, without evidence.
func TestExecute_recoveryPassSeesTheRenewedToken(t *testing.T) {
	task := Task{OutputSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)}
	_, tokens, got, err := runRotating(t, task, map[int][]byte{
		1: resultLine(t, "working notes, no answer yet", false),
		2: resultLine(t, revokedRender, true),
	}, 2)
	var tr *ErrTransient
	if !errors.As(err, &tr) {
		t.Fatalf("err = %v, want ErrTransient (spawn tokens %v)", err, tokens)
	}
	if len(*got) != 0 {
		t.Fatalf("auth evidence filed for a rotated token: %+v", *got)
	}
}

// A session that ends without a result message still names the credential
// it ran on: the spend a failed stream carries is booked on it. Red when the
// stream-error result drops the session fingerprint.
func TestExecute_aStreamErrorKeepsTheSessionFingerprint(t *testing.T) {
	res, _, _, err := runRotating(t, Task{}, map[int][]byte{1: nil})
	if err == nil {
		t.Fatal("a session that ended without a result succeeded")
	}
	if res.SessionFingerprint == "" {
		t.Errorf("the stream-error result names no session fingerprint (err %v)", err)
	}
}
