package authority

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rbacItem(kind, namespace, name string) map[string]any {
	return map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": kind,
		"metadata": map[string]any{"namespace": namespace, "name": name, "uid": "uid-" + name,
			"resourceVersion": "17"}}
}

func rbacList(items ...map[string]any) []byte {
	encoded, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	return encoded
}

func rbacShim(t *testing.T, namespaced, cluster []byte) (binary, namespacePath, clusterPath string) {
	t.Helper()
	dir := t.TempDir()
	binary = filepath.Join(dir, "kubectl")
	namespacePath = filepath.Join(dir, "namespace.json")
	clusterPath = filepath.Join(dir, "cluster.json")
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = --namespace ]; then cat \"$ITERION_RBAC_NAMESPACE_FIXTURE\"; exit; fi\ndone\ncat \"$ITERION_RBAC_CLUSTER_FIXTURE\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(namespacePath, namespaced, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clusterPath, cluster, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_RBAC_NAMESPACE_FIXTURE", namespacePath)
	t.Setenv("ITERION_RBAC_CLUSTER_FIXTURE", clusterPath)
	return
}

func TestKubernetesRBACSnapshotKeepsNamespacedAndClusterBindings(t *testing.T) {
	role := rbacItem("Role", "trusted", "secret-reader")
	role["rules"] = []any{map[string]any{"apiGroups": []string{""}, "resources": []string{"secrets"},
		"verbs": []string{"get", "list"}}}
	binding := rbacItem("RoleBinding", "trusted", "worker-cross-access")
	binding["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "secret-reader"}
	binding["subjects"] = []any{map[string]any{"kind": "ServiceAccount", "name": "runner", "namespace": "worker"}}
	clusterRole := rbacItem("ClusterRole", "", "system:discovery")
	clusterRole["rules"] = []any{map[string]any{"verbs": []string{"get"}, "nonResourceURLs": []string{"/api"}}}
	clusterBinding := rbacItem("ClusterRoleBinding", "", "system:discovery")
	clusterBinding["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "system:discovery"}
	clusterBinding["subjects"] = []any{map[string]any{"kind": "Group", "name": "system:authenticated"}}
	binary, _, _ := rbacShim(t, rbacList(role, binding), rbacList(clusterRole, clusterBinding))
	snapshot, err := ReadRBACSnapshot(t.Context(), binary, "fixture-context", []string{"trusted"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Roles) != 2 || len(snapshot.Bindings) != 2 ||
		snapshot.Roles[0].Kind != "ClusterRole" || snapshot.Roles[0].Name != "system:discovery" ||
		snapshot.Bindings[1].Subjects[0].Namespace != "worker" {
		t.Fatalf("RBAC namespace or cluster evidence was lost: %+v", snapshot)
	}
}

func TestKubernetesRBACSnapshotRefusesPartialOrUnsupportedEvidence(t *testing.T) {
	validCluster := rbacList(rbacItem("ClusterRole", "", "system:discovery"))
	for name, namespaceOutput := range map[string][]byte{
		"continued list":         []byte(`{"apiVersion":"v1","kind":"List","metadata":{"continue":"next"},"items":[]}`),
		"foreign namespace":      rbacList(rbacItem("Role", "other", "reader")),
		"unknown kind":           rbacList(rbacItem("Policy", "trusted", "reader")),
		"missing role reference": rbacList(rbacItem("RoleBinding", "trusted", "broken")),
	} {
		t.Run(name, func(t *testing.T) {
			binary, _, _ := rbacShim(t, namespaceOutput, validCluster)
			if _, err := ReadRBACSnapshot(t.Context(), binary, "", []string{"trusted"}); err == nil {
				t.Fatal("incomplete Kubernetes RBAC evidence was accepted")
			}
		})
	}
	for _, namespaces := range [][]string{{}, {"trusted", "trusted"}, {"--all-namespaces"}} {
		if _, err := ReadRBACSnapshot(t.Context(), "kubectl", "", namespaces); err == nil {
			t.Fatal("invalid RBAC namespace scope reached kubectl")
		}
	}
	clusterBinding := rbacItem("ClusterRoleBinding", "", "system:discovery")
	clusterBinding["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "reader"}
	binary, _, _ := rbacShim(t, rbacList(), rbacList(clusterBinding))
	if _, err := ReadRBACSnapshot(t.Context(), binary, "", []string{"trusted"}); err == nil {
		t.Fatal("cluster binding to a namespaced Role was accepted")
	}
	if !rbacName("system:basic-user") || rbacName("../secret") || rbacName(strings.Repeat("a", 254)) {
		t.Fatal("RBAC path-segment names were validated incorrectly")
	}
}
