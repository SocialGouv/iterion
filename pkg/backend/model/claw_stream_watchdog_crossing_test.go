package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The stream-silence watchdog knobs cross as the host's own planted-as-unset
// answer, not the raw process env: the marks that say "a .env set this" do
// not cross, so a raw copy would let a project `.env` disarm the container's
// watchdog after the host refused it — or arm the twin when the host refused.
// The operator's own export crosses intact, and the crossing is EXPLICIT, an
// empty included, so the workspace .env the runner auto-loads cannot fill
// what the host refused (applyDotEnv honours LookupEnv).
func TestForwardableProviderEnv_CarriesTheHostsPlantedAnswerForTheWatchdog(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("ITERION_CLAW_STREAM_COLD_TIMEOUT", "0")
	t.Setenv("ITERION_CLAW_STREAM_IDLE_TIMEOUT", "0")
	envtrust.MarkPlanted("ITERION_CLAW_STREAM_COLD_TIMEOUT")
	envtrust.MarkPlanted("ITERION_CLAW_STREAM_IDLE_TIMEOUT")

	// The host reads planted-as-unset: both tiers fall back.
	hostCold, hostIdle := resolveClawStreamColdTimeout(), resolveClawStreamIdleTimeout()
	if hostCold != defaultClawStreamColdTimeout || hostIdle != defaultClawStreamIdleTimeout {
		t.Fatalf("a planted 0 read through on the host (cold=%s idle=%s): the liveness family reads planted-as-unset", hostCold, hostIdle)
	}

	// The crossing carries the host's answer — explicit empties, not the
	// planted bytes.
	env := envFor(t, context.Background())
	if got, ok := env["ITERION_CLAW_STREAM_COLD_TIMEOUT"]; !ok || got != "" {
		t.Errorf("the planted cold timeout crossed as %q (present=%v): the crossing carries the host's refusal as an explicit empty", got, ok)
	}
	if got, ok := env["ITERION_CLAW_STREAM_IDLE_TIMEOUT"]; !ok || got != "" {
		t.Errorf("the planted idle timeout crossed as %q (present=%v): the crossing carries the host's refusal as an explicit empty", got, ok)
	}

	// The twin, resolving what crossed, resolves what the host resolved.
	t.Setenv("ITERION_CLAW_STREAM_COLD_TIMEOUT", env["ITERION_CLAW_STREAM_COLD_TIMEOUT"])
	t.Setenv("ITERION_CLAW_STREAM_IDLE_TIMEOUT", env["ITERION_CLAW_STREAM_IDLE_TIMEOUT"])
	envtrust.ResetForTest()
	t.Setenv(envtrust.EnvPlantedNames, "")
	if got := resolveClawStreamColdTimeout(); got != hostCold {
		t.Errorf("in-container cold timeout = %s, host = %s", got, hostCold)
	}
	if got := resolveClawStreamIdleTimeout(); got != hostIdle {
		t.Errorf("in-container idle timeout = %s, host = %s", got, hostIdle)
	}
}
