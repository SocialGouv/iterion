package kubernetes

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// decodedSecretValues returns the set of decoded plaintext values in a
// rendered Secret manifest, so a test can assert on content without
// depending on the exact indexed key names.
func decodedSecretValues(t *testing.T, manifest []byte) map[string]bool {
	t.Helper()
	var sec map[string]any
	if err := json.Unmarshal(manifest, &sec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, _ := sec["data"].(map[string]any)
	out := map[string]bool{}
	for _, raw := range data {
		b64, _ := raw.(string)
		dec, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("data not base64: %v", err)
		}
		out[string(dec)] = true
	}
	return out
}

// TestRenderRefreshedSecret pins the k8s mid-run refresh: re-rendering the
// per-run Secret with one key rotated must update that key, leave the
// others at their current snapshot, and persist the update so a later
// refresh of a DIFFERENT key doesn't revert the first. Unknown names and
// empty values are errors; a run with no Secret refuses.
func TestRenderRefreshedSecret(t *testing.T) {
	r := &Run{
		namespace:             "ns",
		secretFilesSecretName: "iterion-run-x-secret-files",
		info:                  sandbox.RunInfo{RunID: "x", FriendlyName: "friendly"},
		secretFiles: []sandbox.SecretFileMount{
			{Name: "forge_token", MountPath: "/run/iterion/secrets/forge_token", Value: []byte("token-v1")},
			{Name: "other", MountPath: "/run/iterion/secrets/other", Value: []byte("other-v1")},
		},
	}

	manifest, err := r.renderRefreshedSecret("forge_token", []byte("token-v2"))
	if err != nil {
		t.Fatalf("renderRefreshedSecret: %v", err)
	}
	vals := decodedSecretValues(t, manifest)
	if !vals["token-v2"] || !vals["other-v1"] || vals["token-v1"] {
		t.Fatalf("after refresh #1, values = %v", vals)
	}

	// Rotate the OTHER key; the first refresh must survive (snapshot
	// persisted), not revert to token-v1.
	manifest, err = r.renderRefreshedSecret("other", []byte("other-v2"))
	if err != nil {
		t.Fatalf("renderRefreshedSecret #2: %v", err)
	}
	vals = decodedSecretValues(t, manifest)
	if !vals["token-v2"] || !vals["other-v2"] || vals["token-v1"] || vals["other-v1"] {
		t.Fatalf("after refresh #2, values = %v", vals)
	}

	if _, err := r.renderRefreshedSecret("nope", []byte("x")); err == nil {
		t.Fatal("refresh of unknown secret must error")
	}
	if _, err := r.renderRefreshedSecret("forge_token", nil); err == nil {
		t.Fatal("refresh with empty value must error")
	}

	empty := &Run{namespace: "ns", info: sandbox.RunInfo{RunID: "y"}}
	if _, err := empty.renderRefreshedSecret("forge_token", []byte("v")); err == nil {
		t.Fatal("refresh on a run with no file-secrets Secret must error")
	}
}
