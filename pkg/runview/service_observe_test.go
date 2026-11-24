package runview

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// The spawn keeps the supervisors the resolver funds (ADR-161 decision 4): on
// a run whose credentials carry the stamp (ADR-131 §7), a supervisor on a key
// the run sealed is kept — the stamp refuses the pod's environment, never the
// run's own money — and one only the pod's environment would fund is dropped
// and named with the stamp's refusal. An unstamped run keeps the pod-funded
// supervisor.
//
// The run credentials lookups are the runner's (cmd/iterion installs them at
// boot): the claw arm reads the run's keys through them.
func TestFundedSupervisorSpecs_AStampedRunsOwnKeyServesItsSupervisor(t *testing.T) {
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	t.Cleanup(model.SetOAuthDirLookup(model.RunOAuthDirLookup))
	for _, name := range []string{
		"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_OAUTH_TOKEN",
		"ZAI_API_KEY", "ITERION_FORBID_SUBSCRIPTION_OAUTH", "ITERION_DEFAULT_SUPERVISOR_MODEL",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	specs := func() []supervise.Spec {
		return []supervise.Spec{{Name: "vendor", Model: "anthropic/claude-opus-4-8"}}
	}

	stamped := func(keys map[secrets.Provider]string) context.Context {
		return secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true, APIKeys: keys})
	}
	logs := &bytes.Buffer{}
	logger := iterlog.New(iterlog.LevelInfo, logs)

	kept := FundedSupervisorSpecs(stamped(map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"}), specs(), nil, "", logger)
	if len(kept) != 1 {
		t.Fatalf("a stamped run's supervisor on the run's own key was dropped: %+v (%s)", kept, logs)
	}

	logs.Reset()
	kept = FundedSupervisorSpecs(stamped(map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"}), specs(), nil, "", logger)
	if len(kept) != 0 {
		t.Fatalf("a stamped run kept a supervisor only the pod's environment funds: %+v", kept)
	}
	for _, want := range []string{`"vendor"`, "ambient-stamp"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the drop does not name %s: %s", want, logs)
		}
	}

	if kept := FundedSupervisorSpecs(context.Background(), specs(), nil, "", logger); len(kept) != 1 {
		t.Fatalf("an unstamped run dropped a supervisor the pod's key funds: %+v", kept)
	}
}
