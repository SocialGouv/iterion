package authority

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	queue "github.com/SocialGouv/iterion/pkg/queue/nats"
	natsclient "github.com/nats-io/nats.go"
)

func TestAuthorityLiveDeploymentObservationRejectsSourceRotation(t *testing.T) {
	binary := os.Getenv("ITERION_TEST_NATS_SERVER")
	if binary == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("deployment observation requires the pinned NATS broker")
		}
		t.Skip("ITERION_TEST_NATS_SERVER not set")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	config := fmt.Sprintf(`listen: 127.0.0.1:%d
server_name: nats-0.example
jetstream: true
system_account: SYS
accounts {
  SYS {users: [{user: sys, password: fixture, permissions: {publish: ["$SYS.REQ.>"], subscribe: ["_INBOX.>"]}}]}
  WORK {jetstream: true, users: [{user: worker-user, password: fixture, permissions: {publish: ["$JS.API.CONSUMER.MSG.NEXT.%s.%s", "$JS.ACK.>"], subscribe: ["_INBOX.>"]}}]}
}`, port, queue.StreamRuns, queue.ConsumerRunners)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "main.conf")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "-c", configPath)
	command.Dir = dir
	var brokerLog bytes.Buffer
	command.Stderr = &brokerLog
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	systemURL := fmt.Sprintf("nats://sys:fixture@127.0.0.1:%d", port)
	var system *natsclient.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		system, err = DialSystem(t.Context(), systemURL)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("deployment observation broker did not start: %s", brokerLog.String())
	}
	serverID := system.ConnectedServerId()
	system.Close()
	worker, err := natsclient.Connect(fmt.Sprintf("nats://127.0.0.1:%d", port),
		natsclient.UserInfo("worker-user", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()

	fixture := validRecordFixture(t)
	broker := fixture["brokers"].([]any)[0].(map[string]any)
	broker["server_id"], broker["server_name"] = serverID, "nats-0.example"
	broker["sources"] = map[string]any{"entry": "main.conf", "files": map[string]string{"main.conf": config}}
	workerHolder := fixture["holders"].([]any)[0].(map[string]any)
	workerHolder["workload_kind"], workerHolder["workload_name"] = "Pod", "worker-pod"
	fixture["credentials"] = append(fixture["credentials"].([]any), map[string]any{
		"account": "SYS", "identity": "sys", "holder_ids": []string{"authority-1"}, "issuer_ids": []string{"operator"},
	})
	fixture["holders"] = append(fixture["holders"].([]any), map[string]any{
		"id": "authority-1", "kind": "kubernetes", "namespace": "trusted",
		"workload_kind": "Pod", "workload_name": "server-pod", "container": "server",
		"service_account": "iterion-server", "image_digest": "iterion/server@sha256:" + strings.Repeat("a", 64),
		"build_digest": strings.Repeat("b", 64), "access_scope": "authority",
		"credential_ref": "trusted/nats-system:NATS_URL",
	})
	record, err := parseFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	material, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	secret := func(namespace, name, uid, revision string, data map[string][]byte) map[string]any {
		encoded := make(map[string]string, len(data))
		for key, value := range data {
			encoded[key] = base64.StdEncoding.EncodeToString(value)
		}
		return map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata": map[string]string{"namespace": namespace, "name": name,
				"uid": uid, "resourceVersion": revision}, "data": encoded}
	}
	write("authority.json", secret("trusted", "iterion-authority", "uid-authority", "17",
		map[string][]byte{"authority.json": material}))
	write("authority-rotated.json", secret("trusted", "iterion-authority", "uid-authority", "18",
		map[string][]byte{"authority.json": material}))
	write("nats-config.json", secret("trusted", "nats-config", "uid-config", "17",
		map[string][]byte{"main.conf": []byte(config)}))
	write("nats-system.json", secret("trusted", "nats-system", "uid-system", "17",
		map[string][]byte{"NATS_URL": []byte(systemURL)}))
	write("nats-worker.json", secret("worker", "nats-worker", "uid-worker", "17",
		map[string][]byte{"NATS_URL": []byte(fmt.Sprintf("nats://worker-user:fixture@127.0.0.1:%d", port))}))
	pod := func(namespace, name, uid, serviceAccount, container, image, envName, secretName string) map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"namespace": namespace, "name": name, "uid": uid,
				"resourceVersion": "17", "generation": 1},
			"spec": map[string]any{"serviceAccountName": serviceAccount,
				"containers": []any{map[string]any{"name": container, "image": image,
					"env": []any{map[string]any{"name": envName, "valueFrom": map[string]any{
						"secretKeyRef": map[string]any{"name": secretName, "key": "NATS_URL"}}}}}}},
			"status": map[string]any{"phase": "Running"}}
	}
	workerPod := pod("worker", "worker-pod", "uid-worker-pod", "iterion-runner", "runner",
		record.Holders[0].ImageDigest, ordinaryNATSEnv, "nats-worker")
	serverPod := pod("trusted", "server-pod", "uid-server-pod", "iterion-server", "server",
		record.Holders[1].ImageDigest, systemNATSEnv, "nats-system")
	brokerPod := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"namespace": "trusted", "name": "nats-0", "uid": "uid-nats-pod",
			"resourceVersion": "17", "generation": 1},
		"spec": map[string]any{"serviceAccountName": "nats",
			"containers": []any{map[string]any{"name": "nats", "image": record.Brokers[0].ImageDigest,
				"command": []string{"/nats-server"}, "args": []string{"-c", "/etc/nats/main.conf"},
				"volumeMounts": []any{map[string]any{"name": "config", "mountPath": "/etc/nats", "readOnly": true}}}},
			"volumes": []any{map[string]any{"name": "config", "secret": map[string]any{
				"secretName": "nats-config", "optional": false}}}},
		"status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{
			"name": "nats", "imageID": "containerd://sha256:" + strings.Repeat("a", 64), "ready": true}}}}
	list := func(items []any) map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": "List", "items": items}
	}
	write("workloads-trusted.json", list([]any{serverPod, brokerPod}))
	write("workloads-worker.json", list([]any{workerPod}))
	_, _, rbac := rbacBoundaryFixture(t)
	rbacItems := func(namespace string) []any {
		items := []any{}
		for _, role := range rbac.Roles {
			if role.Namespace != namespace {
				continue
			}
			rules := []any{}
			for _, rule := range role.Rules {
				rules = append(rules, map[string]any{"apiGroups": rule.APIGroups,
					"resources": rule.Resources, "verbs": rule.Verbs,
					"resourceNames": rule.ResourceNames, "nonResourceURLs": rule.NonResourceURLs})
			}
			items = append(items, map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind": role.Kind, "metadata": map[string]string{"namespace": namespace, "name": role.Name,
					"uid": role.UID, "resourceVersion": role.ResourceVersion}, "rules": rules})
		}
		for _, binding := range rbac.Bindings {
			if binding.Namespace != namespace {
				continue
			}
			items = append(items, map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind": binding.Kind, "metadata": map[string]string{"namespace": namespace,
					"name": binding.Name, "uid": binding.UID, "resourceVersion": binding.ResourceVersion},
				"subjects": binding.Subjects, "roleRef": map[string]string{
					"apiGroup": "rbac.authorization.k8s.io", "kind": binding.RoleKind, "name": binding.RoleName}})
		}
		return items
	}
	write("rbac-trusted.json", list(rbacItems("trusted")))
	write("rbac-worker.json", list(rbacItems("worker")))
	write("rbac-cluster.json", list(rbacItems("")))
	kubectl := filepath.Join(dir, "kubectl")
	script := `#!/bin/sh
case "$*" in
  *"--namespace trusted get secret iterion-authority "*)
    if [ "$ITERION_AUTH_ROTATE" = 1 ]; then
      if [ -f "$ITERION_AUTH_FIXTURE_DIR/seen-authority" ]; then
        cat "$ITERION_AUTH_FIXTURE_DIR/authority-rotated.json"
      else
        touch "$ITERION_AUTH_FIXTURE_DIR/seen-authority"
        cat "$ITERION_AUTH_FIXTURE_DIR/authority.json"
      fi
    else cat "$ITERION_AUTH_FIXTURE_DIR/authority.json"; fi ;;
  *"--namespace trusted get secret nats-config "*) cat "$ITERION_AUTH_FIXTURE_DIR/nats-config.json" ;;
  *"--namespace trusted get secret nats-system "*) cat "$ITERION_AUTH_FIXTURE_DIR/nats-system.json" ;;
  *"--namespace worker get secret nats-worker "*) cat "$ITERION_AUTH_FIXTURE_DIR/nats-worker.json" ;;
  *"--namespace trusted get pods,"*) cat "$ITERION_AUTH_FIXTURE_DIR/workloads-trusted.json" ;;
  *"--namespace worker get pods,"*) cat "$ITERION_AUTH_FIXTURE_DIR/workloads-worker.json" ;;
  *"--namespace trusted get roles,rolebindings "*) cat "$ITERION_AUTH_FIXTURE_DIR/rbac-trusted.json" ;;
  *"--namespace worker get roles,rolebindings "*) cat "$ITERION_AUTH_FIXTURE_DIR/rbac-worker.json" ;;
  *"get clusterroles,clusterrolebindings "*) cat "$ITERION_AUTH_FIXTURE_DIR/rbac-cluster.json" ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(kubectl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_AUTH_FIXTURE_DIR", dir)
	result, err := ObserveDeployment(t.Context(), kubectl, "fixture", "trusted/iterion-authority",
		[]string{"trusted", "worker"}, record.Queue, systemURL)
	if err != nil || result.AuthoritySecretRevision != "17" || result.Queue != record.Queue ||
		len(result.ObservationDigest) != 64 ||
		result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) ||
		len(result.BrokerSources) != 1 || len(result.Credentials.Bindings) != 2 ||
		len(result.System.Brokers) != 1 || result.System.Brokers[0].ServerID != serverID {
		t.Fatalf("production deployment observer did not bind live NATS and Kubernetes sources: %+v %v", result, err)
	}
	t.Setenv("ITERION_AUTH_ROTATE", "1")
	if _, err := ObserveDeployment(t.Context(), kubectl, "fixture", "trusted/iterion-authority",
		[]string{"trusted", "worker"}, record.Queue, systemURL); err == nil ||
		!strings.Contains(err.Error(), "changed during observation") {
		t.Fatalf("production deployment observer accepted an authority Secret rotation: %v", err)
	}
}
