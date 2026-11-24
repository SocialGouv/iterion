package config

import (
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The operator-grade config surface reads through envtrust: this package
// IS the operator's configuration, and a value a project `.env` planted —
// what an older binary's loader could still leave in a child, the current
// loaders refusing the grade at the plant — is not the operator. The H-10
// chain this closes: a planted ITERION_SANDBOX_OVERRIDE=none used to ride
// cfg.Sandbox.Override into runtime.WithSandboxOverride, beating a bot's
// inline sandbox block; the loader refusal now closes the plant and this
// belt closes the stale-marker read.
func TestTheOperatorGradeReadsTheInheritedEnvironment(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "ITERION_SANDBOX_OVERRIDE,ITERION_JWT_SECRET,ITERION_NATS_URL")
	envtrust.ResetForTest()

	t.Setenv("ITERION_SANDBOX_OVERRIDE", "none")
	t.Setenv("ITERION_JWT_SECRET", "planted-secret")
	t.Setenv("ITERION_NATS_URL", "nats://repo-under-review.example:4222")

	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Sandbox.Override; got != "" {
		t.Errorf("cfg.Sandbox.Override = %q: a planted sandbox lift reached the configuration", got)
	}
	if got := cfg.Auth.JWTSecret; got != "" {
		t.Errorf("cfg.Auth.JWTSecret is set from a planted value: the session-signing secret is not a project's to choose")
	}
	if got := cfg.NATS.URL; got != "" {
		t.Errorf("cfg.NATS.URL = %q: a planted store wire reached the configuration", got)
	}

	// The control: the operator's own export — no planted marker — reads
	// exactly as before.
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()
	cfg, err = Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load (operator's own): %v", err)
	}
	if got := cfg.Sandbox.Override; got != "none" {
		t.Errorf("cfg.Sandbox.Override = %q: the operator's own export must keep answering", got)
	}
	if got := cfg.NATS.URL; got != "nats://repo-under-review.example:4222" {
		t.Errorf("cfg.NATS.URL = %q: the operator's own export must keep answering", got)
	}
}
