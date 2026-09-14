package authority

import "testing"

func rbacBoundaryFixture(t *testing.T) (*Record, SecretSource, *RBACSnapshot) {
	t.Helper()
	record := staticFixture(t)
	secret := SecretSource{Namespace: "trusted", Name: "iterion-authority", UID: "uid-authority", ResourceVersion: "17"}
	snapshot := &RBACSnapshot{Namespaces: []string{"worker", "trusted"},
		Roles: []RBACRole{
			{Kind: "Role", Namespace: "trusted", Name: "authority-writer", UID: "uid-writer", ResourceVersion: "17",
				Rules: []RBACRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"update"}}}},
			{Kind: "Role", Namespace: "worker", Name: "sandbox", UID: "uid-sandbox", ResourceVersion: "17",
				Rules: []RBACRule{{APIGroups: []string{""}, Resources: []string{"pods", "secrets"}, Verbs: []string{"create", "get"}}}},
			{Kind: "ClusterRole", Name: "system:discovery", UID: "uid-discovery", ResourceVersion: "17",
				Rules: []RBACRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/api"}}}},
		},
		Bindings: []RBACBinding{
			{Kind: "RoleBinding", Namespace: "trusted", Name: "authority-writer", UID: "uid-writer-binding", ResourceVersion: "17",
				Subjects: []RBACSubject{{Kind: "User", Name: "deploy-operator"}}, RoleKind: "Role", RoleName: "authority-writer"},
			{Kind: "RoleBinding", Namespace: "worker", Name: "sandbox", UID: "uid-sandbox-binding", ResourceVersion: "17",
				Subjects: []RBACSubject{{Kind: "ServiceAccount", Namespace: "worker", Name: "iterion-runner"}},
				RoleKind: "Role", RoleName: "sandbox"},
			{Kind: "ClusterRoleBinding", Name: "system:discovery", UID: "uid-discovery-binding", ResourceVersion: "17",
				Subjects: []RBACSubject{{Kind: "Group", Name: "system:authenticated"}},
				RoleKind: "ClusterRole", RoleName: "system:discovery"},
		}}
	return record, secret, snapshot
}

func TestKubernetesRBACBoundaryAcceptsSeparatedWorkerAndNamedWriters(t *testing.T) {
	record, secret, snapshot := rbacBoundaryFixture(t)
	result, err := AnalyzeRBACBoundary(record, secret, snapshot)
	if err != nil || len(result.WorkerServiceAccounts) != 1 ||
		result.WorkerServiceAccounts[0] != "worker/iterion-runner" ||
		len(result.PermittedWriters) != 1 {
		t.Fatalf("separated RBAC identity boundary was rejected: %+v %v", result, err)
	}
}

func TestKubernetesRBACBoundaryRefusesCrossNamespaceAndWriterGrants(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Record, *RBACSnapshot)
	}{
		{"worker reads trusted Secret", func(_ *Record, s *RBACSnapshot) {
			s.Roles = append(s.Roles, RBACRole{Kind: "Role", Namespace: "trusted", Name: "secret-reader", UID: "uid-secret-reader", ResourceVersion: "18",
				Rules: []RBACRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}}})
			s.Bindings = append(s.Bindings, RBACBinding{Kind: "RoleBinding", Namespace: "trusted", Name: "worker-secret", UID: "uid-extra", ResourceVersion: "18",
				Subjects: []RBACSubject{{Kind: "ServiceAccount", Namespace: "worker", Name: "iterion-runner"}},
				RoleKind: "Role", RoleName: "secret-reader"})
		}},
		{"worker creates Deployment through cluster group", func(_ *Record, s *RBACSnapshot) {
			s.Roles = append(s.Roles, RBACRole{Kind: "ClusterRole", Name: "create-deployment", UID: "uid-extra", ResourceVersion: "18",
				Rules: []RBACRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"create"}}}})
			s.Bindings = append(s.Bindings, RBACBinding{Kind: "ClusterRoleBinding", Name: "worker-deployer", UID: "uid-extra-binding", ResourceVersion: "18",
				Subjects: []RBACSubject{{Kind: "Group", Name: "system:serviceaccounts:worker"}},
				RoleKind: "ClusterRole", RoleName: "create-deployment"})
		}},
		{"unapproved authority writer", func(_ *Record, s *RBACSnapshot) {
			s.Bindings[0].Subjects = []RBACSubject{{Kind: "ServiceAccount", Namespace: "worker", Name: "iterion-runner"}}
		}},
		{"broad authenticated writer", func(r *Record, s *RBACSnapshot) {
			r.PermittedWriters = append(r.PermittedWriters, OperatorWriter{Kind: "group", Name: "system:authenticated"})
			s.Bindings[0].Subjects = []RBACSubject{{Kind: "Group", Name: "system:authenticated"}}
		}},
		{"unresolved aggregated role", func(_ *Record, s *RBACSnapshot) { s.Roles[0].Aggregated = true }},
		{"missing role reference", func(_ *Record, s *RBACSnapshot) { s.Bindings[0].RoleName = "missing" }},
		{"authority holder outside trusted namespace", func(r *Record, _ *RBACSnapshot) { r.Holders[0].AccessScope = "authority" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, secret, snapshot := rbacBoundaryFixture(t)
			test.mutate(record, snapshot)
			if _, err := AnalyzeRBACBoundary(record, secret, snapshot); err == nil {
				t.Fatal("incomplete Kubernetes RBAC authority boundary was accepted")
			}
		})
	}
	for _, rule := range []RBACRule{
		{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create"}},
		{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"rolebindings"}, Verbs: []string{"patch"}},
		{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, Verbs: []string{"create"}},
		{APIGroups: []string{""}, Resources: []string{"users"}, Verbs: []string{"impersonate"}},
	} {
		if !ruleCrossesTrustedBoundary(rule) {
			t.Fatalf("protected Kubernetes permission was missed: %+v", rule)
		}
	}
	if ruleCrossesTrustedBoundary(RBACRule{Verbs: []string{"get"}, NonResourceURLs: []string{"/api"}}) {
		t.Fatal("ordinary discovery permission was misclassified")
	}
}
