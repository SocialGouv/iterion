package authority

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func brokerConfigSecretFixture(t *testing.T) (*Record, *WorkloadSnapshot, *BrokerConfigSecret) {
	t.Helper()
	record, snapshot := brokerLaunchFixture(t)
	var spec map[string]any
	if err := json.Unmarshal(snapshot.Workloads[0].podSpec, &spec); err != nil {
		t.Fatal(err)
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	container["volumeMounts"] = []any{map[string]any{
		"name": "nats-config", "mountPath": "/etc/nats", "readOnly": true,
	}}
	spec["volumes"] = []any{map[string]any{"name": "nats-config", "secret": map[string]any{
		"secretName": "nats-config", "optional": false,
	}}}
	snapshot.Workloads[0].podSpec, _ = json.Marshal(spec)
	secret := &BrokerConfigSecret{Namespace: "trusted", Name: "nats-config", UID: "uid-config",
		ResourceVersion: "21", data: map[string][]byte{"main.conf": []byte(record.Brokers[0].sources.Files["main.conf"])}}
	return record, snapshot, secret
}

func TestNATSBrokerConfigSecretBindsProjectedSourceBytes(t *testing.T) {
	record, snapshot, secret := brokerConfigSecretFixture(t)
	result, err := reconcileBrokerConfigSecretsWithReader(t.Context(), record, snapshot,
		func(_ context.Context, namespace, name string) (*BrokerConfigSecret, error) {
			if namespace != "trusted" || name != "nats-config" {
				t.Fatal("configuration Secret lookup escaped declared source")
			}
			return secret, nil
		})
	if err != nil || len(result) != 1 || result[0].SecretUID != "uid-config" ||
		result[0].PodUID != "uid-nats-pod" {
		t.Fatalf("NATS broker source bytes were not bound to the Pod: %+v %v", result, err)
	}
	for _, rendered := range []string{fmt.Sprintf("%+v", secret), fmt.Sprintf("%#v", *secret)} {
		if strings.Contains(rendered, "super-secret") {
			t.Fatal("configuration Secret was exposed through formatting")
		}
	}
	encoded, err := json.Marshal(secret)
	if err != nil || bytes.Contains(encoded, []byte("super-secret")) {
		t.Fatal("configuration Secret was exposed through JSON")
	}
}

func TestNATSBrokerConfigSecretBindsNestedIncludesWithoutShadowMounts(t *testing.T) {
	record, snapshot, secret := brokerConfigSecretFixture(t)
	record.Brokers[0].sources.Files = map[string]string{
		"main.conf": `include "sub/extra.conf"`, "sub/extra.conf": "private_password: super-secret",
	}
	secret.data = map[string][]byte{"entry": []byte(`include "sub/extra.conf"`),
		"include": []byte("private_password: super-secret")}
	changeBrokerPodSpec(t, snapshot, func(spec map[string]any) {
		spec["volumes"].([]any)[0].(map[string]any)["secret"].(map[string]any)["items"] = []any{
			map[string]any{"key": "entry", "path": "main.conf"},
			map[string]any{"key": "include", "path": "sub/extra.conf"},
		}
	})
	read := func(context.Context, string, string) (*BrokerConfigSecret, error) { return secret, nil }
	if _, err := reconcileBrokerConfigSecretsWithReader(t.Context(), record, snapshot, read); err != nil {
		t.Fatalf("nested projected NATS source was rejected: %v", err)
	}
	changeBrokerPodSpec(t, snapshot, func(spec map[string]any) {
		mounts := spec["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)
		spec["containers"].([]any)[0].(map[string]any)["volumeMounts"] = append(mounts,
			map[string]any{"name": "shadow", "mountPath": "/etc/nats/sub", "readOnly": true})
	})
	if _, err := reconcileBrokerConfigSecretsWithReader(t.Context(), record, snapshot, read); err == nil {
		t.Fatal("mount shadowing an included file was accepted")
	}
}

func TestNATSBrokerConfigSecretRefusesUnboundOrShadowedSources(t *testing.T) {
	for name, mutate := range map[string]func(*Record, *WorkloadSnapshot, *BrokerConfigSecret){
		"wrong source bytes":  func(_ *Record, _ *WorkloadSnapshot, s *BrokerConfigSecret) { s.data["main.conf"] = []byte("changed") },
		"missing source file": func(_ *Record, _ *WorkloadSnapshot, s *BrokerConfigSecret) { delete(s.data, "main.conf") },
		"wrong entry path": func(r *Record, _ *WorkloadSnapshot, _ *BrokerConfigSecret) {
			r.Brokers[0].ConfigPath = "/other/main.conf"
		},
		"mutable mount": func(_ *Record, s *WorkloadSnapshot, _ *BrokerConfigSecret) {
			changeBrokerPodSpec(t, s, func(spec map[string]any) {
				spec["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)[0].(map[string]any)["readOnly"] = false
			})
		},
		"subPath mount": func(_ *Record, s *WorkloadSnapshot, _ *BrokerConfigSecret) {
			changeBrokerPodSpec(t, s, func(spec map[string]any) {
				spec["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)[0].(map[string]any)["subPath"] = "main.conf"
			})
		},
		"shadowed source": func(_ *Record, s *WorkloadSnapshot, _ *BrokerConfigSecret) {
			changeBrokerPodSpec(t, s, func(spec map[string]any) {
				mounts := spec["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)
				spec["containers"].([]any)[0].(map[string]any)["volumeMounts"] = append(mounts,
					map[string]any{"name": "shadow", "mountPath": "/etc/nats/main.conf", "readOnly": true})
			})
		},
		"shadowed Secret symlink directory": func(_ *Record, s *WorkloadSnapshot, _ *BrokerConfigSecret) {
			changeBrokerPodSpec(t, s, func(spec map[string]any) {
				mounts := spec["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)
				spec["containers"].([]any)[0].(map[string]any)["volumeMounts"] = append(mounts,
					map[string]any{"name": "shadow", "mountPath": "/etc/nats/..data", "readOnly": true})
			})
		},
		"shadowed Secret symlink target": func(_ *Record, s *WorkloadSnapshot, _ *BrokerConfigSecret) {
			changeBrokerPodSpec(t, s, func(spec map[string]any) {
				mounts := spec["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)
				spec["containers"].([]any)[0].(map[string]any)["volumeMounts"] = append(mounts,
					map[string]any{"name": "shadow", "mountPath": "/etc/nats/..data/main.conf", "readOnly": true})
			})
		},
		"noncanonical shadow mount": func(_ *Record, s *WorkloadSnapshot, _ *BrokerConfigSecret) {
			changeBrokerPodSpec(t, s, func(spec map[string]any) {
				mounts := spec["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)
				spec["containers"].([]any)[0].(map[string]any)["volumeMounts"] = append(mounts,
					map[string]any{"name": "shadow", "mountPath": "/etc/nats/./main.conf", "readOnly": true})
			})
		},
		"optional Secret": func(_ *Record, s *WorkloadSnapshot, _ *BrokerConfigSecret) {
			changeBrokerPodSpec(t, s, func(spec map[string]any) {
				spec["volumes"].([]any)[0].(map[string]any)["secret"].(map[string]any)["optional"] = true
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			record, snapshot, secret := brokerConfigSecretFixture(t)
			mutate(record, snapshot, secret)
			if _, err := reconcileBrokerConfigSecretsWithReader(t.Context(), record, snapshot,
				func(context.Context, string, string) (*BrokerConfigSecret, error) { return secret, nil }); err == nil {
				t.Fatal("unbound NATS broker configuration source was accepted")
			}
		})
	}
}

func TestKubernetesBrokerConfigSecretReaderBindsIdentityAndRedactsData(t *testing.T) {
	document := credentialSecretDocument("trusted", "nats-config", "main.conf", "private_password: super-secret")
	binary, fixture := credentialSecretShim(t, document)
	secret, err := ReadBrokerConfigSecret(t.Context(), binary, "fixture-context", "trusted", "nats-config")
	if err != nil || secret.UID != "uid-credential" || string(secret.data["main.conf"]) != "private_password: super-secret" {
		t.Fatalf("named broker configuration Secret could not be read: %+v %v", secret, err)
	}
	document["data"].(map[string]string)["main.conf"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 600000)))
	writeCredentialDocument(t, fixture, document)
	if _, err := ReadBrokerConfigSecret(t.Context(), binary, "", "trusted", "nats-config"); err == nil {
		t.Fatal("oversized broker configuration Secret was accepted")
	}
	t.Setenv("ITERION_NATS_CREDENTIAL_FAIL", "1")
	if _, err := ReadBrokerConfigSecret(t.Context(), binary, "", "trusted", "nats-config"); err == nil ||
		strings.Contains(err.Error(), "private-password") {
		t.Fatalf("broker configuration read exposed kubectl stderr: %v", err)
	}
}

func changeBrokerPodSpec(t *testing.T, snapshot *WorkloadSnapshot, change func(map[string]any)) {
	t.Helper()
	var spec map[string]any
	if err := json.Unmarshal(snapshot.Workloads[0].podSpec, &spec); err != nil {
		t.Fatal(err)
	}
	change(spec)
	snapshot.Workloads[0].podSpec, _ = json.Marshal(spec)
}
