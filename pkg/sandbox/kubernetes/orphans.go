package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
)

// reapKinds is the set of per-run resource kinds the label reaper sweeps.
// Order matters only cosmetically; each kind is deleted independently by
// its iterion.io/run-id label, because the resources are owned by the
// RUNNER pod (not the sandbox pod), so deleting the sandbox pod does NOT
// cascade the Secrets/NetworkPolicy — the sweep must target all three.
var reapKinds = []string{"pod", "secret", "networkpolicy"}

// ManagedResource is a per-run kubernetes resource the reaper found via
// the iterion.io/managed label.
type ManagedResource struct {
	Kind  string // "pod" | "secret" | "networkpolicy"
	Name  string
	RunID string // value of the iterion.io/run-id label ("" when missing)
}

// k8sList is the minimal shape of a `kubectl get <kind> -o json` List we
// need: each item's name + labels. Everything else is ignored.
type k8sList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	} `json:"items"`
}

// parseManagedList extracts (name, run-id) tuples from a `kubectl get
// <kind> -o json` payload. Extracted for unit testing without a cluster.
func parseManagedList(kind string, data []byte) ([]ManagedResource, error) {
	var list k8sList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("kubernetes: parse %s list: %w", kind, err)
	}
	out := make([]ManagedResource, 0, len(list.Items))
	for _, it := range list.Items {
		if it.Metadata.Name == "" {
			continue
		}
		out = append(out, ManagedResource{
			Kind:  kind,
			Name:  it.Metadata.Name,
			RunID: it.Metadata.Labels[LabelRunID],
		})
	}
	return out, nil
}

// listManagedResources returns every resource of the given kind labelled
// iterion.io/managed=true in the namespace.
func listManagedResources(ctx context.Context, namespace, kind string) ([]ManagedResource, error) {
	cmd := kubectlCmdContext(ctx, "--namespace", namespace, "get", kind,
		"-l", LabelManaged+"=true", "-o", "json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kubernetes: list managed %ss: %w", kind, err)
	}
	return parseManagedList(kind, out)
}

// ReapOrphanResources force-deletes every iterion-managed sandbox pod,
// Secret and NetworkPolicy whose owning run the caller's isTerminal
// predicate marks as no longer active (or absent from the store). Returns
// the resources reaped and the first error encountered; later per-kind
// errors are swallowed so one Forbidden doesn't strand the rest.
//
// This is the boot/periodic reconcile for the kubernetes driver, the
// counterpart to docker's ReapOrphanContainers: a runner SIGKILLed /
// OOM-killed / node-evicted mid-run never runs Run.Cleanup, so its
// sandbox pod + both Secrets + NetworkPolicy leak. The self-terminating
// manifest (ownerReference + activeDeadlineSeconds) closes the
// runner-pod-deleted and idle-forever windows; this closes the rest —
// including the plaintext-credential Secret, which has no TTL of its own.
//
// isTerminal receives the iterion.io/run-id label value (empty when the
// label is missing — treat those as orphans too, a managed resource with
// no run owner). MUST be gated by the caller on the store's
// cross-process-lock authority so a server sharing a namespace with live
// runners never reaps an in-flight run's sandbox (see the runview
// wiring).
func ReapOrphanResources(ctx context.Context, namespace string, isTerminal func(runID string) bool) ([]ManagedResource, error) {
	if namespace == "" {
		return nil, fmt.Errorf("kubernetes: ReapOrphanResources: namespace is required")
	}
	if isTerminal == nil {
		return nil, fmt.Errorf("kubernetes: ReapOrphanResources: isTerminal predicate is required")
	}
	var reaped []ManagedResource
	var firstErr error
	for _, kind := range reapKinds {
		resources, err := listManagedResources(ctx, namespace, kind)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, r := range resources {
			if !isTerminal(r.RunID) {
				continue
			}
			if delErr := deleteResource(ctx, namespace, kind, r.Name); delErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("kubernetes: reap %s/%s: %w", kind, r.Name, delErr)
				}
				continue
			}
			reaped = append(reaped, r)
		}
	}
	return reaped, firstErr
}
