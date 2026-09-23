package model

import (
	"os"
	"strings"
	"testing"
)

// The routing that failed in production on 2026-09-19: a spec like
// "zai/glm-5.3" used to die with `unknown provider "zai"` and loop on
// auto-resume. These tests pin the three behaviours the fix promises.

// The provider exists: the BYOK path (what cloud runs use) builds a client
// from the tenant's key — the keyed factory is read directly, white-box,
// because the package-external surface goes through ResolveWithContext's
// credential injection.
func TestZaiRegistry_KeyedResolveBuildsClient(t *testing.T) {
	r := NewRegistry()
	factory, ok := r.providersWithKey["zai"]
	if !ok {
		t.Fatal("no keyed factory registered for provider zai")
	}
	client, err := factory("glm-5.3", "zai-key")
	if err != nil {
		t.Fatalf("keyed factory(glm-5.3): %v", err)
	}
	if client == nil {
		t.Fatal("nil client for a resolvable spec")
	}
}

// The env path names its missing credential instead of building a client
// that can only answer 401: the error says ZAI_API_KEY, so an operator
// reads the remedy, not a mystery.
func TestZaiRegistry_EnvResolveNamesMissingCredential(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "")
	os.Unsetenv("ZAI_API_KEY")
	r := NewRegistry()
	_, err := r.Resolve("zai/glm-5.3")
	if err == nil {
		t.Fatal("expected an error without a credential")
	}
	if !strings.Contains(err.Error(), "ZAI_API_KEY") {
		t.Errorf("error %q does not name the remedy (ZAI_API_KEY)", err)
	}
}

// With the env key set, the plain resolve builds the client too.
func TestZaiRegistry_EnvResolveWithKey(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "zai-key")
	r := NewRegistry()
	client, err := r.Resolve("zai/glm-5.3")
	if err != nil {
		t.Fatalf("Resolve(zai/glm-5.3): %v", err)
	}
	if client == nil {
		t.Fatal("nil client")
	}
}
