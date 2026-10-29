package cloudpublisher

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestSubmitLaunch_RidesProvisionedEngineInjectedVars pins #2416 at the
// cloud admission: a launch through the strict API whose vars carry a
// provision's stamped engine-injected dials (the repo-bots integration's
// launch_vars: ITERION_VIBE_* — env-shaped names the engine's expansion
// answers, not DSL vars) is ADMITTED, the dials riding the wire to the
// runner, while a key that is neither a declared var nor engine-injected
// keeps #1757's synchronous refusal — before anything persists or queues.
func TestSubmitLaunch_RidesProvisionedEngineInjectedVars(t *testing.T) {
	f := newReviAcceptanceFixture(t)
	wf := compileTestSource(t, toolOnlyBot)
	ctx := store.WithIdentity(context.Background(), "team1", "webhook:cfg-1")
	spec := runview.LaunchSpec{
		FilePath: "tool.bot",
		Source:   toolOnlyBot,
		BotID:    "tool-bot",
		Vars: map[string]string{
			"ITERION_VIBE_BACKEND_EMIT":  "claw",
			"ITERION_VIBE_PROVIDER_EMIT": "openai_compatible",
		},
	}
	if _, err := f.pub.SubmitLaunch(ctx, "run-injected", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch = %v, want nil — the stamped dials are engine-injected, not unknown inputs", err)
	}
	if len(f.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(f.published))
	}
	if got := f.published[0].Vars["ITERION_VIBE_BACKEND_EMIT"]; got != "claw" {
		t.Errorf("wire vars = %v, want the dial riding to the runner", f.published[0].Vars)
	}

	// The typo class #1757 exists for keeps the refusal, and a refused
	// launch publishes nothing.
	spec.Vars = map[string]string{"ITERION_VIBE_BACKEND_EMIT": "claw", "nonsense_key": "x"}
	_, err := f.pub.SubmitLaunch(ctx, "run-typo", spec, wf, &runview.CompiledSource{Hash: "hash"})
	if err == nil || !strings.Contains(err.Error(), "nonsense_key") {
		t.Fatalf("SubmitLaunch = %v, want the refusal naming the unknown key", err)
	}
	if strings.Contains(err.Error(), "ITERION_VIBE_BACKEND_EMIT") {
		t.Fatalf("refusal = %v, must not name the engine-injected dial as unknown", err)
	}
	if len(f.published) != 1 {
		t.Fatalf("a refused launch published — published %d messages, want 1", len(f.published))
	}
}
