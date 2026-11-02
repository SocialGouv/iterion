package connection

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The credential round-trip the #2760 contract must not break: a payload
// sealed under the preserved `forge_conn:<id>` AAD opens, and one addressed
// to another id does not — the AAD is what makes a transplant visible.
func TestForgeCredential_SealOpenRoundTrip(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	pat, err := SealForgePAT(sealer, "c1", "glpat-xyz")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := OpenForgeCredential(sealer, "c1", pat)
	if err != nil {
		t.Fatalf("open PAT: %v", err)
	}
	if cred.PATToken != "glpat-xyz" || cred.AdminToken() != "glpat-xyz" {
		t.Fatalf("PAT round-trip: %+v", cred)
	}

	pair, err := SealForgeOAuthTokens(sealer, "c2", "access", "refresh", exp)
	if err != nil {
		t.Fatal(err)
	}
	cred, err = OpenForgeCredential(sealer, "c2", pair)
	if err != nil {
		t.Fatalf("open OAuth pair: %v", err)
	}
	if cred.AccessToken != "access" || cred.RefreshToken != "refresh" || !cred.ExpiresAt.Equal(exp) {
		t.Fatalf("OAuth round-trip: %+v", cred)
	}
	if cred.AdminToken() != "access" {
		t.Fatalf("the admin token of an OAuth credential is its access token, got %q", cred.AdminToken())
	}

	// A blob sealed for c2 does not open as c1's: the id is authenticated.
	if _, err := OpenForgeCredential(sealer, "c1", pair); err == nil {
		t.Fatal("a payload transplanted to another connection id opened")
	}

	if _, err := ForgeAdminToken(sealer, ForgeConnection{ID: "c2", SealedPayload: pair}); err != nil {
		t.Fatalf("ForgeAdminToken: %v", err)
	}
}

// A nil sealer is a wiring mistake, not "no encryption": every path refuses.
func TestForgeCredential_NilSealerRefuses(t *testing.T) {
	if _, err := SealForgePAT(nil, "c1", "x"); err == nil {
		t.Fatal("seal with a nil sealer succeeded")
	}
	if _, err := OpenForgeCredential(nil, "c1", []byte("x")); err == nil {
		t.Fatal("open with a nil sealer succeeded")
	}
}
