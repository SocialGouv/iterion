package authority

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

type BrokerLaunch struct {
	ServerID    string `json:"server_id"`
	PodUID      string `json:"pod_uid"`
	PodRevision string `json:"pod_revision"`
	ImageID     string `json:"image_id"`
}

// ReconcileBrokerLaunch binds each operator-declared broker to a running Pod
// and one explicit nats-server -c invocation. Other launch forms are outside
// the initial static-auth profile because command-line auth overrides could
// otherwise defeat configuration-digest matching. This still needs live VARZ
// and cluster-scope reconciliation before it can contribute to a proof.
func ReconcileBrokerLaunch(record *Record, snapshot *WorkloadSnapshot) ([]BrokerLaunch, error) {
	if record == nil || record.validate() != nil || snapshot == nil ||
		!sameStrings(record.Namespaces, snapshot.Namespaces) {
		return nil, fmt.Errorf("NATS broker launch requires matching Kubernetes scope")
	}
	pods := make(map[[2]string]Workload)
	for _, workload := range snapshot.Workloads {
		if workload.Kind != "Pod" {
			continue
		}
		key := [2]string{workload.Namespace, workload.Name}
		if pods[key].UID != "" || workload.UID == "" || workload.ResourceVersion == "" {
			return nil, fmt.Errorf("NATS broker Pod inventory contains duplicate or unidentified objects")
		}
		pods[key] = workload
	}
	launches := make([]BrokerLaunch, 0, len(record.Brokers))
	for _, broker := range record.Brokers {
		pod := pods[[2]string{broker.Namespace, broker.PodName}]
		if pod.UID == "" || pod.APIVersion != "v1" {
			return nil, fmt.Errorf("NATS authority broker has no matching running Pod")
		}
		var spec struct {
			Containers []struct {
				Name    string   `json:"name"`
				Image   string   `json:"image"`
				Command []string `json:"command"`
				Args    []string `json:"args"`
			} `json:"containers"`
		}
		var status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Name    string `json:"name"`
				ImageID string `json:"imageID"`
				Ready   bool   `json:"ready"`
			} `json:"containerStatuses"`
		}
		if json.Unmarshal(pod.podSpec, &spec) != nil || json.Unmarshal(pod.status, &status) != nil ||
			status.Phase != "Running" {
			return nil, fmt.Errorf("NATS authority broker Pod has no supported running status")
		}
		matched := false
		for _, container := range spec.Containers {
			if container.Name != broker.Container {
				continue
			}
			if matched || container.Image != broker.ImageDigest || len(container.Command) != 1 ||
				!oneOf(container.Command[0], "nats-server", "/nats-server") || len(container.Args) != 2 ||
				!oneOf(container.Args[0], "-c", "--config") || container.Args[1] != broker.ConfigPath {
				return nil, fmt.Errorf("NATS broker launch has an unsupported command or image override")
			}
			matched = true
		}
		if !matched {
			return nil, fmt.Errorf("NATS authority broker container is absent from its Pod")
		}
		imageDigest := strings.Split(broker.ImageDigest, "@sha256:")[1]
		statusCount := 0
		for _, container := range status.ContainerStatuses {
			if container.Name != broker.Container {
				continue
			}
			statusCount++
			if !container.Ready || !strings.HasSuffix(container.ImageID, "sha256:"+imageDigest) {
				return nil, fmt.Errorf("NATS broker runtime image disagrees with the immutable Pod template")
			}
			launches = append(launches, BrokerLaunch{ServerID: broker.ServerID,
				PodUID: pod.UID, PodRevision: pod.ResourceVersion, ImageID: container.ImageID})
		}
		if statusCount != 1 {
			return nil, fmt.Errorf("NATS broker Pod has no unique ready container status")
		}
	}
	slices.SortFunc(launches, func(a, b BrokerLaunch) int { return strings.Compare(a.ServerID, b.ServerID) })
	return launches, nil
}
