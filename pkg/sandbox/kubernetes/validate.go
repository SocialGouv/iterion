package kubernetes

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// ValidateSpec runs the kubernetes driver's side-effect-free spec
// validation — the exact constraints [Driver.Prepare] enforces before
// allocating a pod, factored out so callers can verify a spec's
// cloud-compatibility WITHOUT an in-cluster kubectl or a live API
// server.
//
// It is host-independent: it shells out to nothing and reads no cluster
// state, so `iterion sandbox doctor --strict --target cloud` can run it
// from a developer laptop to catch misconfigurations that would only
// otherwise surface 30s into a cloud run. The checks mirror the driver's
// hard constraints:
//
//   - sandbox.build is local-only (cloud references pre-built images);
//   - an image ref is required (no build-at-run-start);
//   - host_state=auto cannot bind a host filesystem that does not exist
//     in a pod — the host_state-vs-k8s mutual exclusion;
//   - a numeric user is required because every sibling pod runs with
//     runAsNonRoot=true.
//
// Returns nil when the spec would be accepted by [Driver.Prepare].
func ValidateSpec(spec sandbox.Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if spec.Build != nil {
		return fmt.Errorf("kubernetes: sandbox.build is local-only; cloud workflows must reference a pre-built image via sandbox.image (build via CI and pin by digest)")
	}
	if spec.Image == "" {
		return fmt.Errorf("kubernetes: sandbox.image is required; declare an image: field or use mode=auto with a .devcontainer/devcontainer.json")
	}
	if spec.HostState == sandbox.HostStateAuto {
		return fmt.Errorf("kubernetes: sandbox.host_state=auto is not supported on the kubernetes driver (no host filesystem to bind); set host_state: none on the workflow, pass --sandbox-host-state=none, or set ITERION_SANDBOX_HOST_STATE=none for cloud runs")
	}
	if spec.User == "" {
		return fmt.Errorf("kubernetes: sandbox.user is required (form: \"uid\" or \"uid:gid\", numeric); the driver enforces runAsNonRoot=true and most base images (alpine, debian) default to root, so kubelet will refuse the container with a cryptic error if no non-zero user is specified")
	}
	if _, _, ok := parseUserSpec(spec.User); !ok {
		return fmt.Errorf("kubernetes: sandbox.user %q must be numeric (form: \"uid\" or \"uid:gid\"); image USER is not introspected", spec.User)
	}
	return nil
}

// runKubectlProbe is the indirection unit tests overwrite to mock
// kubectl invocations. Production uses the real ctx-aware exec wrapper.
var runKubectlProbe = func(ctx context.Context, args ...string) ([]byte, error) {
	return kubectlCmdContext(ctx, args...).CombinedOutput()
}

// PingContext probes whether a usable kubernetes context is selected and
// the API server is reachable — the strict-doctor check for "K8s without
// cluster context". It covers two host topologies:
//
//   - In-cluster (the production runner pod): the service-account mount
//     IS the context. [Detect] verifies kubectl + the namespace mount;
//     we then confirm the API server answers via `kubectl cluster-info`.
//   - Out-of-cluster (an operator validating a cloud workflow from their
//     laptop): there is no service-account mount, so fall back to the
//     kubeconfig — `kubectl config current-context` (is a context even
//     selected?) then `kubectl cluster-info` (does the API answer?).
//
// Returns the resolved context label + namespace on success. The error
// distinguishes "kubectl missing", "no context selected", and "API
// unreachable" so the doctor can print a targeted remediation.
//
// envtrust.LookPath, not exec.LookPath, like [Detect]: the binary this
// probe then shells out to is the driver's kubectl, an authority question
// a PATH a project .env planted must not answer (#2876, the class #2809
// closed site by site).
func PingContext(ctx context.Context) (kctx, namespace string, err error) {
	if _, lookErr := envtrust.LookPath(kubeBinaryName); lookErr != nil {
		return "", "", fmt.Errorf("%s not found on PATH (install kubectl or build the runtime image with it)", kubeBinaryName)
	}
	// Prefer the in-cluster path: if the service-account mount is present
	// the context is implicit and Detect resolves the namespace.
	if _, ns, detErr := Detect(); detErr == nil {
		if out, infoErr := runKubectlProbe(ctx, "cluster-info"); infoErr != nil {
			return "", ns, fmt.Errorf("in-cluster API server unreachable: %v\noutput: %s", infoErr, strings.TrimSpace(string(out)))
		}
		return "in-cluster service account", ns, nil
	}
	// Out-of-cluster: consult the kubeconfig.
	ctxOut, ctxErr := runKubectlProbe(ctx, "config", "current-context")
	current := strings.TrimSpace(string(ctxOut))
	if ctxErr != nil || current == "" {
		return "", "", fmt.Errorf("no kubernetes context selected; choose one with `kubectl config use-context <name>`, or run the doctor on the in-cluster runner")
	}
	if out, infoErr := runKubectlProbe(ctx, "cluster-info"); infoErr != nil {
		return current, "", fmt.Errorf("context %q selected but API server unreachable: %v\noutput: %s", current, infoErr, strings.TrimSpace(string(out)))
	}
	return current, "", nil
}
