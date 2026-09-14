package authority

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDeploymentRecordBindsSecretToConfiguredQueueAndNamespaces(t *testing.T) {
	fixture := validRecordFixture(t)
	material, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]string{"namespace": "trusted", "name": "iterion-authority",
			"uid": "authority-uid", "resourceVersion": "17"},
		"data": map[string]string{"authority.json": base64.StdEncoding.EncodeToString(material)},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "kubectl")
	response := filepath.Join(dir, "response.json")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ncat \"$ITERION_AUTH_FIXTURE_PATH\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(response, document, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_AUTH_FIXTURE_PATH", response)
	expected, err := parseFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	record, source, err := ReadDeploymentRecord(t.Context(), binary, "fixture", "trusted/iterion-authority",
		[]string{"worker", "trusted"}, expected.Queue)
	if err != nil || record.DeploymentRevision != "release-17" || source.UID != "authority-uid" {
		t.Fatalf("trusted deployment record was not bound to the configured scope: %+v %+v %v", record, source, err)
	}
	changedQueue := expected.Queue
	changedQueue.Consumer = "other-consumer"
	if _, _, err := ReadDeploymentRecord(t.Context(), binary, "fixture", "trusted/iterion-authority",
		[]string{"worker", "trusted"}, changedQueue); err == nil {
		t.Fatal("authority record for another durable consumer was accepted")
	}
	if _, _, err := ReadDeploymentRecord(t.Context(), binary, "fixture", "trusted/iterion-authority",
		[]string{"trusted"}, expected.Queue); err == nil {
		t.Fatal("authority record omitted a configured namespace")
	}
	if _, _, err := ReadDeploymentRecord(t.Context(), binary, "fixture", "trusted/other-secret",
		[]string{"worker", "trusted"}, expected.Queue); err == nil {
		t.Fatal("authority record came from another named Secret")
	}
}
