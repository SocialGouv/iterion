package authority

import (
	"encoding/json"
	"strings"
	"testing"
)

func heldWorkload(t *testing.T, holder CredentialHolder, kind, name, uid string, owners ...OwnerReference) Workload {
	t.Helper()
	secret := strings.Split(strings.Split(holder.CredentialRef, "/")[1], ":")
	envName := ordinaryNATSEnv
	if holder.AccessScope == "authority" {
		envName = systemNATSEnv
	}
	spec, err := json.Marshal(map[string]any{"serviceAccountName": holder.ServiceAccount,
		"containers": []any{map[string]any{"name": holder.Container, "image": holder.ImageDigest,
			"env": []any{map[string]any{"name": envName, "valueFrom": map[string]any{
				"secretKeyRef": map[string]any{"name": secret[0], "key": secret[1]}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	version := "apps/v1"
	if kind == "Pod" {
		version = "v1"
	}
	return Workload{APIVersion: version, Kind: kind, Namespace: holder.Namespace, Name: name,
		UID: uid, ResourceVersion: "17", Generation: 1, Owners: owners, podSpec: spec}
}

func reconciledFixture(t *testing.T) (*Record, *WorkloadSnapshot) {
	t.Helper()
	record := staticFixture(t)
	worker, authority := record.Holders[0], record.Holders[1]
	workerDeployment := heldWorkload(t, worker, "Deployment", worker.WorkloadName, "uid-worker")
	authorityDeployment := heldWorkload(t, authority, "Deployment", authority.WorkloadName, "uid-authority")
	replica := heldWorkload(t, worker, "ReplicaSet", "worker-revision", "uid-replica",
		OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: worker.WorkloadName, UID: workerDeployment.UID, Controller: true})
	pod := heldWorkload(t, worker, "Pod", "worker-pod", "uid-pod",
		OwnerReference{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replica.Name, UID: replica.UID, Controller: true})
	return record, &WorkloadSnapshot{Namespaces: []string{"worker", "trusted"},
		Workloads: []Workload{pod, authorityDeployment, replica, workerDeployment}}
}

func TestKubernetesWorkloadReconciliationBindsDeclaredAndRunningDescendants(t *testing.T) {
	record, snapshot := reconciledFixture(t)
	result, err := ReconcileWorkloads(record, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Bindings) != 4 || result.Bindings[0].WorkloadUID != "uid-authority" ||
		result.Bindings[3].WorkloadUID != "uid-worker" {
		t.Fatalf("workload holder closure was not bound: %+v", result)
	}
}

func TestKubernetesWorkloadReconciliationRefusesDormantOrUnknownCredentialPaths(t *testing.T) {
	t.Run("scaled-zero old controller", func(t *testing.T) {
		record, snapshot := reconciledFixture(t)
		old := heldWorkload(t, record.Holders[0], "Deployment", "old-runner", "uid-old")
		snapshot.Workloads = append(snapshot.Workloads, old)
		if _, err := ReconcileWorkloads(record, snapshot); err == nil {
			t.Fatal("unaccounted scaled-zero deployment retained NATS queue access")
		}
	})
	t.Run("old ReplicaSet image", func(t *testing.T) {
		record, snapshot := reconciledFixture(t)
		workload := &snapshot.Workloads[2]
		var spec map[string]any
		if err := json.Unmarshal(workload.podSpec, &spec); err != nil {
			t.Fatal(err)
		}
		spec["containers"].([]any)[0].(map[string]any)["image"] = "iterion/old@sha256:" + strings.Repeat("d", 64)
		workload.podSpec, _ = json.Marshal(spec)
		if _, err := ReconcileWorkloads(record, snapshot); err == nil {
			t.Fatal("old ReplicaSet inherited a compatible holder despite an old image")
		}
	})
	t.Run("missing controlling owner", func(t *testing.T) {
		record, snapshot := reconciledFixture(t)
		snapshot.Workloads[0].Owners[0].UID = "replaced"
		if _, err := ReconcileWorkloads(record, snapshot); err == nil {
			t.Fatal("pod with changed controller UID inherited credential custody")
		}
	})
	for name, mutate := range map[string]func(map[string]any){
		"literal URL": func(spec map[string]any) {
			env := spec["containers"].([]any)[0].(map[string]any)["env"].([]any)[0].(map[string]any)
			env["value"] = "nats://private.example"
		},
		"envFrom known Secret": func(spec map[string]any) {
			spec["containers"].([]any)[0].(map[string]any)["envFrom"] = []any{
				map[string]any{"secretRef": map[string]any{"name": "nats-worker"}}}
		},
		"mounted known Secret": func(spec map[string]any) {
			spec["volumes"] = []any{map[string]any{"secret": map[string]any{"secretName": "nats-worker"}}}
		},
		"duplicate NATS env": func(spec map[string]any) {
			container := spec["containers"].([]any)[0].(map[string]any)
			container["env"] = append(container["env"].([]any), container["env"].([]any)[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			record, snapshot := reconciledFixture(t)
			workload := &snapshot.Workloads[3]
			var spec map[string]any
			if err := json.Unmarshal(workload.podSpec, &spec); err != nil {
				t.Fatal(err)
			}
			mutate(spec)
			workload.podSpec, _ = json.Marshal(spec)
			if _, err := ReconcileWorkloads(record, snapshot); err == nil {
				t.Fatal("unsupported Kubernetes credential path was accepted")
			}
		})
	}
}
