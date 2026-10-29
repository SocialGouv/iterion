package server

import "testing"

// #2644. pkg/trigger proves the two lanes that BUILD a launch plan carry a
// subscription's credential pins; this proves the one hop that turns a plan
// into a run — the LaunchSpec the publisher seals and a resume re-resolves
// from. The carrier that will finally WRITE those pins is the ticket's open
// product question (ARB-89); the conveyor it would write onto is not, and
// it was untested on all three of its copies. A dropped copy reads exactly
// like a subscription that pins nothing: an unresolvable pin produces no
// refusal anywhere downstream.

func TestTheTriggerSpineCarriesAPlansPinsOntoTheLaunchSpec(t *testing.T) {
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1"}, pub)
	plan := spinePlan("t1")
	plan.KeyOverrides = map[string]string{"anthropic": "key-pinned-by-the-operator"}
	plan.SecretOverrides = map[string]string{"forge_token": "sec-pinned-by-the-operator"}

	if _, err := s.triggerLauncher().Launch(spineCtx(t), plan); err != nil {
		t.Fatalf("Launch = %v, want nil", err)
	}
	spec := pub.lastSpec()
	if got := spec.KeyOverrides["anthropic"]; got != "key-pinned-by-the-operator" {
		t.Errorf("spec.KeyOverrides[anthropic] = %q, want the plan's pin — the run would seal the team's default key", got)
	}
	if got := spec.SecretOverrides["forge_token"]; got != "sec-pinned-by-the-operator" {
		t.Errorf("spec.SecretOverrides[forge_token] = %q, want the plan's pin", got)
	}
}
