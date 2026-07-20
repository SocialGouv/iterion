package forge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestOAuthAppStore_CRUD(t *testing.T) {
	st := NewMemoryOAuthAppStore()
	ctx := context.Background()
	app := ForgeOAuthApp{
		ID: "app-1", TenantID: "t1", Provider: ProviderGitLab,
		ForgeBaseURL: "https://gitlab.example.com", ClientID: "cid", SealedSecret: []byte("sealed"),
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := st.Create(ctx, app); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.Get(ctx, "app-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ClientID != "cid" {
		t.Fatalf("client id = %q", got.ClientID)
	}

	// GetByInstance canonicalises the query (trailing slash, no scheme).
	bi, err := st.GetByInstance(ctx, "t1", ProviderGitLab, "gitlab.example.com/")
	if err != nil {
		t.Fatalf("getByInstance: %v", err)
	}
	if bi.ID != "app-1" {
		t.Fatalf("getByInstance id = %q", bi.ID)
	}

	apps, err := st.ListByTenant(ctx, "t1")
	if err != nil || len(apps) != 1 {
		t.Fatalf("list: %v len=%d", err, len(apps))
	}

	if err := st.Delete(ctx, "app-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, "app-1"); !errors.Is(err, ErrOAuthAppNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
}

func TestOAuthAppStore_DuplicateInstance(t *testing.T) {
	st := NewMemoryOAuthAppStore()
	ctx := context.Background()
	a := ForgeOAuthApp{ID: "a", TenantID: "t1", Provider: ProviderGitLab, ForgeBaseURL: "https://gitlab.com", ClientID: "x"}
	if err := st.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Empty base URL canonicalises to the SaaS default → same instance as `a`.
	b := ForgeOAuthApp{ID: "b", TenantID: "t1", Provider: ProviderGitLab, ForgeBaseURL: "", ClientID: "y"}
	if err := st.Create(ctx, b); !errors.Is(err, ErrOAuthAppExists) {
		t.Fatalf("expected ErrOAuthAppExists, got %v", err)
	}
	// Different tenant on the same instance is fine.
	c := ForgeOAuthApp{ID: "c", TenantID: "t2", Provider: ProviderGitLab, ForgeBaseURL: "https://gitlab.com", ClientID: "z"}
	if err := st.Create(ctx, c); err != nil {
		t.Fatalf("different tenant create: %v", err)
	}
}

// A tenant legitimately holds ONE GitHub App PER OWNING ORG on the same host:
// a private App can only be installed on the account that owns it, so an org
// with a prod org and a sandbox org needs two. Keying uniqueness on the host
// alone made the second one impossible — that is the constraint this relaxes.
func TestOAuthAppStore_OneAppPerOwnerOnSameHost(t *testing.T) {
	st := NewMemoryOAuthAppStore()
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()

	prod := ForgeOAuthApp{ID: "prod", TenantID: "t1", Provider: ProviderGitHub,
		ForgeBaseURL: "https://github.com", OwnerLogin: "SocialGouv", ClientID: "c1", CreatedAt: base}
	sandbox := ForgeOAuthApp{ID: "sandbox", TenantID: "t1", Provider: ProviderGitHub,
		ForgeBaseURL: "https://github.com", OwnerLogin: "iterion-sandbox", ClientID: "c2", CreatedAt: base.Add(time.Hour)}

	if err := st.Create(ctx, prod); err != nil {
		t.Fatalf("create prod app: %v", err)
	}
	if err := st.Create(ctx, sandbox); err != nil {
		t.Fatalf("a second app on the same host but a DIFFERENT org must be allowed: %v", err)
	}

	// Same owner twice is still a duplicate — the constraint moved, it did not vanish.
	dup := ForgeOAuthApp{ID: "dup", TenantID: "t1", Provider: ProviderGitHub,
		ForgeBaseURL: "https://github.com", OwnerLogin: "SocialGouv", ClientID: "c3"}
	if err := st.Create(ctx, dup); !errors.Is(err, ErrOAuthAppExists) {
		t.Fatalf("same owner twice must conflict, got %v", err)
	}

	list, err := st.ListByInstance(ctx, "t1", ProviderGitHub, "https://github.com")
	if err != nil {
		t.Fatalf("list by instance: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 apps on the instance, got %d", len(list))
	}

	// GetByInstance must stay DETERMINISTIC (oldest first) — it is the legacy
	// answer for connections created before they recorded which app they use.
	// An unordered "any match" would hand callers a different private key run
	// to run, which cannot mint for the installation it is used against.
	for i := 0; i < 5; i++ {
		got, err := st.GetByInstance(ctx, "t1", ProviderGitHub, "https://github.com")
		if err != nil {
			t.Fatalf("get by instance: %v", err)
		}
		if got.ID != "prod" {
			t.Fatalf("GetByInstance must return the oldest app deterministically, got %q", got.ID)
		}
	}
}

func TestOAuthAppSealer_RoundTrip(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealOAuthAppSecret(sealer, "app-1", "s3cr3t")
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenOAuthAppSecret(sealer, "app-1", sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cr3t" {
		t.Fatalf("got %q", got)
	}
	// AAD binds the blob to the app id — opening under another id must fail.
	if _, err := OpenOAuthAppSecret(sealer, "app-2", sealed); err == nil {
		t.Fatal("expected AAD mismatch error opening under a different app id")
	}
}

func TestForgeAppPrivateKeySealer_RoundTrip(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	const pem = "-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n-----END RSA PRIVATE KEY-----"
	sealed, err := SealForgeAppPrivateKey(sealer, "app-1", pem)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenForgeAppPrivateKey(sealer, "app-1", sealed)
	if err != nil || got != pem {
		t.Fatalf("round-trip = %q, %v", got, err)
	}
	// AAD binds to the app id…
	if _, err := OpenForgeAppPrivateKey(sealer, "app-2", sealed); err == nil {
		t.Fatal("expected AAD mismatch under a different app id")
	}
	// …and is distinct from the client_secret envelope: a key blob must NOT open
	// as a client_secret (different AAD), so the two can't be transplanted.
	if _, err := OpenOAuthAppSecret(sealer, "app-1", sealed); err == nil {
		t.Fatal("private-key blob must not open as a client_secret (distinct AAD)")
	}
}
