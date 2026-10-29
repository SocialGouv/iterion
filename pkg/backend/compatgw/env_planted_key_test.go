package compatgw

import "testing"

// A keyless operator gateway is a supported configuration — the base URL is
// the presence signal, and a gateway may sit behind network auth alone — so
// the host crosses no key and the name is ABSENT in the container. The
// in-container runner auto-loads the `.env` of the repository under review,
// which fills exactly the absent names, and this key is the credential the
// gateway factory presents on every call: on a LiteLLM-style gateway it
// picks which virtual key — hence which budget, model allowlist and tenant
// — its reviewer's calls are billed to. A planted key is therefore not the
// operator's: it is dropped, and the gateway is dialled with no auth.
//
// Dropped, never replaced by an explicit empty: an empty key reads as "auth
// attempted, invalid" to some gateways, where no key at all reads as "no
// auth".
func TestFromEnv_APlantedApiKeyIsNotTheOperators(t *testing.T) {
	const (
		base    = "https://198.51.100.7"
		repoKey = "sk-chosen-by-the-repo-under-review"
	)
	get := mapGet(map[string]string{BaseURLEnv: base, APIKeyEnv: repoKey})

	// Inherited, the key authenticates as it always did.
	inheritEnv(t)
	cfg, err := FromEnv(get)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.APIKey != repoKey {
		t.Fatalf("an inherited key read %q, want the operator's: the deployment lost its gateway auth", cfg.APIKey)
	}

	// The same bytes, planted by a workspace `.env`.
	plantEnv(t, APIKeyEnv)
	cfg, err = FromEnv(get)
	if err != nil {
		t.Fatalf("FromEnv with a planted key: %v", err)
	}
	if cfg.APIKey != "" {
		t.Fatalf("the repository under review chose the credential its reviewer's gateway calls present: %q", cfg.APIKey)
	}
	// The endpoint is the operator's own — crossed by the host for every
	// gateway node — and the gateway stays configured: no auth is a
	// gateway state, not a refusal.
	if cfg.BaseURL != base {
		t.Fatalf("BaseURL = %q, want %q: the refusal must cost the endpoint nothing", cfg.BaseURL, base)
	}
}
