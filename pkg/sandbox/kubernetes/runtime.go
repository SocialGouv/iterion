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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// kubeBinaryName is the kubectl CLI iterion shells out to. Hardcoded
// rather than configurable because the in-cluster runner pod ships a
// known kubectl in its image; users on local hosts who want to point
// iterion at a remote cluster should set ITERION_MODE=cloud and run
// the production image.
const kubeBinaryName = "kubectl"

// inClusterNamespacePath holds the namespace the pod runs in. Used
// to scope sibling pod creation when the engine doesn't pass an
// explicit namespace, and as the cheapest "are we in-cluster" probe
// — the kubelet mounts it for every pod that hasn't opted out via
// automountServiceAccountToken: false.
const inClusterNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Detect reports whether this process can act as the kubernetes
// driver: kubectl on PATH and the in-cluster service-account token
// present + readable. Returns the resolved namespace too — used by
// Driver as the default scope for new pods.
//
// Returns ("", "", error) when the host doesn't qualify as a k8s
// runner, with the error explaining which check failed so the
// factory can surface a clear ErrUnavailable to the user.
func Detect() (kubectl string, namespace string, err error) {
	binPath, lookupErr := exec.LookPath(kubeBinaryName)
	if lookupErr != nil {
		return "", "", fmt.Errorf("%s not found on PATH (did you build the runtime image with kubectl?)", kubeBinaryName)
	}
	// Read the namespace file directly — its presence implies we're
	// in a pod with the standard service-account mount, and an
	// unreadable namespace surfaces a real error rather than a
	// misleading "not in a pod" for an unrelated permission issue.
	nsBytes, readErr := os.ReadFile(inClusterNamespacePath)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return "", "", fmt.Errorf("not running in a kubernetes pod (no service account mount at %s)", inClusterNamespacePath)
		}
		return "", "", fmt.Errorf("read namespace from %s: %w", inClusterNamespacePath, readErr)
	}
	ns := strings.TrimSpace(string(nsBytes))
	if ns == "" {
		return "", "", fmt.Errorf("empty namespace at %s", inClusterNamespacePath)
	}
	return binPath, ns, nil
}

// kubectlCmdContext wraps exec.CommandContext(kubectl, args...) with
// LC_ALL=C so callers can branch on stderr substrings ("NotFound",
// "AlreadyExists") stably across user locales. Mirrors the gitCmd /
// runtimeCmdContext helpers. Used for long-running ops (apply, delete,
// exec) that should respect run cancellation.
func kubectlCmdContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, kubeBinaryName, args...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "LANG=C")
	proc.DetachProcessGroup(cmd)
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
		// `kubectl wait` only says the condition never came. The cluster
		// knows WHY, and the reason decides between two different fixes: a
		// resource request no node can hold (Unschedulable: Insufficient
		// memory) and a slow or broken image pull. Read it on the error
		// path only.
		return fmt.Errorf("kubectl wait pod/%s: %w\noutput: %s%s", podName, err, string(out), podScheduledReason(ctx, namespace, podName))
	}
	return nil
}

// NodeLabelCoverage counts the cluster's nodes and those carrying the label
// key, for the doctor: a topology spread over a key some nodes lack
// excludes those nodes from scheduling, soft constraint or not, and a key
// no node carries leaves every pod Pending. Listing nodes is a
// cluster-scoped permission the chart's namespaced runner Role does not
// grant on purpose, so the error must carry kubectl's reason (Forbidden)
// for the doctor to say why it could not answer.
func NodeLabelCoverage(ctx context.Context, key string) (labelled, total int, err error) {
	cmd := kubectlCmdContext(ctx, "get", "nodes", "-o", "json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, fmt.Errorf("kubectl get nodes: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var nodes struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &nodes); err != nil {
		return 0, 0, fmt.Errorf("kubectl get nodes: decode: %w", err)
	}
	for _, n := range nodes.Items {
		if _, ok := n.Metadata.Labels[key]; ok {
			labelled++
		}
	}
	return labelled, len(nodes.Items), nil
}

// podScheduledReason renders the pod's PodScheduled condition (status,
// reason, message) as a "\nscheduling: …" suffix for a wait error — "" when
// it cannot be read. Best-effort: it decorates an error, it never creates one.
func podScheduledReason(ctx context.Context, namespace, podName string) string {
	cmd := kubectlCmdContext(ctx, "--namespace", namespace, "get", "pod", podName, "-o",
		`jsonpath={.status.conditions[?(@.type=="PodScheduled")].status}{" "}{.status.conditions[?(@.type=="PodScheduled")].reason}{": "}{.status.conditions[?(@.type=="PodScheduled")].message}`)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// A scheduled pod has no reason and no message: the jsonpath's literal
	// ": " then dangles after the status and is dropped here.
	reason := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(out)), ":"))
	if reason == "" {
		return ""
	}
	return "\nscheduling: " + reason
}
