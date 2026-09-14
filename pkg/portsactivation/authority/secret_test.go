package authority

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadAuthoritySecretBindsNamedKubernetesSource(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "kubectl")
	fixture := filepath.Join(dir, "response.json")
	argsPath := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ITERION_AUTH_ARGS_PATH\"\n" +
		"if [ \"$ITERION_AUTH_FAIL\" = 1 ]; then echo private-credential >&2; exit 1; fi\n" +
		"cat \"$ITERION_AUTH_FIXTURE_PATH\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_AUTH_ARGS_PATH", argsPath)
	t.Setenv("ITERION_AUTH_FIXTURE_PATH", fixture)
	material := []byte(`{"version":1,"sources":{"entry":"main.conf"}}`)
	document := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]string{"namespace": "1trusted", "name": "iterion.authority", "uid": "uid-1", "resourceVersion": "17"},
		"data":     map[string]string{"authority.json": base64.StdEncoding.EncodeToString(material)},
	}
	writeDocument := func() {
		t.Helper()
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeDocument()
	source, err := ReadAuthoritySecret(t.Context(), binary, "fixture-context", "1trusted", "iterion.authority")
	if err != nil {
		t.Fatal(err)
	}
	if source.UID != "uid-1" || source.ResourceVersion != "17" || !bytes.Equal(source.Material(), material) {
		t.Fatal("Kubernetes Secret source lost identity or exact material")
	}
	for _, rendered := range []string{
		fmt.Sprintf("%+v", source), fmt.Sprintf("%+v", *source),
		fmt.Sprintf("%#v", source), fmt.Sprintf("%#v", *source),
	} {
		if rendered != "Kubernetes authority Secret [material redacted]" {
			t.Fatal("formatted authority source exposed confidential material")
		}
	}
	encodedSource, err := json.Marshal(source)
	if err != nil || bytes.Contains(encodedSource, []byte("sources")) {
		t.Fatal("JSON-encoded authority source exposed confidential material")
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--context", "fixture-context", "--namespace", "1trusted", "get", "secret", "iterion.authority", "-o", "json"}; !reflect.DeepEqual(strings.Split(strings.TrimSpace(string(args)), "\n"), want) {
		t.Fatal("authority reader did not use a scoped named Secret request")
	}
	document["metadata"] = map[string]string{"namespace": "other", "name": "iterion.authority", "uid": "uid-1", "resourceVersion": "17"}
	writeDocument()
	if _, err := ReadAuthoritySecret(t.Context(), binary, "", "1trusted", "iterion.authority"); err == nil {
		t.Fatal("Secret from another namespace was accepted")
	}
	t.Setenv("ITERION_AUTH_FAIL", "1")
	if _, err := ReadAuthoritySecret(t.Context(), binary, "", "1trusted", "iterion.authority"); err == nil ||
		strings.Contains(err.Error(), "private-credential") {
		t.Fatalf("kubectl diagnostic leaked credential content: %v", err)
	}
}

func TestReadAuthoritySecretRefusesUnboundNamesAndOversizedResponses(t *testing.T) {
	for _, name := range []string{"", "--help", "a..b", strings.Repeat("a", 254)} {
		if _, err := ReadAuthoritySecret(t.Context(), "kubectl", "", "trusted", name); err == nil {
			t.Fatalf("invalid Secret name %q reached kubectl", name)
		}
	}
	b := &boundedKubectlOutput{limit: 4}
	if _, err := b.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("5")); err == nil {
		t.Fatal("oversized Kubernetes response was not bounded")
	}
}

func TestReadAuthoritySecretBoundsActualKubectlPipe(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "kubectl")
	fixture := filepath.Join(dir, "response.json")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ncat \"$ITERION_AUTH_FIXTURE_PATH\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_AUTH_FIXTURE_PATH", fixture)
	// The trailing JSON would be accepted if os/exec bypassed Write by
	// calling the promoted bytes.Buffer.ReadFrom method.
	valid := []byte(`{"apiVersion":"v1","kind":"Secret","type":"Opaque","metadata":{"namespace":"trusted","name":"authority","uid":"uid","resourceVersion":"1"},"data":{"authority.json":"e30="}}`)
	response := append(bytes.Repeat([]byte(" "), maxSecretResponse+1), valid...)
	if err := os.WriteFile(fixture, response, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAuthoritySecret(t.Context(), binary, "", "trusted", "authority"); err == nil {
		t.Fatal("oversized kubectl pipe was accepted despite a small valid material payload")
	}
}

func TestReadAuthoritySecretCancellationBoundsInheritedPipe(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "kubectl")
	ready := filepath.Join(dir, "ready")
	// The shell exits on cancellation, but its child retains the stdout
	// descriptor. Without WaitDelay, os/exec waits for that child to exit.
	script := "#!/bin/sh\nsleep 5 &\nprintf ready > \"$ITERION_AUTH_READY_PATH\"\nwait\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_AUTH_READY_PATH", ready)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ReadAuthoritySecret(ctx, binary, "", "trusted", "authority")
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("kubectl wrapper did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled authority read was accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("authority read waited for an inherited stdout pipe after cancellation")
	}
}
