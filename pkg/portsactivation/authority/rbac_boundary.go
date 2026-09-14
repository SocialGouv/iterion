package authority

import (
	"fmt"
	"slices"
	"strings"
)

type serviceAccountIdentity struct{ namespace, name string }
type roleIdentity struct{ kind, namespace, name string }

// RBACBoundary is a corroborated view of this RBAC snapshot only. Its caller
// must separately establish the active Kubernetes authorization modes,
// admission controls, credential issuers and change protocol. A passing
// result cannot by itself authorize distributed admission.
type RBACBoundary struct {
	WorkerServiceAccounts []string `json:"worker_service_accounts"`
	PermittedWriters      []string `json:"permitted_writers"`
}

func AnalyzeRBACBoundary(record *Record, authoritySecret SecretSource, snapshot *RBACSnapshot) (*RBACBoundary, error) {
	if record == nil || record.validate() != nil || snapshot == nil ||
		!sameStrings(record.Namespaces, snapshot.Namespaces) ||
		authoritySecret.Namespace == "" || authoritySecret.Name == "" ||
		authoritySecret.UID == "" || authoritySecret.ResourceVersion == "" ||
		!slices.Contains(record.Namespaces, authoritySecret.Namespace) {
		return nil, fmt.Errorf("authority: Kubernetes RBAC boundary requires matching authority and inventory scope")
	}
	roles := make(map[roleIdentity]RBACRole, len(snapshot.Roles))
	for _, role := range snapshot.Roles {
		key := roleIdentity{role.Kind, role.Namespace, role.Name}
		if roles[key].UID != "" || role.UID == "" || role.ResourceVersion == "" ||
			!oneOf(role.Kind, "Role", "ClusterRole") || role.Kind == "Role" &&
			!slices.Contains(snapshot.Namespaces, role.Namespace) || role.Kind == "ClusterRole" && role.Namespace != "" {
			return nil, fmt.Errorf("authority: Kubernetes RBAC boundary has an incomplete role inventory")
		}
		roles[key] = role
	}
	workers := make(map[serviceAccountIdentity]bool)
	for _, holder := range record.Holders {
		if holder.Kind == "kubernetes" {
			if holder.Namespace == authoritySecret.Namespace && !oneOf(holder.AccessScope, "server", "authority") ||
				holder.Namespace != authoritySecret.Namespace && holder.AccessScope == "authority" {
				return nil, fmt.Errorf("authority: Kubernetes worker shares the authority Secret namespace")
			}
			if holder.Namespace != authoritySecret.Namespace {
				workers[serviceAccountIdentity{holder.Namespace, holder.ServiceAccount}] = true
			}
		}
	}
	if len(workers) == 0 {
		return nil, fmt.Errorf("authority: Kubernetes RBAC boundary has no named worker identity")
	}
	permitted := make(map[[3]string]bool, len(record.PermittedWriters))
	for _, writer := range record.PermittedWriters {
		permitted[[3]string{writer.Kind, writer.Namespace, writer.Name}] = true
	}
	result := &RBACBoundary{}
	for worker := range workers {
		result.WorkerServiceAccounts = append(result.WorkerServiceAccounts, worker.namespace+"/"+worker.name)
	}
	for _, writer := range record.PermittedWriters {
		result.PermittedWriters = append(result.PermittedWriters, writer.Kind+":"+writer.Namespace+":"+writer.Name)
	}
	seenBindings := make(map[roleIdentity]bool)
	for _, binding := range snapshot.Bindings {
		key := roleIdentity{binding.Kind, binding.Namespace, binding.Name}
		if seenBindings[key] || binding.UID == "" || binding.ResourceVersion == "" ||
			!oneOf(binding.Kind, "RoleBinding", "ClusterRoleBinding") ||
			binding.Kind == "RoleBinding" && !slices.Contains(snapshot.Namespaces, binding.Namespace) ||
			binding.Kind == "ClusterRoleBinding" && binding.Namespace != "" {
			return nil, fmt.Errorf("authority: Kubernetes RBAC boundary has an incomplete binding inventory")
		}
		seenBindings[key] = true
		roleNamespace := binding.Namespace
		if binding.RoleKind == "ClusterRole" {
			roleNamespace = ""
		}
		role, exists := roles[roleIdentity{binding.RoleKind, roleNamespace, binding.RoleName}]
		if !exists || role.Aggregated {
			return nil, fmt.Errorf("authority: Kubernetes RBAC boundary references an unresolved or dynamic role")
		}
		appliesToTrusted := binding.Kind == "ClusterRoleBinding" || binding.Namespace == authoritySecret.Namespace
		if !appliesToTrusted {
			continue
		}
		for _, rule := range role.Rules {
			if !ruleCanWriteSecret(rule) {
				continue
			}
			for _, subject := range binding.Subjects {
				writerKey, okay := subjectWriterKey(subject)
				if !okay || !permitted[writerKey] || broadCredentialWriter(subject) {
					return nil, fmt.Errorf("authority: Kubernetes authority Secret has an unapproved RBAC writer")
				}
			}
		}
		for worker := range workers {
			if !bindingMatchesServiceAccount(binding, worker) {
				continue
			}
			for _, rule := range role.Rules {
				if ruleCrossesTrustedBoundary(rule) {
					return nil, fmt.Errorf("authority: Kubernetes worker can cross the authority namespace boundary")
				}
			}
		}
	}
	slices.Sort(result.WorkerServiceAccounts)
	slices.Sort(result.PermittedWriters)
	return result, nil
}

func bindingMatchesServiceAccount(binding RBACBinding, sa serviceAccountIdentity) bool {
	username := "system:serviceaccount:" + sa.namespace + ":" + sa.name
	groups := []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + sa.namespace}
	for _, subject := range binding.Subjects {
		switch subject.Kind {
		case "ServiceAccount":
			if subject.Namespace == sa.namespace && subject.Name == sa.name {
				return true
			}
		case "User":
			if subject.Name == username {
				return true
			}
		case "Group":
			if slices.Contains(groups, subject.Name) {
				return true
			}
		}
	}
	return false
}

func subjectWriterKey(subject RBACSubject) ([3]string, bool) {
	switch subject.Kind {
	case "ServiceAccount":
		return [3]string{"service_account", subject.Namespace, subject.Name}, true
	case "User":
		return [3]string{"user", "", subject.Name}, true
	case "Group":
		return [3]string{"group", "", subject.Name}, true
	default:
		return [3]string{}, false
	}
}

func broadCredentialWriter(subject RBACSubject) bool {
	return subject.Kind == "User" && subject.Name == "system:anonymous" ||
		subject.Kind == "Group" && (subject.Name == "system:anonymous" || subject.Name == "system:unauthenticated" ||
			subject.Name == "system:authenticated" || subject.Name == "system:serviceaccounts" ||
			strings.HasPrefix(subject.Name, "system:serviceaccounts:"))
}

func ruleCanWriteSecret(rule RBACRule) bool {
	return listIntersects(rule.APIGroups, "", "*") && rbacResourceMatches(rule.Resources, "secrets") &&
		listIntersects(rule.Verbs, "create", "update", "patch", "delete", "deletecollection", "*")
}

func ruleCrossesTrustedBoundary(rule RBACRule) bool {
	if len(rule.Resources) == 0 || len(rule.Verbs) == 0 {
		return false
	}
	if listIntersects(rule.Verbs, "impersonate", "bind", "escalate") {
		return true
	}
	if !listIntersects(rule.Verbs, "get", "list", "watch", "create", "update", "patch", "delete", "deletecollection", "*") {
		return false
	}
	if listIntersects(rule.APIGroups, "", "*") {
		// A Pod subresource can mutate or disclose a trusted Pod independently
		// of access to pods itself. Reject every explicit pods/ grant, including
		// future subresources, and every */subresource grant in this API group.
		for _, resource := range rule.Resources {
			if strings.HasPrefix(resource, "pods/") || strings.HasPrefix(resource, "*/") {
				return true
			}
		}
		if rbacAnyResourceMatches(rule.Resources,
			"secrets", "pods", "serviceaccounts", "serviceaccounts/token",
			"users", "groups", "configmaps", "replicationcontrollers", "replicationcontrollers/scale",
			"namespaces", "nodes", "nodes/proxy") {
			return true
		}
	}
	if listIntersects(rule.APIGroups, "apps", "*") && rbacAnyResourceMatches(rule.Resources,
		"deployments", "deployments/scale", "deployments/status",
		"replicasets", "replicasets/scale", "replicasets/status",
		"statefulsets", "statefulsets/scale", "statefulsets/status",
		"daemonsets", "daemonsets/status") {
		return true
	}
	if listIntersects(rule.APIGroups, "batch", "*") && rbacAnyResourceMatches(rule.Resources,
		"jobs", "jobs/status", "cronjobs", "cronjobs/status") {
		return true
	}
	if listIntersects(rule.APIGroups, "rbac.authorization.k8s.io", "*") &&
		rbacAnyResourceMatches(rule.Resources, "roles", "rolebindings", "clusterroles", "clusterrolebindings") {
		return true
	}
	if listIntersects(rule.APIGroups, "authentication.k8s.io", "*") &&
		rbacAnyResourceMatches(rule.Resources, "userextras") {
		return true
	}
	if listIntersects(rule.APIGroups, "certificates.k8s.io", "*") &&
		rbacAnyResourceMatches(rule.Resources, "certificatesigningrequests", "certificatesigningrequests/approval") {
		return true
	}
	if listIntersects(rule.APIGroups, "admissionregistration.k8s.io", "*") &&
		rbacAnyResourceMatches(rule.Resources, "mutatingwebhookconfigurations", "validatingwebhookconfigurations") {
		return true
	}
	if listIntersects(rule.APIGroups, "apiextensions.k8s.io", "*") &&
		rbacAnyResourceMatches(rule.Resources, "customresourcedefinitions") {
		return true
	}
	return false
}

func rbacAnyResourceMatches(grants []string, protected ...string) bool {
	for _, resource := range protected {
		if rbacResourceMatches(grants, resource) {
			return true
		}
	}
	return false
}

// Mirrors the Kubernetes RBAC ResourceMatches rule for the finite protected
// catalog: '*' grants all, exact names grant one, and '*/subresource' grants
// the named subresource on every resource in the API group.
func rbacResourceMatches(grants []string, protected string) bool {
	subresource := ""
	if index := strings.IndexByte(protected, '/'); index >= 0 {
		subresource = protected[index+1:]
	}
	for _, grant := range grants {
		if grant == "*" || grant == protected ||
			subresource != "" && grant == "*/"+subresource {
			return true
		}
	}
	return false
}

func listIntersects(values []string, candidates ...string) bool {
	for _, value := range values {
		if slices.Contains(candidates, value) {
			return true
		}
	}
	return false
}
