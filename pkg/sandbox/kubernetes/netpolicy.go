package kubernetes

import (
	"encoding/json"
	"fmt"
)

// NetworkPolicyInput is the surface the driver hands to
// [BuildNetworkPolicy] when it provisions a per-run policy alongside
// the sibling pod.
//
// V1 generates a deny-everything-by-default policy with two
// exceptions: egress to the runner pod's IP (so the sibling can reach
// the iterion network proxy) and DNS to kube-dns. All other outbound
// traffic must be either application-layer policy-allowed (via the
// proxy) or simply rejected at the IP layer — defence in depth.
// An UNTRUSTED run (Untrusted below, ADR-162 decision 7) keeps only the
// first exception, port-scoped to the run's own listeners.
//
// Enforcement requires a CNI that honours NetworkPolicy resources
// (Calico, Cilium, weave-net, kube-router, …). kind's default
// kindnetd CNI does NOT enforce; the resource still applies cleanly,
// just silently ignored. See docs/sandbox.md § cloud for the CNI
// requirement.
type NetworkPolicyInput struct {
	Namespace    string
	Name         string // typically the same as the pod name
	RunID        string
	FriendlyName string
	// RunnerPodIP is the IP of the runner pod that hosts the iterion
	// network proxy. The synthesised policy allows egress to this IP
	// (any port) so the sibling can reach the proxy regardless of
	// which port the proxy ended up bound to.
	RunnerPodIP string
	// Untrusted narrows the policy to ADR-162 decision 7's floor for a
	// run whose workspace code the engine does not trust (the fork
	// lane): the runner pod is reachable on EXACTLY EgressPorts and
	// cluster DNS is DROPPED — the proxy resolves runner-side and the
	// container reaches it by the pod IP the proxy endpoint already
	// advertises, so a DNS round-trip is a road out that names nothing
	// this run owns.
	Untrusted bool
	// EgressPorts are the runner-pod ports the untrusted run may reach:
	// the proxy's port first, then the run's own listener ports (an
	// ask-user listener bound before the policy was written). Required
	// and validated when Untrusted holds; ignored otherwise.
	EgressPorts []int
	// Owner, when valid, is stamped as the policy's ownerReferences so
	// the cluster cascade-GCs it with its owner (the runner pod). See
	// ADR-070.
	Owner *OwnerReference
}

// BuildNetworkPolicy renders a JSON NetworkPolicy resource that scopes
// the sibling pod's egress to (a) the runner pod IP and (b) cluster
// DNS. Inbound is locked down to the same runner pod IP — all
// kubectl exec traffic transits the API server, so this doesn't break
// the iterion run/exec/log path.
func BuildNetworkPolicy(in NetworkPolicyInput) ([]byte, error) {
	if in.Namespace == "" {
		return nil, fmt.Errorf("kubernetes: netpolicy: namespace is required")
	}
	if in.Name == "" {
		return nil, fmt.Errorf("kubernetes: netpolicy: name is required")
	}
	if in.RunID == "" {
		return nil, fmt.Errorf("kubernetes: netpolicy: runID is required")
	}
	if in.RunnerPodIP == "" {
		return nil, fmt.Errorf("kubernetes: netpolicy: RunnerPodIP is required (read from %s downward API)", PodIPEnvVar)
	}
	if in.Untrusted {
		// Fail closed: an untrusted policy that named no port would
		// degrade to any-port egress toward the runner pod — every other
		// run's listener included — the exact surface the floor exists
		// to close.
		if len(in.EgressPorts) == 0 {
			return nil, fmt.Errorf("kubernetes: netpolicy: an untrusted run's policy must name the runner pod's egress ports (the proxy's, the run's own listeners')")
		}
		for _, p := range in.EgressPorts {
			if p < 1 || p > 65535 {
				return nil, fmt.Errorf("kubernetes: netpolicy: egress port %d is not a port", p)
			}
		}
	}

	labels := map[string]string{
		LabelManaged: "true",
		LabelRunID:   in.RunID,
	}
	if in.FriendlyName != "" {
		labels[LabelRunName] = in.FriendlyName
	}

	meta := map[string]any{
		"name":      in.Name,
		"namespace": in.Namespace,
		"labels":    labels,
	}
	withOwner(meta, in.Owner)

	// The runner-pod egress rule. A trusted run reaches the runner pod on
	// any port (today's shape — the proxy's port is not known here); an
	// untrusted run's rule carries its EXACT port scope, every port
	// TCP (the proxy is an HTTP CONNECT proxy, the ask-user listener HTTP).
	runnerRule := map[string]any{
		"to": []any{
			map[string]any{
				"ipBlock": map[string]any{
					"cidr": in.RunnerPodIP + "/32",
				},
			},
		},
	}
	if in.Untrusted {
		ports := make([]any, 0, len(in.EgressPorts))
		for _, p := range in.EgressPorts {
			ports = append(ports, map[string]any{"protocol": "TCP", "port": p})
		}
		runnerRule["ports"] = ports
	}
	// kube-dns serves the trusted run's own resolutions. An untrusted
	// run gets NO DNS rule: the proxy resolves on the runner side and the
	// container reaches it by the pod IP, so the resolver is one more
	// reachable service the floor refuses to name.
	var egress []any
	egress = append(egress, runnerRule)
	if !in.Untrusted {
		egress = append(egress, map[string]any{
			"to": []any{
				map[string]any{
					"namespaceSelector": map[string]any{
						"matchLabels": map[string]any{
							"kubernetes.io/metadata.name": "kube-system",
						},
					},
					"podSelector": map[string]any{
						"matchLabels": map[string]any{
							"k8s-app": "kube-dns",
						},
					},
				},
			},
			"ports": []any{
				map[string]any{"protocol": "UDP", "port": 53},
				map[string]any{"protocol": "TCP", "port": 53},
			},
		})
	}

	policy := map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata":   meta,
		"spec": map[string]any{
			"podSelector": map[string]any{
				"matchLabels": map[string]any{
					LabelRunID: in.RunID,
				},
			},
			"policyTypes": []any{"Egress", "Ingress"},
			"egress":      egress,
			"ingress": []any{
				map[string]any{
					"from": []any{
						map[string]any{
							"ipBlock": map[string]any{
								"cidr": in.RunnerPodIP + "/32",
							},
						},
					},
				},
			},
		},
	}

	return json.MarshalIndent(policy, "", "  ")
}
