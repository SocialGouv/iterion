package runview

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The process ledger is deliberately global (a laptop's whole world), so
// this test only ever writes readings that are inert unless the cap env is
// set — which t.Setenv scopes to the test.
func recordLocal(t *testing.T, util float64) {
	t.Helper()
	if err := processUsageStore().Record(t.Context(), usagecap.Key("", usagecap.ScopeLocal, ""), usagecap.Reading{
		Window:      usagecap.WindowSevenDay,
		Utilization: util,
		Status:      usagecap.StatusWarning,
		ResetsAt:    time.Now().UTC().Add(24 * time.Hour),
		ObservedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalUsagePreflight(t *testing.T) {
	t.Run("no cap configured", func(t *testing.T) {
		recordLocal(t, 0.99)
		if blocked, _ := LocalUsagePreflight(); blocked {
			t.Fatal("blocked with no cap configured — the feature must be inert until asked for")
		}
	})

	t.Run("over the cap", func(t *testing.T) {
		t.Setenv(usagecap.EnvWeek, "75")
		recordLocal(t, 0.99)
		blocked, reason := LocalUsagePreflight()
		if !blocked {
			t.Fatal("want the launch refused at 99% against a 75% cap")
		}
		if reason == "" {
			t.Error("a refusal must say why, and when the window reopens")
		}
	})

	t.Run("under the cap", func(t *testing.T) {
		t.Setenv(usagecap.EnvWeek, "75")
		recordLocal(t, 0.10)
		if blocked, reason := LocalUsagePreflight(); blocked {
			t.Fatalf("blocked at 10%%: %s", reason)
		}
	})

	// A cap nobody can read is not a cap: a malformed policy must not
	// silently degrade into "no cap" here either. Failing open is the
	// deliberate choice for the LAUNCH gate (the run itself still carries
	// the guard, and BuildExecutor refuses the run loudly), so this pins
	// the intent rather than an accident.
	t.Run("malformed policy does not block", func(t *testing.T) {
		t.Setenv(usagecap.EnvWeek, "not-a-number")
		recordLocal(t, 0.99)
		if blocked, _ := LocalUsagePreflight(); blocked {
			t.Fatal("a malformed policy must not block launches; BuildExecutor is what refuses the run")
		}
	})
}

// Recording is not enforcing (issue #629 pt 1). An in-process run on a
// host that configured no cap still has to put the provider's refusals on
// the ledger — the credential-tier skips read nothing else — so the guard
// exists whatever the policy says, and simply blocks nothing.
func TestResolveUsageGuard_RecordsEvidenceWithNoCapConfigured(t *testing.T) {
	g, err := resolveUsageGuard(ExecutorSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if g == nil {
		t.Fatal("want a guard with no cap configured: the ledger is what the credential skips read")
	}
	obs := time.Now().UTC()
	if d := g.Observe(usagecap.Reading{
		Window:     usagecap.WindowFrequency,
		Status:     usagecap.StatusRejected,
		ObservedAt: obs,
	}); d.Blocked {
		t.Fatal("an unconfigured cap must block nothing")
	}
	got, err := processUsageStore().Latest(t.Context(), usagecap.Key("", usagecap.ScopeLocal, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.Window == usagecap.WindowFrequency && r.Status == usagecap.StatusRejected {
			return
		}
	}
	t.Fatalf("ledger = %+v, want the frequency refusal recorded", got)
}
