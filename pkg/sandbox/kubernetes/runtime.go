// Package kubernetes implements iterion's sandbox driver for cloud
// (in-cluster) deployments.
//
// Topology: the iterion runner pod (deployed via the Helm chart)
// detects an in-cluster service account at start time and creates a
// **Pod sibling** in the same namespace for each iterion run. The
// runner's claude_code / tool / claw invocations stream through
// `kubectl exec` into the sibling pod; the workspace is provided via
// an emptyDir volume that an initContainer optionally clones from a
// git repo. Cleanup deletes the pod (and its emptyDir) on run exit.
//
// Rationale for shell-out vs client-go (per .plans §1b/§5): we mirror
// the DockerDriver convention (shell-out to `docker`/`podman`) so the
// iterion binary stays small and the surface stable. kubectl is a
// thin layer over the same in-cluster auth (mounted token at
// /var/run/secrets/kubernetes.io/serviceaccount/) and ships ~50 MB
// in the runtime image — small relative to the Go SDK alternative
// which transitively pulls 100+ MB of k8s deps.
//
// V1 deferments (tracked for Phase 5 V2):
//   - Per-run NetworkPolicy synthesis. Today the engine's CONNECT
//     proxy (Phase 3) provides egress filtering; in cloud mode the
//     proxy runs as a sidecar in the same pod or as a cluster
//     Service so the same allow/deny rules apply. NetworkPolicy
//     resources will tighten host-IP filtering when V2 lands.
//   - initContainer git-clone of RepoURL/RepoSHA. Today the runner
//     pod's WorkDir is bind-rsynced; V2 isolates each run with its
//     own checkout per the cloud-ready plan's lingering TODO.
//   - Image-pull secrets propagation when the workflow's image
//     lives in a private registry distinct from the runner's.
package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// kubeBinaryName is the kubectl CLI iterion shells out to. Hardcoded
// rather than configurable because the in-cluster runner pod ships a
// known kubectl in its image; users on local hosts who want to point
// iterion at a remote cluster should set ITERION_MODE=cloud and run
// the production image.
const kubeBinaryName = "kubectl"

// inClusterTokenPath is the canonical mount point for a pod's
// service-account token. Existence of this file is the cheapest
// "are we in-cluster" probe — the kubelet always mounts it for
// pods that haven't opted out via automountServiceAccountToken: false.
const inClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// inClusterNamespacePath holds the namespace the pod runs in. Used
// to scope sibling pod creation when the engine doesn't pass an
// explicit namespace.
const inClusterNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Detect reports whether this process can act as the kubernetes
// driver: kubectl on PATH and the in-cluster service-account token
// present. Returns the resolved namespace too — used by Driver as
// the default scope for new pods.
//
// Returns ("", "", error) when the host doesn't qualify as a k8s
// runner, with the error explaining which check failed so the
// factory can surface a clear ErrUnavailable to the user.
func Detect() (kubectl string, namespace string, err error) {
	binPath, lookupErr := exec.LookPath(kubeBinaryName)
	if lookupErr != nil {
		return "", "", fmt.Errorf("%s not found on PATH (did you build the runtime image with kubectl?)", kubeBinaryName)
	}
	if _, statErr := os.Stat(inClusterTokenPath); statErr != nil {
		return "", "", fmt.Errorf("not running in a kubernetes pod (no service account token at %s)", inClusterTokenPath)
	}
	nsBytes, readErr := os.ReadFile(inClusterNamespacePath)
	if readErr != nil {
		return "", "", fmt.Errorf("read namespace from %s: %w", inClusterNamespacePath, readErr)
	}
	ns := strings.TrimSpace(string(nsBytes))
	if ns == "" {
		return "", "", fmt.Errorf("empty namespace at %s", inClusterNamespacePath)
	}
	return binPath, ns, nil
}

// kubectlCmd wraps exec.Command(kubectl, args...) with LC_ALL=C so
// callers can branch on stderr substrings ("NotFound", "AlreadyExists")
// stably across user locales. Mirrors the gitCmd / runtimeCmd helpers.
func kubectlCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(kubeBinaryName, args...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "LANG=C")
	detachProcessGroup(cmd)
	return cmd
}

// kubectlCmdContext is the ctx-aware sibling for long-running ops
// (apply, delete, exec) that should respect run cancellation.
func kubectlCmdContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, kubeBinaryName, args...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "LANG=C")
	detachProcessGroup(cmd)
	return cmd
}

// applyManifest runs `kubectl apply -f -` with the given YAML on
// stdin. Returns the combined stdout+stderr on failure for
// diagnostic surfacing — kubectl writes the failure reason to
// stderr in a structured way ("Error from server (NotFound)") that
// callers can parse without re-issuing the request.
func applyManifest(ctx context.Context, namespace string, manifest []byte) error {
	cmd := kubectlCmdContext(ctx, "--namespace", namespace, "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl apply: %w\noutput: %s", err, string(out))
	}
	return nil
}

// deleteResource runs `kubectl delete <kind> <name> --namespace ...`.
// Treats NotFound as success — callers invoke it from defer paths
// where the resource may already be gone (a panicking iterion run
// can leak partial state).
func deleteResource(ctx context.Context, namespace, kind, name string) error {
	cmd := kubectlCmdContext(ctx, "--namespace", namespace, "delete", kind, name,
		"--ignore-not-found=true", "--wait=false")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Even with --ignore-not-found, delete returns non-zero on
		// permission errors, dial failures, etc. Surface those.
		return fmt.Errorf("kubectl delete %s/%s: %w\noutput: %s", kind, name, err, string(out))
	}
	return nil
}

// waitForPodRunning polls `kubectl wait` until the pod reaches the
// Ready condition or timeout. We use kubectl's built-in --timeout
// (the alternative — a manual polling loop — would re-implement the
// same logic without saving a process spawn).
func waitForPodRunning(ctx context.Context, namespace, podName string, timeoutSecs int) error {
	cmd := kubectlCmdContext(ctx, "--namespace", namespace,
		"wait", "--for=condition=Ready", fmt.Sprintf("pod/%s", podName),
		fmt.Sprintf("--timeout=%ds", timeoutSecs))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl wait pod/%s: %w\noutput: %s", podName, err, string(out))
	}
	return nil
}

// runtimeVersion returns kubectl's reported client version. Used by
// `iterion sandbox doctor` so operators see the kubectl binary
// they're shipping with the runner image.
func runtimeVersion() (string, error) {
	out, err := kubectlCmd("version", "--client", "--output=json").Output()
	if err != nil {
		return "", err
	}
	// Don't bother decoding the JSON — keep this dependency-free.
	// The doctor caller renders the raw string anyway.
	return strings.TrimSpace(string(out)), nil
}
