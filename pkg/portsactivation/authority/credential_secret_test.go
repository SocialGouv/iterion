package authority

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func credentialSecretDocument(namespace, name, key, value string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"namespace": namespace, "name": name, "uid": "uid-credential",
			"resourceVersion": "19"},
		"data": map[string]string{key: base64.StdEncoding.EncodeToString([]byte(value)),
			"other-key": base64.StdEncoding.EncodeToString([]byte("non-NATS material"))}}
}

func credentialSecretShim(t *testing.T, document map[string]any) (binary, fixture string) {
	t.Helper()
	dir := t.TempDir()
	binary = filepath.Join(dir, "kubectl")
	fixture = filepath.Join(dir, "response.json")
	script := "#!/bin/sh\nif [ \"$ITERION_NATS_CREDENTIAL_FAIL\" = 1 ]; then echo private-password >&2; exit 1; fi\ncat \"$ITERION_NATS_CREDENTIAL_FIXTURE\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_NATS_CREDENTIAL_FIXTURE", fixture)
	writeCredentialDocument(t, fixture, document)
	return
}

func writeCredentialDocument(t *testing.T, fixture string, document map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesCredentialSecretBindsNamedKeyAndRedactsURL(t *testing.T) {
	value := "nats://worker-user:private-password@nats.example:4222"
	binary, _ := credentialSecretShim(t, credentialSecretDocument("worker", "nats-worker", "NATS_URL", value))
	credential, err := ReadCredentialSecretKey(t.Context(), binary, "fixture-context", "worker", "nats-worker", "NATS_URL")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := credential.Identity()
	if err != nil || identity != "worker-user" || credential.UID != "uid-credential" ||
		credential.ResourceVersion != "19" || string(credential.Value()) != value {
		t.Fatal("named NATS Secret key lost its value or Kubernetes source identity")
	}
	copyOfValue := credential.Value()
	copyOfValue[0] = 'X'
	if string(credential.Value()) != value {
		t.Fatal("NATS Secret source accessor exposed mutable internal bytes")
	}
	for _, rendered := range []string{
		fmt.Sprintf("%+v", credential), fmt.Sprintf("%+v", *credential),
		fmt.Sprintf("%#v", credential), fmt.Sprintf("%#v", *credential),
	} {
		if strings.Contains(rendered, "private-password") {
			t.Fatal("NATS URL leaked through source formatting")
		}
	}
	encoded, err := json.Marshal(credential)
	if err != nil || bytes.Contains(encoded, []byte("private-password")) {
		t.Fatal("NATS URL leaked through source JSON")
	}
}

func TestKubernetesCredentialSecretRefusesForeignOrUnsupportedMaterial(t *testing.T) {
	valid := credentialSecretDocument("worker", "nats-worker", "NATS_URL", "nats://worker:private-password@nats:4222")
	binary, fixture := credentialSecretShim(t, valid)
	for name, document := range map[string]map[string]any{
		"foreign namespace": credentialSecretDocument("other", "nats-worker", "NATS_URL", "nats://worker:secret@nats:4222"),
		"missing key":       credentialSecretDocument("worker", "nats-worker", "OTHER_URL", "nats://worker:secret@nats:4222"),
		"anonymous URL":     credentialSecretDocument("worker", "nats-worker", "NATS_URL", "nats://nats:4222"),
		"server list":       credentialSecretDocument("worker", "nats-worker", "NATS_URL", "nats://worker:secret@host-a,host-b"),
		"query credentials": credentialSecretDocument("worker", "nats-worker", "NATS_URL", "nats://worker:secret@nats:4222?token=private"),
		"oversized URL":     credentialSecretDocument("worker", "nats-worker", "NATS_URL", "nats://worker:secret@nats:4222/"+strings.Repeat("a", maxCredentialURLBytes)),
	} {
		t.Run(name, func(t *testing.T) {
			writeCredentialDocument(t, fixture, document)
			if _, err := ReadCredentialSecretKey(t.Context(), binary, "", "worker", "nats-worker", "NATS_URL"); err == nil {
				t.Fatal("unsupported NATS credential source was accepted")
			}
		})
	}
	for _, key := range []string{"", "../password", "--help", strings.Repeat("a", 254)} {
		if _, err := ReadCredentialSecretKey(t.Context(), binary, "", "worker", "nats-worker", key); err == nil {
			t.Fatal("invalid Kubernetes Secret key reached kubectl")
		}
	}
	t.Setenv("ITERION_NATS_CREDENTIAL_FAIL", "1")
	if _, err := ReadCredentialSecretKey(t.Context(), binary, "", "worker", "nats-worker", "NATS_URL"); err == nil ||
		strings.Contains(err.Error(), "private-password") {
		t.Fatalf("kubectl diagnostic exposed NATS credential: %v", err)
	}
}
