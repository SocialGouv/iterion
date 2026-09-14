package authority

import (
	"encoding/json"
	"strings"
	"testing"
)

func brokerLaunchFixture(t *testing.T) (*Record, *WorkloadSnapshot) {
	t.Helper()
	record := staticFixture(t)
	broker := record.Brokers[0]
	spec, err := json.Marshal(map[string]any{"containers": []any{map[string]any{
		"name": broker.Container, "image": broker.ImageDigest,
		"command": []string{"nats-server"}, "args": []string{"-c", broker.ConfigPath},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{
		"name": broker.Container, "imageID": "containerd://sha256:" + strings.Repeat("a", 64), "ready": true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return record, &WorkloadSnapshot{Namespaces: []string{"worker", "trusted"}, Workloads: []Workload{{
		APIVersion: "v1", Kind: "Pod", Namespace: broker.Namespace, Name: broker.PodName,
		UID: "uid-nats-pod", ResourceVersion: "17", podSpec: spec, status: status,
	}}}
}

func TestNATSBrokerLaunchRequiresExactRunningPodAndConfigInvocation(t *testing.T) {
	record, snapshot := brokerLaunchFixture(t)
	launches, err := ReconcileBrokerLaunch(record, snapshot)
	if err != nil || len(launches) != 1 || launches[0].ServerID != "NC123" ||
		launches[0].PodUID != "uid-nats-pod" || launches[0].PodRevision != "17" {
		t.Fatalf("broker Pod launch was not bound: %+v %v", launches, err)
	}
}

func TestNATSBrokerLaunchRefusesAuthOverridesAndUnpinnedRuntime(t *testing.T) {
	for name, mutate := range map[string]func(*Record, *WorkloadSnapshot){
		"command-line auth override": func(_ *Record, s *WorkloadSnapshot) {
			var spec map[string]any
			_ = json.Unmarshal(s.Workloads[0].podSpec, &spec)
			container := spec["containers"].([]any)[0].(map[string]any)
			container["args"] = []string{"-c", "/etc/nats/main.conf", "--user", "old"}
			s.Workloads[0].podSpec, _ = json.Marshal(spec)
		},
		"shell wrapper": func(_ *Record, s *WorkloadSnapshot) {
			var spec map[string]any
			_ = json.Unmarshal(s.Workloads[0].podSpec, &spec)
			spec["containers"].([]any)[0].(map[string]any)["command"] = []string{"sh", "-c"}
			s.Workloads[0].podSpec, _ = json.Marshal(spec)
		},
		"wrong image digest": func(_ *Record, s *WorkloadSnapshot) {
			var status map[string]any
			_ = json.Unmarshal(s.Workloads[0].status, &status)
			status["containerStatuses"].([]any)[0].(map[string]any)["imageID"] = "containerd://sha256:" + strings.Repeat("b", 64)
			s.Workloads[0].status, _ = json.Marshal(status)
		},
		"pending Pod": func(_ *Record, s *WorkloadSnapshot) {
			var status map[string]any
			_ = json.Unmarshal(s.Workloads[0].status, &status)
			status["phase"] = "Pending"
			s.Workloads[0].status, _ = json.Marshal(status)
		},
		"other Pod name": func(r *Record, _ *WorkloadSnapshot) { r.Brokers[0].PodName = "replacement" },
	} {
		t.Run(name, func(t *testing.T) {
			record, snapshot := brokerLaunchFixture(t)
			mutate(record, snapshot)
			if _, err := ReconcileBrokerLaunch(record, snapshot); err == nil {
				t.Fatal("unsupported or mismatched NATS broker launch was accepted")
			}
		})
	}
}
