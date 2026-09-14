package authority

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func workloadList(items ...map[string]any) []byte {
	encoded, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	return encoded
}

func workloadItem(kind, version, name string, spec any) map[string]any {
	return map[string]any{"apiVersion": version, "kind": kind,
		"metadata": map[string]any{"namespace": "trusted", "name": name, "uid": "uid-" + name,
			"resourceVersion": "17", "generation": 2}, "spec": spec}
}

func workloadShim(t *testing.T, response []byte) (binary, fixture, argsPath string) {
	t.Helper()
	dir := t.TempDir()
	binary = filepath.Join(dir, "kubectl")
	fixture = filepath.Join(dir, "response.json")
	argsPath = filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ITERION_WORKLOAD_ARGS_PATH\"\ncat \"$ITERION_WORKLOAD_FIXTURE_PATH\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, response, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_WORKLOAD_ARGS_PATH", argsPath)
	t.Setenv("ITERION_WORKLOAD_FIXTURE_PATH", fixture)
	return
}

func TestKubernetesWorkloadSnapshotIncludesDormantControllersAndPodTemplates(t *testing.T) {
	template := map[string]any{"spec": map[string]any{"serviceAccountName": "runner",
		"containers": []any{map[string]any{"name": "runner", "image": "iterion@sha256:abc",
			"env": []any{map[string]any{"name": "TOKEN", "value": "super-secret"}}}}}}
	oldReplica := workloadItem("ReplicaSet", "apps/v1", "old-runner", map[string]any{"replicas": 0, "template": template})
	cron := workloadItem("CronJob", "batch/v1", "nightly", map[string]any{
		"jobTemplate": map[string]any{"spec": map[string]any{"template": template}}})
	pod := workloadItem("Pod", "v1", "running", template["spec"])
	pod["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{
		"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "old-runner",
		"uid": "uid-old-runner", "controller": true,
	}}
	pod["status"] = map[string]any{"containerStatuses": []any{map[string]any{"name": "runner", "imageID": "sha256:abc"}}}
	binary, _, argsPath := workloadShim(t, workloadList(oldReplica, cron, pod))
	snapshot, err := ReadWorkloadSnapshot(t.Context(), binary, "fixture-context", []string{"trusted"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Workloads) != 3 || snapshot.Workloads[0].Kind != "CronJob" ||
		snapshot.Workloads[1].Kind != "Pod" || snapshot.Workloads[2].Kind != "ReplicaSet" ||
		!bytes.Contains(snapshot.Workloads[2].PodSpec(), []byte("super-secret")) ||
		!bytes.Contains(snapshot.Workloads[1].Status(), []byte("imageID")) ||
		len(snapshot.Workloads[1].Owners) != 1 || snapshot.Workloads[1].Owners[0].UID != "uid-old-runner" {
		t.Fatal("Kubernetes workload inventory lost a dormant controller, pod template or runtime status")
	}
	for _, rendered := range []string{
		fmt.Sprintf("%+v", snapshot), fmt.Sprintf("%#v", *snapshot),
		fmt.Sprintf("%+v", snapshot.Workloads[0]), fmt.Sprintf("%#v", snapshot.Workloads[0]),
	} {
		if strings.Contains(rendered, "super-secret") {
			t.Fatal("Kubernetes literal environment value leaked through formatting")
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || bytes.Contains(encoded, []byte("super-secret")) {
		t.Fatal("Kubernetes literal environment value leaked through JSON projection")
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--context", "fixture-context", "--namespace", "trusted", "get", workloadResources, "-o", "json"}
	if !reflect.DeepEqual(strings.Split(strings.TrimSpace(string(args)), "\n"), want) {
		t.Fatal("workload inventory did not request every supported kind in the named namespace")
	}
}

func TestKubernetesWorkloadSnapshotRefusesMissingOrForeignObjects(t *testing.T) {
	template := map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{"name": "runner", "image": "iterion:old"}}}}}
	valid := workloadItem("Deployment", "apps/v1", "runner", template)
	for name, response := range map[string][]byte{
		"foreign namespace": func() []byte {
			item := workloadItem("Deployment", "apps/v1", "runner", template)
			item["metadata"].(map[string]any)["namespace"] = "other"
			return workloadList(item)
		}(),
		"duplicate object": workloadList(valid, valid),
		"unknown kind":     workloadList(workloadItem("Widget", "v1", "runner", template)),
		"missing template": workloadList(workloadItem("Deployment", "apps/v1", "runner", map[string]any{"replicas": 0})),
		"partial list":     []byte(`{"apiVersion":"v1","kind":"List"}`),
		"continued list":   []byte(`{"apiVersion":"v1","kind":"List","metadata":{"continue":"next-page"},"items":[]}`),
	} {
		t.Run(name, func(t *testing.T) {
			binary, _, _ := workloadShim(t, response)
			if _, err := ReadWorkloadSnapshot(t.Context(), binary, "", []string{"trusted"}); err == nil {
				t.Fatal("incomplete Kubernetes workload inventory was accepted")
			}
		})
	}
	for _, namespaces := range [][]string{{}, {"trusted", "trusted"}, {"--all-namespaces"}} {
		if _, err := ReadWorkloadSnapshot(context.Background(), "kubectl", "", namespaces); err == nil {
			t.Fatal("invalid Kubernetes namespace scope reached kubectl")
		}
	}
}

func TestKubernetesWorkloadSnapshotBoundsActualKubectlPipe(t *testing.T) {
	valid := workloadList(workloadItem("Pod", "v1", "runner", map[string]any{
		"containers": []any{map[string]any{"name": "runner", "image": "iterion:old"}}}))
	binary, fixture, _ := workloadShim(t, nil)
	response := append(bytes.Repeat([]byte(" "), maxWorkloadResponse+1), valid...)
	if err := os.WriteFile(fixture, response, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWorkloadSnapshot(t.Context(), binary, "", []string{"trusted"}); err == nil {
		t.Fatal("oversized Kubernetes workload response bypassed the subprocess pipe bound")
	}
}
