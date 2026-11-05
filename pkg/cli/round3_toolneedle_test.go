package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A tool node (no provider involved) whose command fails and whose stderr
// carries a common HTTP-client wording.
func toolFailBot(stderr string) string {
	return `dsl: 2

schema out:
  ok: bool

tool call_api:
  command: "echo '` + stderr + `' >&2; exit 1"
  output: out

workflow api:
  worktree: none
  sandbox: none
  entry: call_api
  call_api -> done
`
}

func TestR3ToolStderrNeedles(t *testing.T) {
	cases := []string{
		"AxiosError: Request failed with status code 400",
		"BadRequestError: invalid payload (http-errors)",
		"curl: (22) The requested URL returned error: 400",
		"API error 401: invalid api key",
		"API error 403: forbidden",
		"API error 404: model_not_found",
	}
	for i, stderr := range cases {
		t.Run(stderr, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			r := r3RunBot(t, r3WriteBot(t, toolFailBot(stderr)), "tool-needle")
			starts := 0
			var recov []string
			for _, ev := range r.events {
				if ev.Type == store.EventNodeStarted && ev.NodeID == "call_api" {
					starts++
				}
				if strings.Contains(string(ev.Type), "recover") {
					recov = append(recov, string(ev.Type)+":"+r3ToString(ev.Data["code"])+":"+r3ToString(ev.Data["action"]))
				}
			}
			code := r.run.FailureCode
			if r.err == nil || code != store.FailureExecutionFailed || starts != 2 || !retrypolicy.AutoResumable(code) {
				t.Errorf("tool failed classification: code=%s starts=%d err=%v", code, starts, r.err)
			}
			t.Logf("case %d stderr=%q", i, stderr)
			t.Logf("  status=%s code=%s disposition=%q autoResumable=%v deterministic=%v tool_starts=%d recovery=%v",
				r.run.Status, code, retrypolicy.Classify(code), retrypolicy.AutoResumable(code), retrypolicy.IsDeterministic(code), starts, recov)
			if idx := strings.Index(r.run.Error, "fix the provider"); idx >= 0 {
				t.Logf("  persisted hint: %q", r3Truncate(r.run.Error[idx:], 160))
			}
		})
	}
}

func r3ToString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(r3FmtAny(v), "\n", " "), "  ", " "))
}

func r3Truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// A tool command whose own network I/O fails transiently keeps the network
// backoff: the tool type stops PROVIDER readings of its stderr, not the
// network one. The command fails twice with the network wording, then
// succeeds, so the run must finish on the third start.
func TestR4ToolNetworkFailureKeepsTheNetworkBackoff(t *testing.T) {
	for name, stderr := range map[string]string{
		"reset": "curl: (56) Recv failure: Connection reset by peer",
		"dns":   "dial tcp: lookup api.github.com: i/o timeout",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			dir := t.TempDir()
			counter := filepath.Join(dir, "count")
			script := filepath.Join(dir, "tool.sh")
			body := "#!/bin/sh\nn=$(cat '" + counter + "' 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > '" + counter + "'\n" +
				"if [ $n -lt 3 ]; then echo '" + stderr + "' >&2; exit 1; fi\necho '{\"ok\":true}'\n"
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			bot := strings.Replace(toolFailBot(""), `command: "echo '' >&2; exit 1"`, `command: "sh '`+script+`'"`, 1)
			if !strings.Contains(bot, script) {
				t.Fatal("fixture: the tool command was not replaced")
			}
			r := r3RunBot(t, r3WriteBot(t, bot), "tool-network")
			starts := 0
			var codes []string
			for _, ev := range r.events {
				if ev.Type == store.EventNodeStarted && ev.NodeID == "call_api" {
					starts++
				}
				if strings.Contains(string(ev.Type), "recover") {
					codes = append(codes, r3ToString(ev.Data["code"]))
				}
			}
			if r.err != nil || r.run.Status != store.RunStatusFinished || starts != 3 {
				t.Fatalf("a transient network failure of a tool stopped the run: status=%s code=%s starts=%d recovery=%v err=%v", r.run.Status, r.run.FailureCode, starts, codes, r.err)
			}
			network := 0
			for _, code := range codes {
				switch code {
				case string(store.FailureNetworkTransient):
					network++
				case "":
				default:
					t.Errorf("recovery code = %q, want NETWORK_TRANSIENT (all: %v)", code, codes)
				}
			}
			if network == 0 {
				t.Errorf("no NETWORK_TRANSIENT recovery recorded (all: %v)", codes)
			}
		})
	}
}
