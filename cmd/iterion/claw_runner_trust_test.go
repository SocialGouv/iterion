package main

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The runner side of the carrier (ADR-162): the in-container claw runner
// rebuilds the task from the wire, checks it against the launcher's pre-task
// announcement, and carries an untrusted task's trust into its own
// environment, which every process it starts inherits. Each row reaches the
// check and reports which side of it the runner ended on: the refusal, or the
// model stage right after it (this test registers no provider, so a runner
// past the check dies resolving the model).
func TestClawRunner_CarriesAndChecksTheWorkspaceTrust(t *testing.T) {
	const mismatch = "workspace trust mismatch"
	announce := func(t *testing.T, trust store.RunTrust) []delegate.Envelope {
		t.Helper()
		env, err := delegate.NewWorkspaceTrustEnvelope(trust)
		if err != nil {
			t.Fatal(err)
		}
		return []delegate.Envelope{env}
	}
	for _, row := range []struct {
		name      string
		announced []delegate.Envelope
		carried   store.RunTrust
		refused   bool
		exported  string
	}{
		{name: "a trusted task, nothing announced (the control)", carried: store.RunTrustDefault},
		{name: "a fork task, announced", announced: announce(t, store.RunTrustFork), carried: store.RunTrustFork, exported: "fork"},
		// A task that lost the field on the way — an older launcher's IOTask,
		// a lying peer — under a fork announcement.
		{name: "a fork announcement, a task read as trusted", announced: announce(t, store.RunTrustFork), carried: store.RunTrustDefault, refused: true},
		{name: "announcement and task naming different trusts", announced: announce(t, store.RunTrust("vendored")), carried: store.RunTrustFork, refused: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			// The runner exports into its own process environment; t.Setenv
			// restores it after the row, and starts the row from no carrier.
			t.Setenv(envtrust.EnvRunTrust, "")
			if err := os.Unsetenv(envtrust.EnvRunTrust); err != nil {
				t.Fatal(err)
			}
			res := driveClawRunner(t, row.announced, delegate.IOTask{NodeID: "x", Model: "bogus-provider/nope", WorkspaceTrust: row.carried})
			if res.Error == "" {
				t.Fatal("the runner ended without an error — neither the refusal nor the model stage was reached, so this row proves nothing")
			}
			if refused := strings.Contains(res.Error, mismatch); refused != row.refused {
				t.Fatalf("refused = %v, want %v (runner said %q)", refused, row.refused, res.Error)
			}
			if row.refused {
				if got, found := os.LookupEnv(envtrust.EnvRunTrust); found {
					t.Errorf("a refused task still exported %s=%q", envtrust.EnvRunTrust, got)
				}
				return
			}
			got, found := os.LookupEnv(envtrust.EnvRunTrust)
			switch {
			case row.exported == "" && found:
				t.Errorf("a trusted task exported %s=%q — the trusted value is never written", envtrust.EnvRunTrust, got)
			case row.exported != "" && got != row.exported:
				t.Errorf("%s = %q (present=%v), want %q — what the runner starts would run as the tenant's own", envtrust.EnvRunTrust, got, found, row.exported)
			}
		})
	}
}
