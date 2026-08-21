package kubernetes

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
)

func TestBuildPodManifest_CAInjection(t *testing.T) {
	out, err := BuildPodManifest(PodManifestInput{
		Namespace:    "ns",
		Name:         "iterion-run-x",
		RunID:        "x",
		Spec:         sandbox.Spec{Image: "img"},
		CASecretName: "iterion-run-x-ca",
	})
	if err != nil {
		t.Fatalf("BuildPodManifest: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "CURL_CA_BUNDLE",
		"GIT_SSL_CAINFO", "REQUESTS_CA_BUNDLE",
		caContainerPath, caMountDir, "iterion-egress-ca", "iterion-run-x-ca",
		// The JVM leg: dedicated vars + a JAVA_TOOL_OPTIONS flag pointing
		// at the mounted PKCS12 truststore.
		"ITERION_JAVA_TRUSTSTORE", trustStoreContainerPath,
		"-Djavax.net.ssl.trustStore=" + trustStoreContainerPath,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("CA-injected manifest missing %q:\n%s", want, s)
		}
	}

	// No CA wiring when no secret is configured.
	bare, err := BuildPodManifest(PodManifestInput{
		Namespace: "ns", Name: "iterion-run-y", Spec: sandbox.Spec{Image: "img"},
	})
	if err != nil {
		t.Fatalf("BuildPodManifest (bare): %v", err)
	}
	if strings.Contains(string(bare), "NODE_EXTRA_CA_CERTS") || strings.Contains(string(bare), "iterion-egress-ca") {
		t.Errorf("CA wiring leaked into a manifest with no CA secret:\n%s", bare)
	}
}

// testCAPEM returns a real ephemeral CA cert: BuildCASecret derives the
// JVM truststore from the PEM, so a dummy blob is a named refusal.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	ca, err := netproxy.NewEphemeralCA()
	if err != nil {
		t.Fatalf("ephemeral CA: %v", err)
	}
	return ca.CertPEM()
}

func TestBuildCASecret(t *testing.T) {
	pem := testCAPEM(t)
	out, err := BuildCASecret("ns", "iterion-run-x-ca", "x", "friendly", pem, nil)
	if err != nil {
		t.Fatalf("BuildCASecret: %v", err)
	}
	var sec map[string]any
	if err := json.Unmarshal(out, &sec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sec["kind"] != "Secret" || sec["type"] != "Opaque" {
		t.Errorf("unexpected kind/type: %v / %v", sec["kind"], sec["type"])
	}
	data, _ := sec["data"].(map[string]any)
	b64, _ := data[caSecretKey].(string)
	dec, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("data not base64: %v", err)
	}
	if string(dec) != string(pem) {
		t.Errorf("decoded CA data mismatch:\n got %q\nwant %q", dec, pem)
	}
	// The JVM truststore ships in the same secret: the CA env vars cover
	// OpenSSL-family clients but a JVM only trusts a keystore-typed file.
	tsB64, _ := data[trustStoreSecretKey].(string)
	if ts, err := base64.StdEncoding.DecodeString(tsB64); err != nil || len(ts) == 0 {
		t.Errorf("truststore key %s missing/empty in CA secret (err=%v)", trustStoreSecretKey, err)
	}
}

func TestBuildCASecretRefusesGarbagePEM(t *testing.T) {
	if _, err := BuildCASecret("ns", "n", "x", "f", []byte("not a pem"), nil); err == nil {
		t.Fatal("garbage CA PEM produced a secret, want a named refusal")
	}
}

func TestBuildPodManifest_SecretFilesInjection(t *testing.T) {
	out, err := BuildPodManifest(PodManifestInput{
		Namespace: "ns",
		Name:      "iterion-run-x",
		RunID:     "x",
		Spec: sandbox.Spec{
			Image: "img",
			SecretFiles: []sandbox.SecretFileMount{{
				Name:      "kubeconfig",
				MountPath: "/run/iterion/secrets/kubeconfig",
				Value:     []byte("apiVersion: v1"),
			}},
		},
		SecretFilesSecretName: "iterion-run-x-secret-files",
	})
	if err != nil {
		t.Fatalf("BuildPodManifest: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"iterion-secret-files",
		"iterion-run-x-secret-files",
		"/run/iterion/secrets",
		"kubeconfig",
		"readOnly",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("secret-files manifest missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "subPath") {
		t.Errorf("default secret directory should be mounted as a volume, not via subPath:\n%s", s)
	}
}

func TestBuildPodManifest_SecretFilesInjectionCustomPath(t *testing.T) {
	out, err := BuildPodManifest(PodManifestInput{
		Namespace: "ns",
		Name:      "iterion-run-x",
		RunID:     "x",
		Spec: sandbox.Spec{
			Image: "img",
			SecretFiles: []sandbox.SecretFileMount{{
				Name:      "kubeconfig",
				MountPath: "/root/.kube/config",
				Value:     []byte("apiVersion: v1"),
			}},
		},
		SecretFilesSecretName: "iterion-run-x-secret-files",
	})
	if err != nil {
		t.Fatalf("BuildPodManifest: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"iterion-secret-files",
		"iterion-run-x-secret-files",
		"/root/.kube/config",
		"subPath",
		"readOnly",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("secret-files manifest missing %q:\n%s", want, s)
		}
	}
}

func TestBuildSecretFilesSecret(t *testing.T) {
	out, err := BuildSecretFilesSecret("ns", "iterion-run-x-secret-files", "x", "friendly", []sandbox.SecretFileMount{{
		Name:  "kubeconfig",
		Value: []byte("apiVersion: v1"),
	}}, nil)
	if err != nil {
		t.Fatalf("BuildSecretFilesSecret: %v", err)
	}
	var sec map[string]any
	if err := json.Unmarshal(out, &sec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sec["kind"] != "Secret" || sec["type"] != "Opaque" {
		t.Errorf("unexpected kind/type: %v / %v", sec["kind"], sec["type"])
	}
	data, _ := sec["data"].(map[string]any)
	if len(data) != 1 {
		t.Fatalf("data = %+v", data)
	}
	for _, raw := range data {
		b64, _ := raw.(string)
		dec, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("data not base64: %v", err)
		}
		if string(dec) != "apiVersion: v1" {
			t.Fatalf("decoded payload = %q", dec)
		}
	}
}
