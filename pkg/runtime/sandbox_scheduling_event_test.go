package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

type fakeSchedulingDriver struct {
	sandbox.Driver
	policy string
	err    error
}

func (f fakeSchedulingDriver) SchedulingPolicy() (string, error) { return f.policy, f.err }

// A resume on another runner re-renders the pod under THAT runner's policy;
// the sandbox_started event is the only place the difference is visible, so
// it must carry the policy — and only when the driver reports one.
func TestSandboxStartedEventCarriesTheSchedulingPolicy(t *testing.T) {
	var got map[string]any
	emit := func(typ store.EventType, data map[string]any) error {
		if typ == store.EventSandboxStarted {
			got = data
		}
		return nil
	}
	spec := &sandbox.Spec{Image: "img"}

	emitSandboxStarted(nil, spec, "kubernetes", "workflow", schedulingSummary(fakeSchedulingDriver{policy: "requests cpu=2, spread=kubernetes.io/hostname"}), emit)
	if got["scheduling"] != "requests cpu=2, spread=kubernetes.io/hostname" {
		t.Fatalf("sandbox_started must record the policy, got %v", got)
	}

	got = nil
	emitSandboxStarted(nil, spec, "docker", "workflow", schedulingSummary(fakeSchedulingDriver{}), emit)
	if _, present := got["scheduling"]; present {
		t.Fatalf("no policy must not produce a scheduling field, got %v", got)
	}

	// A misconfigured policy is Start's error to report, not the event's.
	if s := schedulingSummary(fakeSchedulingDriver{err: errTestPolicy}); s != "" {
		t.Fatalf("a policy error must not be summarised, got %q", s)
	}
}

type testPolicyError struct{}

func (testPolicyError) Error() string { return "policy error" }

var errTestPolicy error = testPolicyError{}
