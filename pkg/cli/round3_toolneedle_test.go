package cli_test

import (
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
