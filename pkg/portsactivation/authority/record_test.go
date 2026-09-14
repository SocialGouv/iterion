package authority

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	queuecore "github.com/SocialGouv/iterion/pkg/queue"
	queue "github.com/SocialGouv/iterion/pkg/queue/nats"
)

func validRecordFixture(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"version": RecordVersion, "deployment_revision": "release-17", "epoch": 3,
		"assertions": map[string]any{"broker_scope": true, "configuration": true,
			"workload_scope": true, "credential_custody": true, "issuer_scope": true, "writer_scope": true},
		"namespaces": []string{"trusted", "worker"},
		"queue": map[string]any{"account": "WORK", "system_account": "SYS",
			"stream": queue.StreamRuns, "consumer": queue.ConsumerRunners, "dlq_stream": queue.StreamRunsDLQ,
			"run_subject": queue.SubjectRuns, "dlq_subject": queue.SubjectRunsDLQ,
			"lock_bucket": queue.KVRunLocks, "rollout_bucket": queue.KVRolloutEpochs},
		"brokers": []any{map[string]any{"server_id": "NC123", "server_name": "nats-0",
			"namespace": "trusted", "pod_name": "nats-0", "container": "nats",
			"image_digest": "nats/server@sha256:" + strings.Repeat("a", 64),
			"config_path":  "/etc/nats/main.conf",
			"sources": map[string]any{"entry": "main.conf", "files": map[string]string{
				"main.conf": "private_password: super-secret", // source validation does not parse NATS
			}}}},
		"credentials": []any{map[string]any{"account": "WORK", "identity": "worker-user",
			"holder_ids": []string{"worker-1"}, "issuer_ids": []string{"operator"}}},
		"holders": []any{map[string]any{"id": "worker-1", "kind": "kubernetes",
			"namespace": "worker", "workload_kind": "Deployment", "workload_name": "iterion-runner",
			"container": "runner", "service_account": "iterion-runner",
			"image_digest": "iterion/runner@sha256:" + strings.Repeat("c", 64),
			"build_digest": strings.Repeat("b", 64), "access_scope": "runner",
			"credential_ref": "worker/nats-worker:NATS_URL"}},
		"build_approvals": []any{map[string]any{
			"image_digest":      "iterion/runner@sha256:" + strings.Repeat("c", 64),
			"build_digest":      strings.Repeat("b", 64),
			"capability_digest": strings.Repeat("a", 64), "queue_version": queuecore.SchemaVersion,
		}},
		"issuers":           []any{map[string]any{"id": "operator", "kind": "external", "identity": "deploy-operator"}},
		"permitted_writers": []any{map[string]any{"kind": "user", "name": "deploy-operator"}},
	}
}

func parseFixture(t *testing.T, fixture map[string]any) (*Record, error) {
	t.Helper()
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	return ParseRecord(SecretSource{material: raw})
}

func TestAuthorityRecordKeepsConcreteSourcesPrivate(t *testing.T) {
	record, err := parseFixture(t, validRecordFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if record.Version != RecordVersion || record.Epoch != 3 || len(record.Brokers) != 1 ||
		record.Brokers[0].Sources().Files["main.conf"] != "private_password: super-secret" {
		t.Fatal("authority record lost its concrete deployment source")
	}
	copyOfSources := record.Brokers[0].Sources()
	copyOfSources.Files["main.conf"] = "tampered"
	if record.Brokers[0].Sources().Files["main.conf"] != "private_password: super-secret" {
		t.Fatal("authority source accessor exposed a mutable internal map")
	}
	for _, value := range []any{record, *record, record.Brokers[0], &record.Brokers[0]} {
		for _, rendering := range []string{fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value)} {
			if strings.Contains(rendering, "super-secret") {
				t.Fatal("authority source leaked through diagnostic formatting")
			}
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil || bytes.Contains(encoded, []byte("super-secret")) {
		t.Fatal("authority source leaked through JSON serialization")
	}
}

func TestAuthorityRecordRejectsAmbiguousOrUnaccountedCustody(t *testing.T) {
	fixture := validRecordFixture(t)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"old-version", func(f map[string]any) { f["version"] = RecordVersion - 1 }},
		{"missing-completeness", func(f map[string]any) { f["assertions"].(map[string]any)["credential_custody"] = false }},
		{"case-aliased-completeness", func(f map[string]any) {
			f["assertions"].(map[string]any)["credential_custody"] = false
			f["assertions"].(map[string]any)["CREDENTIAL_CUSTODY"] = true
		}},
		{"unicode-aliased-completeness", func(f map[string]any) {
			f["assertions"].(map[string]any)["credential_custody"] = false
			f["assertions"].(map[string]any)["credential_cuſtody"] = true
		}},
		{"unknown-field", func(f map[string]any) { f["consumer_access_evidence"] = "approved" }},
		{"missing-source", func(f map[string]any) {
			f["brokers"].([]any)[0].(map[string]any)["sources"] = map[string]any{"entry": "missing.conf", "files": map[string]string{"main.conf": ""}}
		}},
		{"unaccounted-holder", func(f map[string]any) {
			f["credentials"].([]any)[0].(map[string]any)["holder_ids"] = []string{"missing"}
		}},
		{"orphan-issuer", func(f map[string]any) {
			f["issuers"] = append(f["issuers"].([]any), map[string]any{"id": "unclaimed", "kind": "external", "identity": "other-operator"})
		}},
		{"duplicate-holder-reference", func(f map[string]any) {
			f["credentials"].([]any)[0].(map[string]any)["holder_ids"] = []string{"worker-1", "worker-1"}
		}},
		{"duplicate-issuer-reference", func(f map[string]any) {
			f["credentials"].([]any)[0].(map[string]any)["issuer_ids"] = []string{"operator", "operator"}
		}},
		{"mutable-image", func(f map[string]any) { f["holders"].([]any)[0].(map[string]any)["image_digest"] = "iterion:latest" }},
		{"missing-build-approvals", func(f map[string]any) { f["build_approvals"] = []any{} }},
		{"duplicate-build-image", func(f map[string]any) {
			f["build_approvals"] = append(f["build_approvals"].([]any), f["build_approvals"].([]any)[0])
		}},
		{"unsupported-build-capability", func(f map[string]any) {
			f["build_approvals"].([]any)[0].(map[string]any)["capability_digest"] = "unverified"
		}},
		{"unsupported-build-queue", func(f map[string]any) {
			f["build_approvals"].([]any)[0].(map[string]any)["queue_version"] = queuecore.SchemaVersion - 1
		}},
		{"cross-namespace-secret", func(f map[string]any) {
			f["holders"].([]any)[0].(map[string]any)["credential_ref"] = "trusted/nats-worker:NATS_URL"
		}},
		{"unsupported-workload", func(f map[string]any) {
			f["holders"].([]any)[0].(map[string]any)["workload_kind"] = "Widget"
		}},
		{"missing-writers", func(f map[string]any) { f["permitted_writers"] = []any{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := validRecordFixture(t)
			test.mutate(copy)
			if _, err := parseFixture(t, copy); err == nil {
				t.Fatal("incomplete or ambiguous authority record was accepted")
			}
		})
	}
	if _, err := parseFixture(t, fixture); err != nil {
		t.Fatalf("valid fixture changed during rejection cases: %v", err)
	}
}

func TestAuthorityRecordRejectsDuplicateJSONKeys(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"version":1,"version":1}`),
		[]byte(`{"version":1,"VERSION":1}`),
		[]byte(`{"sources":{"files":{"main.conf":"first","main.conf":"second"}}}`),
	} {
		if _, err := ParseRecord(SecretSource{material: raw}); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("duplicate authority key was accepted or misdiagnosed: %v", err)
		}
	}
}

func TestAuthorityRecordPreservesCaseSensitiveSourceFiles(t *testing.T) {
	fixture := validRecordFixture(t)
	files := fixture["brokers"].([]any)[0].(map[string]any)["sources"].(map[string]any)["files"].(map[string]string)
	files["A.conf"] = "port: 4222"
	files["a.conf"] = "port: 4223"
	record, err := parseFixture(t, fixture)
	if err != nil {
		t.Fatalf("case-distinct NATS source paths were rejected: %v", err)
	}
	got := record.Brokers[0].Sources().Files
	if got["A.conf"] == got["a.conf"] {
		t.Fatal("case-distinct NATS source files were collapsed")
	}
}
