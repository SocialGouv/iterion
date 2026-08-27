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

// TestStampLocalCredentials_ReachesTheEngineContext pins WHERE the resolved
// credentials have to land. The engine mounts declared file secrets into the
// sandbox at run start, reading them from the context handed to Run — so a
// caller that stamps only the executor's own context ships a container with
// no secret files, and an optional secret is skipped in silence. That is how
// a docs bot's PR tail reported "no forge_token secret" on a host whose
// store held exactly that secret.
func TestStampLocalCredentials_ReachesTheEngineContext(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	memStore := secrets.NewMemoryGenericSecretStore()
	const secretID = "sec-forge-token"
	sealed, err := secrets.SealGenericSecret(sealer, secretID, []byte("jeton-de-test"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := memStore.Create(context.Background(), secrets.GenericSecret{
		ID:           secretID,
		ScopeTeamID:  secrets.LocalScopeTeam,
		Name:         "forge_token",
		SealedSecret: sealed,
	}); err != nil {
		t.Fatalf("store the secret: %v", err)
	}

	wf := &ir.Workflow{Name: "canary", Secrets: map[string]*ir.Secret{
		"forge_token": {As: "file", Optional: true},
	}}

	ctx, err := StampLocalCredentials(context.Background(), wf, memStore, sealer, nil)
	if err != nil {
		t.Fatalf("StampLocalCredentials: %v", err)
	}
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		t.Fatal("the returned context carries no credentials: the sandbox would mount nothing")
	}
	if got := creds.GenericSecret("forge_token"); got == "" {
		t.Fatal("forge_token resolved empty from a store that holds it — the file secret would be skipped as unresolved")
	}

	// No local store: the caller's context must come back untouched rather
	// than carrying an empty credential set that masks the cloud path's own.
	same, err := StampLocalCredentials(context.Background(), wf, nil, nil, nil)
	if err != nil {
		t.Fatalf("no-store stamp: %v", err)
	}
	if _, ok := secrets.CredentialsFromContext(same); ok {
		t.Fatal("stamped credentials with no local store configured")
	}
}
