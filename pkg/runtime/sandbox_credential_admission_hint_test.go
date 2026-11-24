package runtime

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestForkRunWithAnAsFileCredentialAloneIsStillRefusedWithAnHonestHint pins
// the TWO honest halves of the refusal's remedy list: a credential declared
// `as: file` reaches the container's filesystem — not the backend CLI's
// auth — so the refusal STANDS for a run whose only credential is one (the
// node would die mid-run on the backend's opaque "not logged in" — a false
// admission costs the run's hours, exactly what the admission exists to
// prevent), and the hint stops naming it as a way out (it used to send the
// operator into a remedy that cannot lift the refusal). Red when someone
// re-honors the file channel in the admission (a false admission: remove
// the test's first arm) or re-names the inert remedy in the hint (second
// arm).
func TestForkRunWithAnAsFileCredentialAloneIsStillRefusedWithAnHonestHint(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("HOME", t.TempDir())
	wf := credentialWorkflow("review", "claude_code", "none")
	wf.Secrets = map[string]*ir.Secret{
		"anthropic_api_key": {
			Value: "sk-ant-probe-0123456789abcdef",
			As:    "file",
			Env:   "ANTHROPIC_API_KEY",
		},
	}
	err := credentialAdmissionRun(t, wf, store.RunTrustFork, nil)
	var rerr *RuntimeError
	if !errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxCredentialUnreachable {
		t.Fatalf("as:file-only fork run = %v, want the typed credential refusal — the mounted file cannot authenticate the backend CLI", err)
	}
	if strings.Contains(rerr.Hint, "declare the credential") {
		t.Fatalf("the hint offers the `as: file` declaration as a way out: %q", rerr.Hint)
	}
	if !strings.Contains(rerr.Hint, "does not lift this refusal") {
		t.Fatalf("the hint stops explaining why the file channel cannot serve: %q", rerr.Hint)
	}
	if !strings.Contains(rerr.Hint, "ANTHROPIC_API_KEY") || !strings.Contains(rerr.Hint, "host_state: auto") {
		t.Fatalf("the hint names only remedies that cross: %q", rerr.Hint)
	}
}
