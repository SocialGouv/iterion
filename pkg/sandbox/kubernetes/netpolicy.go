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

	labels := map[string]string{
		LabelManaged: "true",
		LabelRunID:   in.RunID,
	}
	if in.FriendlyName != "" {
		labels[LabelRunName] = in.FriendlyName
	}

	policy := map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      in.Name,
			"namespace": in.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"podSelector": map[string]any{
				"matchLabels": map[string]any{
					LabelRunID: in.RunID,
				},
			},
			"policyTypes": []any{"Egress", "Ingress"},
			"egress": []any{
				map[string]any{
					"to": []any{
						map[string]any{
							"ipBlock": map[string]any{
								"cidr": in.RunnerPodIP + "/32",
							},
						},
					},
				},
				map[string]any{
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
				},
			},
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
