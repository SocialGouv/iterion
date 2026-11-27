package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	redeliveryTenant     = "team-1"
	redeliveryRunID      = "run-redelivered"
	redeliveryExpiredTok = "ghs_expired_installation_token"
)

// redeliveryFixture is the state a redelivered repo-targeted run finds: the
// run's sealed bundle still holds the forge token minted at launch, now
// expired, while the server-side refresh worker has rewritten the token's
// generic-secret record with a live one (cloneCredTestToken).
type redeliveryFixture struct {
	sealer secrets.Sealer
	run    *secrets.MemoryRunSecretsStore
	gen    *secrets.MemoryGenericSecretStore
	msg    *queue.RunMessage
}

func newRedeliveryFixture(t *testing.T, recordValue string) *redeliveryFixture {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealerFromBase64("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	f := &redeliveryFixture{sealer: sealer, run: secrets.NewMemoryRunSecretsStore(), gen: secrets.NewMemoryGenericSecretStore()}
	tctx := store.WithTenant(context.Background(), redeliveryTenant)

	id := secrets.NewGenericSecretID()
	sealedRec, err := secrets.SealGenericSecret(sealer, id, []byte(recordValue))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.gen.Create(tctx, secrets.GenericSecret{ID: id, TenantID: redeliveryTenant, ScopeTeamID: redeliveryTenant, Name: "forge_token", SealedSecret: sealedRec}); err != nil {
		t.Fatal(err)
	}

	// The bundle seals under a per-run DEK carried in the message (ADR-123),
	// as every bundle the publisher writes today does.
	dek, err := secrets.NewRunBundleDEK()
	if err != nil {
		t.Fatal(err)
	}
	dekSealer, err := secrets.NewAESGCMSealer(dek)
	if err != nil {
		t.Fatal(err)
	}
	sealed, keyID, err := secrets.SealRunBundle(dekSealer, redeliveryTenant, "", redeliveryRunID, secrets.RunBundle{
		GenericSecrets:    map[string]string{"forge_token": redeliveryExpiredTok},
		GenericSecretRefs: map[string]string{"forge_token": id},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.run.Put(tctx, secrets.RunSecretsRecord{
		ID: "ref-1", TenantID: redeliveryTenant, RunID: redeliveryRunID, KeyID: keyID, SealedBundle: sealed,
		CreatedAt: time.Now().Add(-90 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	f.msg = &queue.RunMessage{RunID: redeliveryRunID, TenantID: redeliveryTenant, SecretsRef: "ref-1", BundleDEK: dek}
	return f
}

// A redelivered run opens the bundle sealed at launch. The clone credential
// it hands git must be the store record's current value, not that
// snapshot: the snapshot's installation token has expired by the time the
// redelivery claims the run.
func TestARedeliveryReadsTheCloneCredentialFromItsStoreRecordNotTheLaunchSnapshot(t *testing.T) {
	f := newRedeliveryFixture(t, cloneCredTestToken)
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: f.run, Sealer: f.sealer, GenericSecrets: f.gen}}

	ctx, cleanup, err := r.injectCredentials(context.Background(), f.msg)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("injectCredentials: %v", err)
	}
	name, tok := cloneCredentialToken(ctx)
	if name != "forge_token" || tok != cloneCredTestToken {
		t.Fatalf("clone credential = %s=%q, want forge_token=%q (the store record's live value, not the launch snapshot %q)", name, tok, cloneCredTestToken, redeliveryExpiredTok)
	}
}

// A store record that cannot be read leaves the snapshot in place: the run
// still carries its launch credential, which may be valid, and the clone's
// own refusal says so when it is not.
func TestAnUnreadableStoreRecordKeepsTheLaunchSnapshot(t *testing.T) {
	f := newRedeliveryFixture(t, cloneCredTestToken)
	empty := secrets.NewMemoryGenericSecretStore() // the ref names no record here
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: f.run, Sealer: f.sealer, GenericSecrets: empty}}

	ctx, cleanup, err := r.injectCredentials(context.Background(), f.msg)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("injectCredentials: %v", err)
	}
	if _, tok := cloneCredentialToken(ctx); tok != redeliveryExpiredTok {
		t.Fatalf("clone credential = %q, want the launch snapshot %q when the record cannot be read", tok, redeliveryExpiredTok)
	}
}

// The whole redelivery, against a private https forge that accepts only the
// live token: the bundle's snapshot is expired, the store record is live,
// and the workspace preparation clones.
func TestARedeliveredRunClonesWithTheRefreshedForgeToken(t *testing.T) {
	forge := newLFSForge(t, false, false)
	f := newRedeliveryFixture(t, cloneCredTestToken)
	r, _ := forgeRunner(t)
	r.cfg.RunSecrets, r.cfg.Sealer, r.cfg.GenericSecrets = f.run, f.sealer, f.gen

	ctx, cleanup, err := r.injectCredentials(context.Background(), f.msg)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("injectCredentials: %v", err)
	}
	msg := &queue.RunMessage{RunID: redeliveryRunID, TenantID: redeliveryTenant, RepoURL: forge.url, RepoSHA: "feat"}
	if _, _, err := r.prepareRepoWorkspace(ctx, cloneCredWorkflow(), msg); err != nil {
		log, _ := forge.logs()
		t.Fatalf("prepareRepoWorkspace on a redelivery whose store record is live: %v\nthe forge saw:\n%s", err, strings.Join(log, "\n"))
	}
}

// A forge that answers 401 to the presented token fails the clone with a
// typed error naming the refused secret — never only git's "could not read
// Username", which names a prompt instead of the credential.
func TestACloneRefusedWith401IsTypedAsARefusedForgeCredential(t *testing.T) {
	forge := newLFSForge(t, false, false)
	r, _ := forgeRunner(t)
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		Generic: map[string]string{"forge_token": redeliveryExpiredTok},
	})
	msg := &queue.RunMessage{RunID: "run-401", RepoURL: forge.url, RepoSHA: "feat"}
	_, _, err := r.prepareRepoWorkspace(ctx, cloneCredWorkflow(), msg)
	if err == nil {
		t.Fatal("prepareRepoWorkspace succeeded with a token the forge refuses")
	}
	if !errors.Is(err, ErrForgeCredentialRefused) {
		t.Fatalf("err = %v, want ErrForgeCredentialRefused", err)
	}
	if !strings.Contains(err.Error(), `secret "forge_token"`) {
		t.Fatalf("err = %v, want it to name the refused secret forge_token", err)
	}
	if strings.Contains(err.Error(), redeliveryExpiredTok) {
		t.Fatalf("err leaks the refused token: %v", err)
	}
}

// The classification types only authentication refusals, and only when a
// credential was presented.
func TestForgeCredentialRefusedClassifiesOnlyAuthRefusals(t *testing.T) {
	prompt := errors.New("git clone: exit status 128: fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	for _, tc := range []struct {
		name     string
		err      error
		credName string
		want     bool
	}{
		{"prompt after 401", prompt, "forge_token", true},
		{"gitlab basic refusal", errors.New("remote: HTTP Basic: Access denied"), "gitlab_token", true},
		{"authentication failed", errors.New("fatal: Authentication failed for 'https://x/'"), "forge_token", true},
		{"no credential presented", prompt, "", false},
		{"not found", errors.New("fatal: repository 'https://x/' not found"), "forge_token", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := forgeCredentialRefused(tc.err, tc.credName)
			if errors.Is(got, ErrForgeCredentialRefused) != tc.want {
				t.Fatalf("forgeCredentialRefused(%v, %q) = %v, typed=%v want %v", tc.err, tc.credName, got, !tc.want, tc.want)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("the git error is not kept in the chain: %v", got)
			}
		})
	}
}
