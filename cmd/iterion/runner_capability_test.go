package main

import (
	"strings"
	"testing"

	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
)

func TestRunnerPortCapabilityIsOptInAndBoundToDeploymentInputs(t *testing.T) {
	cfg := iterconfig.Defaults()
	cfg.NATS.URL = "nats://worker:password@nats.example:4222"
	cfg.NATS.Stream = natsq.StreamRuns
	conn := &natsq.Conn{}
	if _, enabled, err := runnerPortCapabilityFromEnv(cfg, conn, "mongo-sha256:test", 7); err != nil || enabled {
		t.Fatalf("unset capability census was not optional: enabled=%v err=%v", enabled, err)
	}
	t.Setenv("ITERION_PORT_CAPABILITY_ENABLED", "true")
	t.Setenv("ITERION_PORT_CAPABILITY_INSTANCE", "runner-a")
	t.Setenv("ITERION_PORT_CAPABILITY_ACCOUNT", "WORK")
	t.Setenv("ITERION_PORT_CAPABILITY_BUILD_DIGEST", "sha256:"+strings.Repeat("a", 64))
	t.Setenv("ITERION_PORT_CAPABILITY_AUTHORITY_EPOCH", "9")
	claim, enabled, err := runnerPortCapabilityFromEnv(cfg, conn, "mongo-sha256:test", 7)
	if err != nil || !enabled || claim.Principal != "worker" || claim.Instance != "runner-a" ||
		claim.Account != "WORK" || claim.AuthorityEpoch != 9 || claim.RunnerEpoch != 7 || claim.QueueVersion == 0 {
		t.Fatalf("capability claim = %+v enabled=%v err=%v", claim, enabled, err)
	}
	if err := claim.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerPortCapabilityRejectsMalformedOptInConfiguration(t *testing.T) {
	cfg := iterconfig.Defaults()
	cfg.NATS.URL = "nats://worker:password@nats.example:4222"
	cfg.NATS.Stream = natsq.StreamRuns
	t.Setenv("ITERION_PORT_CAPABILITY_ENABLED", "yes")
	t.Setenv("ITERION_PORT_CAPABILITY_ACCOUNT", "WORK")
	t.Setenv("ITERION_PORT_CAPABILITY_BUILD_DIGEST", "sha256:"+strings.Repeat("a", 64))
	if _, _, err := runnerPortCapabilityFromEnv(cfg, &natsq.Conn{}, "mongo-sha256:test", 7); err == nil {
		t.Fatal("missing authority epoch was accepted")
	}
	t.Setenv("ITERION_PORT_CAPABILITY_AUTHORITY_EPOCH", "9")
	t.Setenv("ITERION_PORT_CAPABILITY_ENABLED", "maybe")
	if _, _, err := runnerPortCapabilityFromEnv(cfg, &natsq.Conn{}, "mongo-sha256:test", 7); err == nil {
		t.Fatal("invalid capability enable flag was accepted")
	}
}
