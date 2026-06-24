package kubernetes

import "testing"

func TestPullPolicyForImage(t *testing.T) {
	cases := map[string]string{
		// mutable tags must re-pull so a fresh CI bake isn't shadowed by a cache
		"ghcr.io/socialgouv/iterion-sandbox-full:edge":   "Always",
		"ghcr.io/socialgouv/iterion-sandbox-full:v1.2.3": "Always",
		"alpine": "Always",
		// a pinned digest is immutable → no needless re-pull
		"ghcr.io/x/y@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": "IfNotPresent",
	}
	for img, want := range cases {
		if got := pullPolicyForImage(img); got != want {
			t.Errorf("pullPolicyForImage(%q) = %q, want %q", img, got, want)
		}
	}
}
