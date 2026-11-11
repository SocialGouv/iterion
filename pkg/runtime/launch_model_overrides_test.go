package runtime

import (
	"context"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The launch's raw rows (store.Run.LaunchModelOverrides) are stamped once,
// and an engine that is not handed them — the runner picking up the
// publisher's queued document, a resume — keeps what the document holds:
// the subbot children of the run's tree read them from there.
func TestEngineKeepsTheLaunchRowsItWasNotHanded(t *testing.T) {
	ctx := context.Background()
	rows := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}

	t.Run("stamped by the option", func(t *testing.T) {
		s := tmpStore(t)
		if err := New(admissionWorkflow(), s, newStubExecutor(), WithLaunchModelOverrides(rows)).Run(ctx, "launch-rows-stamped", nil); err != nil {
			t.Fatal(err)
		}
		got, err := s.LoadRun(ctx, "launch-rows-stamped")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.LaunchModelOverrides, rows) {
			t.Fatalf("run launch rows = %+v, want the option's %+v", got.LaunchModelOverrides, rows)
		}
	})

	t.Run("kept on a pickup without the option", func(t *testing.T) {
		s := tmpStore(t)
		run, err := s.CreateRun(ctx, "launch-rows-kept", "admission", nil)
		if err != nil {
			t.Fatal(err)
		}
		run.LaunchModelOverrides = rows
		if err := s.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		// WithModelOverrides makes the engine stamp the document, so the
		// save that could drop the field runs.
		if err := New(admissionWorkflow(), s, newStubExecutor(), WithModelOverrides([]store.RunModelOverride{{Selector: "*", Model: "m"}})).Run(ctx, run.ID, nil); err != nil {
			t.Fatal(err)
		}
		got, err := s.LoadRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.LaunchModelOverrides, rows) {
			t.Fatalf("run launch rows = %+v after a pickup, want the stamped %+v", got.LaunchModelOverrides, rows)
		}
	})
}

// The routing-policy snapshot a subbot child's engine is handed (its
// root's) lands on the child's document, and an engine handed none — a
// resume — keeps what the document holds.
func TestEngineStampsTheRoutePolicyItIsHandedAndKeepsItOtherwise(t *testing.T) {
	ctx := context.Background()
	pol := &store.RunLLMRoutePolicy{Providers: []string{"openai"}}

	t.Run("stamped by the option", func(t *testing.T) {
		s := tmpStore(t)
		if err := New(admissionWorkflow(), s, newStubExecutor(), WithLLMRoutePolicy(pol)).Run(ctx, "route-policy-stamped", nil); err != nil {
			t.Fatal(err)
		}
		got, err := s.LoadRun(ctx, "route-policy-stamped")
		if err != nil {
			t.Fatal(err)
		}
		if got.LLMRoutePolicy == nil || !reflect.DeepEqual(got.LLMRoutePolicy.Providers, pol.Providers) {
			t.Fatalf("run policy = %+v, want the option's %+v", got.LLMRoutePolicy, pol)
		}
	})

	t.Run("kept on a pickup without the option", func(t *testing.T) {
		s := tmpStore(t)
		run, err := s.CreateRun(ctx, "route-policy-kept", "admission", nil)
		if err != nil {
			t.Fatal(err)
		}
		run.LLMRoutePolicy = pol
		if err := s.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := New(admissionWorkflow(), s, newStubExecutor(), WithModelOverrides([]store.RunModelOverride{{Selector: "*", Model: "m"}})).Run(ctx, run.ID, nil); err != nil {
			t.Fatal(err)
		}
		got, err := s.LoadRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LLMRoutePolicy == nil || !reflect.DeepEqual(got.LLMRoutePolicy.Providers, pol.Providers) {
			t.Fatalf("run policy = %+v after a pickup, want the stamped %+v", got.LLMRoutePolicy, pol)
		}
	})
}
