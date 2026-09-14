package authority

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

const maxWorkloadResponse = 8 << 20
const maxWorkloads = 4096

const workloadResources = "pods,deployments,replicasets,statefulsets,daemonsets,jobs,cronjobs,replicationcontrollers"

// WorkloadSnapshot contains only the declared Kubernetes kinds in the listed
// namespaces. It cannot prove that no custom controller, external holder or
// credential issuer can reach the broker. The eventual authority must also
// reconcile RBAC, actual pod launch arguments and operator scope assertions.
type WorkloadSnapshot struct {
	Namespaces []string   `json:"namespaces"`
	Workloads  []Workload `json:"workloads"`
}

type Workload struct {
	APIVersion      string           `json:"api_version"`
	Kind            string           `json:"kind"`
	Namespace       string           `json:"namespace"`
	Name            string           `json:"name"`
	UID             string           `json:"uid"`
	ResourceVersion string           `json:"resource_version"`
	Generation      int64            `json:"generation"`
	Owners          []OwnerReference `json:"owners,omitempty"`
	podSpec         json.RawMessage
	status          json.RawMessage
}

type OwnerReference struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

func (WorkloadSnapshot) String() string     { return "Kubernetes workload snapshot [pod specs redacted]" }
func (s WorkloadSnapshot) GoString() string { return s.String() }
func (Workload) String() string             { return "Kubernetes workload [pod spec redacted]" }
func (w Workload) GoString() string         { return w.String() }

func (w Workload) PodSpec() json.RawMessage { return bytes.Clone(w.podSpec) }
func (w Workload) Status() json.RawMessage  { return bytes.Clone(w.status) }

// ReadWorkloadSnapshot queries the Kubernetes API through the existing
// bounded kubectl subprocess model. The same supported kinds are requested
// per namespace, including dormant controllers and scaled-to-zero templates.
// Missing list access or unrecognized object shapes fail closed.
func ReadWorkloadSnapshot(ctx context.Context, kubectlBinary, kubeContext string, namespaces []string) (*WorkloadSnapshot, error) {
	if kubectlBinary == "" || len(namespaces) == 0 || len(namespaces) > 16 {
		return nil, fmt.Errorf("Kubernetes workload inventory requires a bounded namespace scope")
	}
	seenNamespaces := make(map[string]bool, len(namespaces))
	for _, namespace := range namespaces {
		if !dnsLabel(namespace) || seenNamespaces[namespace] {
			return nil, fmt.Errorf("Kubernetes workload inventory has an invalid namespace scope")
		}
		seenNamespaces[namespace] = true
	}
	snapshot := &WorkloadSnapshot{Namespaces: append([]string(nil), namespaces...)}
	seenObjects := make(map[[3]string]bool)
	for _, namespace := range namespaces {
		args := make([]string, 0, 10)
		if kubeContext != "" {
			args = append(args, "--context", kubeContext)
		}
		args = append(args, "--namespace", namespace, "get", workloadResources, "-o", "json")
		output, err := runKubectl(ctx, kubectlBinary, args, maxWorkloadResponse)
		if err != nil {
			return nil, fmt.Errorf("Kubernetes workload inventory could not be read")
		}
		var document struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []json.RawMessage `json:"items"`
		}
		decoder := json.NewDecoder(bytes.NewReader(output))
		if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF ||
			document.APIVersion != "v1" || document.Kind != "List" || document.Items == nil ||
			document.Metadata.Continue != "" ||
			len(document.Items) > maxWorkloads-len(snapshot.Workloads) {
			return nil, fmt.Errorf("Kubernetes workload inventory returned an incomplete or oversized list")
		}
		for _, raw := range document.Items {
			workload, err := parseWorkload(raw, namespace)
			if err != nil {
				return nil, err
			}
			key := [3]string{workload.Kind, workload.Namespace, workload.Name}
			if seenObjects[key] {
				return nil, fmt.Errorf("Kubernetes workload inventory contains duplicate objects")
			}
			seenObjects[key] = true
			snapshot.Workloads = append(snapshot.Workloads, workload)
		}
	}
	slices.Sort(snapshot.Namespaces)
	slices.SortFunc(snapshot.Workloads, func(a, b Workload) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return snapshot, nil
}

func parseWorkload(raw json.RawMessage, namespace string) (Workload, error) {
	var item struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Namespace       string `json:"namespace"`
			Name            string `json:"name"`
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
			Generation      int64  `json:"generation"`
			OwnerReferences []struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
				Name       string `json:"name"`
				UID        string `json:"uid"`
				Controller bool   `json:"controller"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
		Spec   json.RawMessage `json:"spec"`
		Status json.RawMessage `json:"status"`
	}
	if json.Unmarshal(raw, &item) != nil || item.Metadata.Namespace != namespace ||
		!dnsSubdomain(item.Metadata.Name) || item.Metadata.UID == "" || item.Metadata.ResourceVersion == "" ||
		len(item.Spec) == 0 {
		return Workload{}, fmt.Errorf("Kubernetes workload inventory contains an unidentified object")
	}
	if !supportedWorkloadVersion(item.Kind, item.APIVersion) {
		return Workload{}, fmt.Errorf("Kubernetes workload inventory contains an unsupported kind or version")
	}
	var spec map[string]json.RawMessage
	if json.Unmarshal(item.Spec, &spec) != nil {
		return Workload{}, fmt.Errorf("Kubernetes workload inventory has an invalid pod template")
	}
	podSpec := item.Spec
	if item.Kind != "Pod" {
		if item.Kind == "CronJob" {
			podSpec = nestedJSON(spec, "jobTemplate", "spec", "template", "spec")
		} else {
			podSpec = nestedJSON(spec, "template", "spec")
		}
	}
	var template struct {
		Containers []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
		} `json:"containers"`
	}
	if len(podSpec) == 0 || json.Unmarshal(podSpec, &template) != nil || len(template.Containers) == 0 {
		return Workload{}, fmt.Errorf("Kubernetes workload inventory has an incomplete pod template")
	}
	for _, container := range template.Containers {
		if !dnsLabel(container.Name) || container.Image == "" {
			return Workload{}, fmt.Errorf("Kubernetes workload inventory has an unnamed container")
		}
	}
	if len(item.Metadata.OwnerReferences) > 8 {
		return Workload{}, fmt.Errorf("Kubernetes workload inventory has too many owners")
	}
	owners := make([]OwnerReference, 0, len(item.Metadata.OwnerReferences))
	controllerCount := 0
	for _, owner := range item.Metadata.OwnerReferences {
		if !supportedWorkloadVersion(owner.Kind, owner.APIVersion) || !dnsSubdomain(owner.Name) || owner.UID == "" {
			return Workload{}, fmt.Errorf("Kubernetes workload inventory has an unsupported owner")
		}
		if owner.Controller {
			controllerCount++
		}
		owners = append(owners, OwnerReference{APIVersion: owner.APIVersion, Kind: owner.Kind,
			Name: owner.Name, UID: owner.UID, Controller: owner.Controller})
	}
	if controllerCount > 1 {
		return Workload{}, fmt.Errorf("Kubernetes workload inventory has ambiguous controlling owners")
	}
	return Workload{APIVersion: item.APIVersion, Kind: item.Kind,
		Namespace: namespace, Name: item.Metadata.Name, UID: item.Metadata.UID,
		ResourceVersion: item.Metadata.ResourceVersion, Generation: item.Metadata.Generation,
		Owners: owners, podSpec: bytes.Clone(podSpec), status: bytes.Clone(item.Status)}, nil
}

func nestedJSON(root map[string]json.RawMessage, path ...string) json.RawMessage {
	for _, field := range path {
		var next map[string]json.RawMessage
		if json.Unmarshal(root[field], &next) != nil {
			return nil
		}
		root = next
	}
	encoded, _ := json.Marshal(root)
	return encoded
}

func supportedWorkloadVersion(kind, version string) bool {
	switch kind {
	case "Pod", "ReplicationController":
		return version == "v1"
	case "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet":
		return version == "apps/v1"
	case "Job", "CronJob":
		return version == "batch/v1"
	default:
		return false
	}
}
