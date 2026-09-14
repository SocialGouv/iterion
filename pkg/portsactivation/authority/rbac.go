package authority

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

const maxRBACResponse = 16 << 20
const maxRBACObjects = 4096

type RBACSnapshot struct {
	Namespaces []string      `json:"namespaces"`
	Roles      []RBACRole    `json:"roles"`
	Bindings   []RBACBinding `json:"bindings"`
}

type RBACRule struct {
	APIGroups       []string `json:"api_groups"`
	Resources       []string `json:"resources"`
	Verbs           []string `json:"verbs"`
	ResourceNames   []string `json:"resource_names,omitempty"`
	NonResourceURLs []string `json:"non_resource_urls,omitempty"`
}

type RBACRole struct {
	Kind            string     `json:"kind"`
	Namespace       string     `json:"namespace,omitempty"`
	Name            string     `json:"name"`
	UID             string     `json:"uid"`
	ResourceVersion string     `json:"resource_version"`
	Aggregated      bool       `json:"aggregated"`
	Rules           []RBACRule `json:"rules"`
}

type RBACSubject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type RBACBinding struct {
	Kind            string        `json:"kind"`
	Namespace       string        `json:"namespace,omitempty"`
	Name            string        `json:"name"`
	UID             string        `json:"uid"`
	ResourceVersion string        `json:"resource_version"`
	Subjects        []RBACSubject `json:"subjects"`
	RoleKind        string        `json:"role_kind"`
	RoleName        string        `json:"role_name"`
}

// ReadRBACSnapshot retrieves declared RBAC state only. It does not prove
// that RBAC is the cluster's sole authorizer, nor that admissions and future
// policy changes will preserve a denial. The verifier must reconcile actual
// bindings and operator assertions before producing a proof.
func ReadRBACSnapshot(ctx context.Context, kubectlBinary, kubeContext string, namespaces []string) (*RBACSnapshot, error) {
	if kubectlBinary == "" || len(namespaces) == 0 || len(namespaces) > 16 {
		return nil, fmt.Errorf("Kubernetes RBAC inventory requires a bounded namespace scope")
	}
	seenNamespaces := make(map[string]bool)
	for _, namespace := range namespaces {
		if !dnsLabel(namespace) || seenNamespaces[namespace] {
			return nil, fmt.Errorf("Kubernetes RBAC inventory has an invalid namespace")
		}
		seenNamespaces[namespace] = true
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	snapshot := &RBACSnapshot{Namespaces: append([]string(nil), namespaces...)}
	seen := make(map[[3]string]bool)
	read := func(namespace, resources string) error {
		args := make([]string, 0, 9)
		if kubeContext != "" {
			args = append(args, "--context", kubeContext)
		}
		if namespace != "" {
			args = append(args, "--namespace", namespace)
		}
		args = append(args, "get", resources, "-o", "json")
		output, err := runKubectl(ctx, kubectlBinary, args, maxRBACResponse)
		if err != nil {
			return fmt.Errorf("Kubernetes RBAC inventory could not be read")
		}
		var list struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []json.RawMessage `json:"items"`
		}
		decoder := json.NewDecoder(bytes.NewReader(output))
		if decoder.Decode(&list) != nil || decoder.Decode(new(any)) != io.EOF || list.APIVersion != "v1" ||
			list.Kind != "List" || list.Items == nil || list.Metadata.Continue != "" ||
			len(list.Items) > maxRBACObjects-len(seen) {
			return fmt.Errorf("Kubernetes RBAC inventory returned an incomplete or oversized list")
		}
		for _, raw := range list.Items {
			kind, name, role, binding, err := parseRBACObject(raw, namespace)
			if err != nil {
				return err
			}
			key := [3]string{kind, namespace, name}
			if seen[key] {
				return fmt.Errorf("Kubernetes RBAC inventory contains duplicate objects")
			}
			seen[key] = true
			if role != nil {
				snapshot.Roles = append(snapshot.Roles, *role)
			} else {
				snapshot.Bindings = append(snapshot.Bindings, *binding)
			}
		}
		return nil
	}
	for _, namespace := range namespaces {
		if err := read(namespace, "roles,rolebindings"); err != nil {
			return nil, err
		}
	}
	if err := read("", "clusterroles,clusterrolebindings"); err != nil {
		return nil, err
	}
	slices.Sort(snapshot.Namespaces)
	slices.SortFunc(snapshot.Roles, func(a, b RBACRole) int {
		return strings.Compare(a.Namespace+"/"+a.Kind+"/"+a.Name, b.Namespace+"/"+b.Kind+"/"+b.Name)
	})
	slices.SortFunc(snapshot.Bindings, func(a, b RBACBinding) int {
		return strings.Compare(a.Namespace+"/"+a.Kind+"/"+a.Name, b.Namespace+"/"+b.Kind+"/"+b.Name)
	})
	return snapshot, nil
}

func parseRBACObject(raw json.RawMessage, namespace string) (string, string, *RBACRole, *RBACBinding, error) {
	var item struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Namespace       string `json:"namespace"`
			Name            string `json:"name"`
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Rules []struct {
			APIGroups       []string `json:"apiGroups"`
			Resources       []string `json:"resources"`
			Verbs           []string `json:"verbs"`
			ResourceNames   []string `json:"resourceNames"`
			NonResourceURLs []string `json:"nonResourceURLs"`
		} `json:"rules"`
		AggregationRule json.RawMessage `json:"aggregationRule"`
		Subjects        []RBACSubject   `json:"subjects"`
		RoleRef         struct {
			APIGroup string `json:"apiGroup"`
			Kind     string `json:"kind"`
			Name     string `json:"name"`
		} `json:"roleRef"`
	}
	if json.Unmarshal(raw, &item) != nil || item.APIVersion != "rbac.authorization.k8s.io/v1" ||
		item.Metadata.Namespace != namespace || !rbacName(item.Metadata.Name) ||
		item.Metadata.UID == "" || item.Metadata.ResourceVersion == "" {
		return "", "", nil, nil, fmt.Errorf("Kubernetes RBAC inventory contains an unidentified object")
	}
	switch item.Kind {
	case "Role", "ClusterRole":
		if item.Kind == "Role" && namespace == "" || item.Kind == "ClusterRole" && namespace != "" ||
			len(item.Rules) > 256 {
			return "", "", nil, nil, fmt.Errorf("Kubernetes RBAC inventory has an unsupported role scope")
		}
		role := &RBACRole{Kind: item.Kind, Namespace: namespace, Name: item.Metadata.Name,
			UID: item.Metadata.UID, ResourceVersion: item.Metadata.ResourceVersion,
			Aggregated: len(item.AggregationRule) != 0 && string(item.AggregationRule) != "null"}
		for _, rule := range item.Rules {
			if len(rule.Verbs) == 0 {
				return "", "", nil, nil, fmt.Errorf("Kubernetes RBAC inventory contains an incomplete policy rule")
			}
			role.Rules = append(role.Rules, RBACRule{APIGroups: rule.APIGroups, Resources: rule.Resources,
				Verbs: rule.Verbs, ResourceNames: rule.ResourceNames, NonResourceURLs: rule.NonResourceURLs})
		}
		return item.Kind, item.Metadata.Name, role, nil, nil
	case "RoleBinding", "ClusterRoleBinding":
		if item.Kind == "RoleBinding" && namespace == "" || item.Kind == "ClusterRoleBinding" && namespace != "" ||
			item.RoleRef.APIGroup != "rbac.authorization.k8s.io" ||
			!oneOf(item.RoleRef.Kind, "Role", "ClusterRole") || !rbacName(item.RoleRef.Name) ||
			len(item.Subjects) > 256 {
			return "", "", nil, nil, fmt.Errorf("Kubernetes RBAC inventory has an unsupported binding scope")
		}
		if item.Kind == "ClusterRoleBinding" && item.RoleRef.Kind != "ClusterRole" {
			return "", "", nil, nil, fmt.Errorf("Kubernetes RBAC inventory has a noncluster role binding")
		}
		for _, subject := range item.Subjects {
			if !oneOf(subject.Kind, "User", "Group", "ServiceAccount") || subject.Name == "" ||
				subject.Kind == "ServiceAccount" && (!dnsLabel(subject.Namespace) || !dnsSubdomain(subject.Name)) {
				return "", "", nil, nil, fmt.Errorf("Kubernetes RBAC inventory contains an invalid subject")
			}
		}
		binding := &RBACBinding{Kind: item.Kind, Namespace: namespace, Name: item.Metadata.Name,
			UID: item.Metadata.UID, ResourceVersion: item.Metadata.ResourceVersion,
			Subjects: item.Subjects, RoleKind: item.RoleRef.Kind, RoleName: item.RoleRef.Name}
		return item.Kind, item.Metadata.Name, nil, binding, nil
	default:
		return "", "", nil, nil, fmt.Errorf("Kubernetes RBAC inventory contains an unsupported kind")
	}
}

func rbacName(name string) bool {
	if len(name) == 0 || len(name) > 253 || name == "." || name == ".." {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}
