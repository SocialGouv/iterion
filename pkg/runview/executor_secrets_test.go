package runview

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// localStoreWith seals each entry into an in-memory local store.
func localStoreWith(t *testing.T, sealer secrets.Sealer, entries ...secrets.GenericSecret) secrets.GenericSecretStore {
	t.Helper()
	memStore := secrets.NewMemoryGenericSecretStore()
	for _, e := range entries {
		sealed, err := secrets.SealGenericSecret(sealer, e.ID, []byte("stored-"+e.Name))
		if err != nil {
			t.Fatalf("seal %s: %v", e.Name, err)
		}
		e.ScopeTeamID, e.SealedSecret = secrets.LocalScopeTeam, sealed
		if err := memStore.Create(context.Background(), e); err != nil {
			t.Fatalf("store %s: %v", e.Name, err)
		}
	}
	return memStore
}

// TestStampLocalCredentials_ResolvesOnlyValuelessDeclarations holds the local
// launch to the cloud publisher's rule: the store fills a declaration with no
// inline `value:` and only those. An inline-valued secret resolves from its
// own expression in the run, so a store entry of the same name must not ride
// the run's credentials: the cloud launch never resolves it, and on the cloud
// a subbot child runs on its parent's credentials — a child declaring that
// name without a value would read the store's token on one launch path and
// nothing on the other.
func TestStampLocalCredentials_ResolvesOnlyValuelessDeclarations(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	memStore := localStoreWith(t, sealer,
		secrets.GenericSecret{ID: "sec-forge-token", Name: "forge_token"},
		secrets.GenericSecret{ID: "sec-deploy", Name: "deploy_credential"})

	wf := &ir.Workflow{Name: "canary", Secrets: map[string]*ir.Secret{
		"forge_token":       {As: "file", Value: "{{vars.tok}}"},
		"deploy_credential": {As: "file", Optional: true},
	}}
	ctx, err := StampLocalCredentials(context.Background(), wf, memStore, sealer, nil)
	if err != nil {
		t.Fatalf("StampLocalCredentials: %v", err)
	}
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		t.Fatal("the returned context carries no credentials")
	}
	if got := creds.GenericSecret("forge_token"); got != "" {
		t.Errorf("forge_token is declared with an inline value and still resolved from the store (%q) — the cloud launch resolves nothing for it, so the run reads a different token on each launch path", got)
	}
	if got := creds.GenericSecret("deploy_credential"); got != "stored-deploy_credential" {
		t.Errorf("deploy_credential (no inline value) resolved to %q, want the stored value", got)
	}
}

// TestStampLocalCredentials_SaysWhatAnInlineValueShadows: a store entry an
// inline-valued declaration shadows is not read — its value, nor the host
// scope `iterion secret set --hosts` gave it, which the secret guard would
// otherwise have narrowed the value's egress to. The launch says so, naming
// the secret and the scope, instead of dropping the scope in silence; an
// entry no inline value shadows is not named.
func TestStampLocalCredentials_SaysWhatAnInlineValueShadows(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	memStore := localStoreWith(t, sealer,
		secrets.GenericSecret{ID: "sec-api", Name: "api_key", AllowedHosts: []string{"api.example.com"}},
		secrets.GenericSecret{ID: "sec-plain", Name: "plain_key"},
		secrets.GenericSecret{ID: "sec-deploy", Name: "deploy_credential", AllowedHosts: []string{"deploy.example.com"}})
	wf := &ir.Workflow{Name: "canary", Secrets: map[string]*ir.Secret{
		"api_key":           {As: "env", Value: "${API_KEY}"},
		"plain_key":         {As: "env", Value: "{{vars.k}}"},
		"deploy_credential": {As: "file", Optional: true},
	}}
	var buf bytes.Buffer
	if _, err := StampLocalCredentials(context.Background(), wf, memStore, sealer, iterlog.New(iterlog.LevelDebug, &buf)); err != nil {
		t.Fatalf("StampLocalCredentials: %v", err)
	}
	logged := buf.String()
	for _, want := range []string{`secret "api_key" is declared with an inline value`, "api.example.com", `secret "plain_key" is declared with an inline value`} {
		if !strings.Contains(logged, want) {
			t.Errorf("the launch log does not say %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, "deploy_credential") {
		t.Errorf("the launch names deploy_credential, which no inline value shadows:\n%s", logged)
	}
}
