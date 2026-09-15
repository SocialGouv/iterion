package forge

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRefreshWritesAndClearsTokenProofWithThePlaintext(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := secrets.NewMemoryGenericSecretStore()
	ctx := store.WithTenant(t.Context(), "team")
	if err := s.Create(ctx, secrets.GenericSecret{ID: "managed", TenantID: "team"}); err != nil {
		t.Fatal(err)
	}
	w := &RefreshWorker{Secrets: s, Sealer: sealer}
	now := time.Now()
	proof := secrets.NewTokenPermissionProof("first", map[string]string{"workflows": "write"}, now.Add(time.Hour))
	for _, out := range []RefreshedToken{{AccessToken: "first", TokenProof: proof}, {AccessToken: "second"}} {
		if err := w.rewriteManagedSecret(ctx, "managed", out); err != nil {
			t.Fatal(err)
		}
		rec, err := s.Get(ctx, "managed")
		if err != nil {
			t.Fatal(err)
		}
		plaintext, err := secrets.OpenGenericSecret(sealer, rec.ID, rec.SealedSecret)
		if err != nil || string(plaintext) != out.AccessToken {
			t.Fatalf("token rewrite: %v", err)
		}
		if rec.ForgeTokenProof.Allows(string(plaintext), "workflows", now) != (out.TokenProof != nil) {
			t.Fatal("token and proof drifted")
		}
		if out.TokenProof == nil && rec.ForgeTokenProof != nil {
			t.Fatal("stale proof retained")
		}
	}
}
