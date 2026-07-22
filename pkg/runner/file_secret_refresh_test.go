package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fakeRefresher records the values pushed to the sandbox so a test can
// assert the sandboxed refresh path propagates a rotation exactly once.
type fakeRefresher struct {
	got map[string][]string
}

func (f *fakeRefresher) RefreshSecretFile(_ context.Context, name string, value []byte) error {
	if f.got == nil {
		f.got = map[string][]string{}
	}
	f.got[name] = append(f.got[name], string(value))
	return nil
}

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

// TestRefreshSandboxFileSecretsOnce_PushesRotationToDriver pins the
// SANDBOXED mid-run refresh (#99 extended): a rotated store record is
// handed to the sandbox driver's SecretFileRefresher exactly once, and an
// unchanged record is not re-pushed.
func TestRefreshSandboxFileSecretsOnce_PushesRotationToDriver(t *testing.T) {
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

	r := &Runner{cfg: Config{
		Logger:         iterlog.New(iterlog.LevelError, os.Stderr),
		Sealer:         sealer,
		GenericSecrets: mem,
	}}
	refs := map[string]string{"forge_token": id}
	fake := &fakeRefresher{}
	last := map[string]string{}

	// First tick reads v1 and pushes it once.
	r.refreshSandboxFileSecretsOnce(context.Background(), "team-1", refs, fake, last)
	if got := fake.got["forge_token"]; len(got) != 1 || got[0] != "token-v1" {
		t.Fatalf("first push = %v, want [token-v1]", got)
	}
	// Unchanged store value → no re-push.
	r.refreshSandboxFileSecretsOnce(context.Background(), "team-1", refs, fake, last)
	if got := fake.got["forge_token"]; len(got) != 1 {
		t.Fatalf("unchanged value re-pushed: %v", got)
	}

	// Rotate the store record → the new value is pushed to the driver.
	sealed2, err := secrets.SealGenericSecret(sealer, id, []byte("token-v2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Update(tctx, secrets.GenericSecret{ID: id, TenantID: "team-1", ScopeTeamID: "team-1", Name: "forge_token", SealedSecret: sealed2}); err != nil {
		t.Fatal(err)
	}
	r.refreshSandboxFileSecretsOnce(context.Background(), "team-1", refs, fake, last)
	if got := fake.got["forge_token"]; len(got) != 2 || got[1] != "token-v2" {
		t.Fatalf("after rotation, pushes = %v, want [...token-v2]", got)
	}
}

// TestRefreshFileSecretsOnce_StoreErrorKeepsFile pins the failure contract:
// a store read failure is logged and retried next tick — the file keeps its
// last good value, nothing is truncated or removed.
func TestRefreshFileSecretsOnce_StoreErrorKeepsFile(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealerFromBase64("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{
		Logger:         iterlog.New(iterlog.LevelError, os.Stderr),
		Sealer:         sealer,
		GenericSecrets: secrets.NewMemoryGenericSecretStore(), // empty: every Get fails
	}}
	path := filepath.Join(t.TempDir(), "forge_token")
	if err := os.WriteFile(path, []byte("last-good"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.refreshFileSecretsOnce(context.Background(), "team-1",
		map[string]string{"forge_token": "unknown-id"},
		map[string]string{"forge_token": path},
		map[string]string{})
	if got, _ := os.ReadFile(path); string(got) != "last-good" {
		t.Fatalf("file mutated on store error: %q", got)
	}
}

// TestSandboxFileSecretRefs pins which secrets qualify for the sandboxed
// mid-run refresh: file secrets carrying a store ref in the run's
// credentials — env secrets and snapshot-only (ref-less) file secrets are
// excluded, and every absent prerequisite yields nil (observer no-op).
func TestSandboxFileSecretRefs(t *testing.T) {
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"forge_token":   {Name: "forge_token", As: "file"},
		"api_env":       {Name: "api_env", As: "env"},
		"snapshot_only": {Name: "snapshot_only", As: "file"},
	}}
	creds := secrets.Credentials{GenericRefs: map[string]string{
		"forge_token": "id-1",
		"api_env":     "id-2", // ref'd but env-mounted → excluded
	}}
	r := &Runner{cfg: Config{
		Logger:         iterlog.New(iterlog.LevelError, os.Stderr),
		GenericSecrets: secrets.NewMemoryGenericSecretStore(),
	}}
	ctx := secrets.WithCredentials(context.Background(), creds)

	refs := r.sandboxFileSecretRefs(ctx, wf)
	if len(refs) != 1 || refs["forge_token"] != "id-1" {
		t.Fatalf("refs = %v, want exactly {forge_token: id-1}", refs)
	}

	if got := r.sandboxFileSecretRefs(ctx, nil); got != nil {
		t.Errorf("nil workflow: refs = %v, want nil", got)
	}
	if got := r.sandboxFileSecretRefs(context.Background(), wf); got != nil {
		t.Errorf("no credentials in ctx: refs = %v, want nil", got)
	}
	noStore := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelError, os.Stderr)}}
	if got := noStore.sandboxFileSecretRefs(ctx, wf); got != nil {
		t.Errorf("no GenericSecrets store: refs = %v, want nil", got)
	}
	refless := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"snapshot_only": {Name: "snapshot_only", As: "file"},
	}}
	if got := r.sandboxFileSecretRefs(ctx, refless); got != nil {
		t.Errorf("ref-less file secret: refs = %v, want nil", got)
	}
}

// fakeSandboxRun is a sandbox.Run WITHOUT the SecretFileRefresher
// capability — the driver shape the observer must warn about instead of
// crashing or silently skipping.
type fakeSandboxRun struct{ sandbox.Run }

func (fakeSandboxRun) Driver() string { return "fake-driver" }

// TestSandboxRunObserver pins the observer construction: the live Run is
// always registered in the write-through registry (even with no
// refreshable refs); a driver without SecretFileRefresher plus
// refreshable refs → a visible warning naming the driver.
func TestSandboxRunObserver(t *testing.T) {
	var buf bytes.Buffer
	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelWarn, &buf)}}

	// No refreshable refs: still registers, no warning.
	obs := r.sandboxRunObserver(context.Background(), "run-1", "team-1", nil)
	obs(fakeSandboxRun{})
	if r.sandboxRunFor("run-1") == nil {
		t.Fatal("observer must register the live sandbox run")
	}
	if buf.Len() != 0 {
		t.Errorf("no warning expected without refreshable refs, got %q", buf.String())
	}
	r.unregisterSandboxRun("run-1")
	if r.sandboxRunFor("run-1") != nil {
		t.Fatal("unregister must drop the run from the registry")
	}

	obs = r.sandboxRunObserver(context.Background(), "run-2", "team-1", map[string]string{"forge_token": "id-1"})
	obs(fakeSandboxRun{})
	out := buf.String()
	if !strings.Contains(out, "does not support mid-run secret refresh") || !strings.Contains(out, "fake-driver") {
		t.Errorf("expected a driver-naming warning, got %q", out)
	}
}
