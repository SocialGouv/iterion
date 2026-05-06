package docker

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestCapabilitiesAdvertisedFeatures(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	caps := d.Capabilities()

	if !caps.SupportsImage {
		t.Error("docker driver must advertise SupportsImage")
	}
	if !caps.SupportsMounts {
		t.Error("docker driver must advertise SupportsMounts")
	}
	if !caps.SupportsPostCreate {
		t.Error("docker driver must advertise SupportsPostCreate")
	}
	if !caps.SupportsRemoteUser {
		t.Error("docker driver must advertise SupportsRemoteUser")
	}
	// Phase 1 explicitly defers these.
	if caps.SupportsBuild {
		t.Error("Phase 1 docker driver must NOT advertise SupportsBuild")
	}
	if caps.SupportsNetworkPolicy {
		t.Error("Phase 1 docker driver must NOT advertise SupportsNetworkPolicy")
	}
}

func TestPrepareRejectsBuild(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	_, err := d.Prepare(context.Background(), sandbox.Spec{
		Mode:  sandbox.ModeInline,
		Build: &sandbox.Build{Dockerfile: "Dockerfile"},
	})
	if err == nil {
		t.Fatal("expected rejection of sandbox.Build (Phase 1 unsupported)")
	}
}

func TestPrepareRejectsMissingImage(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	_, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeInline})
	if err == nil {
		t.Fatal("expected rejection of inline spec without image")
	}
}

func TestPrepareValidatesSpec(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	_, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.Mode("nonsense")})
	if err == nil {
		t.Fatal("expected validation error for invalid mode")
	}
}

func TestContainerNameDeterministic(t *testing.T) {
	a := containerNameFor("run_123")
	b := containerNameFor("run_123")
	if a != b {
		t.Errorf("containerNameFor not deterministic: %q vs %q", a, b)
	}
	if a != "iterion-run_123" {
		t.Errorf("name = %q, want iterion-run_123", a)
	}
}

func TestContainerNameTruncatesLong(t *testing.T) {
	long := "run_" + string(make([]byte, 200))
	for i := range long[4:] {
		long = long[:4+i] + "x" + long[4+i+1:]
	}
	got := containerNameFor(long)
	if len(got) > 64 {
		t.Errorf("name %q exceeds 64 chars", got)
	}
}

func TestContainerShortIDTruncates(t *testing.T) {
	full := "abcdef0123456789abcdef"
	got := containerShortID(full)
	if got != "abcdef012345" {
		t.Errorf("shortID = %q, want abcdef012345", got)
	}
}

func TestContainerShortIDPassesThroughShort(t *testing.T) {
	got := containerShortID("abc")
	if got != "abc" {
		t.Errorf("shortID = %q, want abc", got)
	}
}

func TestStartRejectsForeignPreparedSpec(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	type otherPrepared struct{ name string }
	_ = otherPrepared{}
	// Use a noop-shaped PreparedSpec — DriverName != docker, must reject.
	_, err := d.Start(context.Background(), foreignPrepared{}, sandbox.RunInfo{})
	if err == nil {
		t.Fatal("expected rejection of foreign PreparedSpec")
	}
}

type foreignPrepared struct{}

func (foreignPrepared) DriverName() string { return "not-docker" }
