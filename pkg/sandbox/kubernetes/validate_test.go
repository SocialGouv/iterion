package kubernetes

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    sandbox.Spec
		wantErr string // substring; "" means expect nil
	}{
		{
			name: "valid",
			spec: sandbox.Spec{Mode: sandbox.ModeInline, Image: "ghcr.io/x/y:1", User: "1000:1000"},
		},
		{
			name:    "build rejected",
			spec:    sandbox.Spec{Mode: sandbox.ModeInline, Build: &sandbox.Build{Dockerfile: "Dockerfile"}, User: "1000"},
			wantErr: "local-only",
		},
		{
			name:    "image required",
			spec:    sandbox.Spec{Mode: sandbox.ModeAuto, User: "1000"},
			wantErr: "sandbox.image is required",
		},
		{
			name:    "host_state auto rejected",
			spec:    sandbox.Spec{Mode: sandbox.ModeInline, Image: "img:1", User: "1000", HostState: sandbox.HostStateAuto},
			wantErr: "host_state=auto is not supported",
		},
		{
			name:    "user required",
			spec:    sandbox.Spec{Mode: sandbox.ModeInline, Image: "img:1"},
			wantErr: "sandbox.user is required",
		},
		{
			name:    "user must be numeric",
			spec:    sandbox.Spec{Mode: sandbox.ModeInline, Image: "img:1", User: "node"},
			wantErr: "must be numeric",
		},
		{
			name:    "image XOR build (spec.Validate path)",
			spec:    sandbox.Spec{Mode: sandbox.ModeInline, Image: "img:1", Build: &sandbox.Build{Dockerfile: "Dockerfile"}, User: "1000"},
			wantErr: "mutually exclusive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSpec(tc.spec)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateSpec() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateSpec() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateSpecMatchesPrepare guards the refactor: a spec ValidateSpec
// rejects must also be rejected by Driver.Prepare (Prepare delegates to
// ValidateSpec), and a spec it accepts must reach the Prepared result.
func TestValidateSpecMatchesPrepare(t *testing.T) {
	d := &Driver{namespace: "test"}

	bad := sandbox.Spec{Mode: sandbox.ModeInline, Image: "img:1", User: "node"}
	if ValidateSpec(bad) == nil {
		t.Fatal("precondition: ValidateSpec should reject a non-numeric user")
	}
	if _, err := d.Prepare(context.Background(), bad); err == nil {
		t.Error("Prepare must reject what ValidateSpec rejects")
	}

	good := sandbox.Spec{Mode: sandbox.ModeInline, Image: "img:1", User: "1000:1000"}
	if err := ValidateSpec(good); err != nil {
		t.Fatalf("precondition: ValidateSpec should accept a good spec, got %v", err)
	}
	if _, err := d.Prepare(context.Background(), good); err != nil {
		t.Errorf("Prepare must accept what ValidateSpec accepts, got %v", err)
	}
}

// TestPingContextWithStub exercises the out-of-cluster kubeconfig branch
// via the runKubectlProbe seam. It only runs when kubectl resolves on
// PATH AND the host is not in-cluster (Detect fails), otherwise the
// LookPath / Detect calls — which the seam does not cover — short-circuit
// before the stub is consulted.
func TestPingContextWithStub(t *testing.T) {
	if _, _, err := Detect(); err == nil {
		t.Skip("running in-cluster; out-of-cluster kubeconfig branch not exercised")
	}

	old := runKubectlProbe
	defer func() { runKubectlProbe = old }()
	var probed Kubectl
	runKubectlProbe = func(ctx context.Context, kubectl Kubectl, args ...string) ([]byte, error) {
		probed = kubectl
		if len(args) >= 2 && args[0] == "config" && args[1] == "current-context" {
			return []byte("kind-iterion\n"), nil
		}
		return []byte("Kubernetes control plane is running\n"), nil
	}

	kubectl, kctx, _, err := PingContext(context.Background())
	if err != nil {
		// kubectl absent → LookPath guard fires before the stub. Accept
		// that as an environment skip rather than a failure.
		t.Skipf("kubectl not on PATH (%v); seam not reached", err)
	}
	if kctx != "kind-iterion" {
		t.Errorf("PingContext context = %q, want kind-iterion", kctx)
	}
	// The ping resolves the binary ONCE and hands it on: the probe it drives
	// runs that one, and so does the doctor's node listing after it. Red when
	// the probe re-resolves the bare name instead of taking what it is given.
	if probed != kubectl || kubectl == "" || kubectl == kubeBinaryName {
		t.Errorf("the probe ran %q while PingContext resolved %q — a lookup answered twice is a lookup the live PATH can answer differently the second time", probed, kubectl)
	}
}
