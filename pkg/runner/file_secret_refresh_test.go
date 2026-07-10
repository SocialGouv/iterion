package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tenantAssertingGenericStore fails any Get whose ctx carries no tenant —
// the same discipline the Mongo store enforces in production.
type tenantAssertingGenericStore struct {
	*secrets.MemoryGenericSecretStore
	t *testing.T
}

func (s *tenantAssertingGenericStore) Get(ctx context.Context, id string) (secrets.GenericSecret, error) {
	if tid, ok := store.TenantFromContext(ctx); !ok || tid == "" {
		s.t.Fatalf("Get without tenant on ctx")
	}
	return s.MemoryGenericSecretStore.Get(ctx, id)
}

// TestRefreshFileSecretsOnce_RewritesRotatedValue pins the mid-run file
// secret refresh: when the store record rotates (the server refresh worker
// re-minted a 1h App installation token), the materialised file is rewritten
// so `cat` at use time reads the live credential; an unchanged record leaves
// the file untouched.
func TestRefreshFileSecretsOnce_RewritesRotatedValue(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealerFromBase64("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	mem := &tenantAssertingGenericStore{MemoryGenericSecretStore: secrets.NewMemoryGenericSecretStore(), t: t}

	id := secrets.NewGenericSecretID()
	sealed, err := secrets.SealGenericSecret(sealer, id, []byte("token-v1"))
	if err != nil {
		t.Fatal(err)
	}
	tctx := store.WithTenant(context.Background(), "team-1")
	if err := mem.Create(tctx, secrets.GenericSecret{ID: id, TenantID: "team-1", ScopeTeamID: "team-1", Name: "forge_token", SealedSecret: sealed}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "forge_token")
	if err := os.WriteFile(path, []byte("token-v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &Runner{cfg: Config{
		Logger:         iterlog.New(iterlog.LevelError, os.Stderr),
		Sealer:         sealer,
		GenericSecrets: mem,
	}}
	refs := map[string]string{"forge_token": id}
	files := map[string]string{"forge_token": path}
	last := map[string]string{"forge_token": "token-v1"}

	// Unchanged store value → file untouched.
	r.refreshFileSecretsOnce(context.Background(), "team-1", refs, files, last)
	if got, _ := os.ReadFile(path); string(got) != "token-v1" {
		t.Fatalf("file changed without rotation: %q", got)
	}

	// Rotate the store record (what the server refresh worker does).
	sealed2, err := secrets.SealGenericSecret(sealer, id, []byte("token-v2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Update(tctx, secrets.GenericSecret{ID: id, TenantID: "team-1", ScopeTeamID: "team-1", Name: "forge_token", SealedSecret: sealed2}); err != nil {
		t.Fatal(err)
	}

	r.refreshFileSecretsOnce(context.Background(), "team-1", refs, files, last)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "token-v2" {
		t.Fatalf("file not refreshed: got %q want token-v2", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("refreshed file perm = %v, want 0600", fi.Mode().Perm())
	}
	// A secret with no ref is never touched.
	orphan := filepath.Join(dir, "other")
	if err := os.WriteFile(orphan, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.refreshFileSecretsOnce(context.Background(), "team-1", map[string]string{}, map[string]string{"other": orphan}, map[string]string{})
	if got, _ := os.ReadFile(orphan); string(got) != "keep" {
		t.Fatalf("ref-less secret rewritten: %q", got)
	}
}
