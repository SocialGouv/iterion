package runview

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRequiredSecretNames(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"forge_token": {As: "file"},                      // required
		"optional_x":  {As: "file", Optional: true},      // optional → excluded
		"inline_v":    {As: "env", Value: "literal-val"}, // inline value → excluded
	}}
	got := requiredSecretNames(wf)
	if len(got) != 1 || got[0] != "forge_token" {
		t.Fatalf("requiredSecretNames = %v, want [forge_token]", got)
	}
	if requiredSecretNames(nil) != nil {
		t.Fatal("nil workflow should return nil")
	}
}

func TestBuildExecutor_RequiredSecretUnresolvedFails(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	// Empty local store — the required secret resolves to nothing.
	spec := ExecutorSpec{
		Ctx:          context.Background(),
		Store:        st,
		RunID:        "run-1",
		Workflow:     &ir.Workflow{Name: "canary", Secrets: map[string]*ir.Secret{"test_e2e_canary": {As: "file"}}},
		LocalSecrets: secrets.NewMemoryGenericSecretStore(),
		LocalSealer:  sealer,
	}
	_, err = BuildExecutor(spec)
	if err == nil {
		t.Fatal("expected BuildExecutor to fail for an unresolved required secret")
	}
	if !strings.Contains(err.Error(), "test_e2e_canary") {
		t.Fatalf("error should name the secret, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "resolves to nothing") {
		t.Fatalf("error should explain the required-secret contract, got %q", err.Error())
	}
}

func TestBuildExecutor_OptionalSecretUnresolvedOK(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	spec := ExecutorSpec{
		Ctx:          context.Background(),
		Store:        st,
		RunID:        "run-1",
		Workflow:     &ir.Workflow{Name: "canary", Secrets: map[string]*ir.Secret{"test_e2e_canary": {As: "file", Optional: true}}},
		LocalSecrets: secrets.NewMemoryGenericSecretStore(),
		LocalSealer:  sealer,
	}
	if _, err := BuildExecutor(spec); err != nil {
		t.Fatalf("optional unresolved secret must not fail BuildExecutor: %v", err)
	}
}
