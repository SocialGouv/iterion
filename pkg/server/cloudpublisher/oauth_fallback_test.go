package cloudpublisher

import (
	"context"
	"io"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

func testLogger() *iterlog.Logger { return iterlog.New(iterlog.LevelError, io.Discard) }

// seedOAuth seals a credentials.json under ownerKey/claude_code.
func seedOAuth(t *testing.T, st secrets.OAuthStore, sealer secrets.Sealer, ownerKey, token string) {
	t.Helper()
	blob := []byte(`{"claudeAiOauth":{"accessToken":"` + token + `"}}`)
	sealed, err := secrets.SealOAuthPayload(sealer, ownerKey, secrets.OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := st.Upsert(context.Background(), secrets.OAuthRecord{
		UserID:        ownerKey,
		Kind:          secrets.OAuthKindClaudeCode,
		SealedPayload: sealed,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

func resolveBundle(t *testing.T, p *Publisher, runSecrets *secrets.MemoryRunSecretsStore, sealer secrets.Sealer, runID, tenant, owner string) secrets.RunBundle {
	t.Helper()
	ctx := store.WithTenant(context.Background(), tenant)
	creds, err := p.resolveAndSealCredentials(ctx, runID, "", tenant, owner, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.secretsRef == "" {
		return secrets.RunBundle{}
	}
	rec, err := runSecrets.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	bundle, err := secrets.OpenRunBundle(sealer, runID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return bundle
}

func TestResolveOAuth_UserPrimaryOrgFallback(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "alice", "sk-ant-personal")
	seedOAuth(t, oauth, sealer, secrets.OrgOwnerKey("team1"), "sk-ant-org")

	p := &Publisher{
		oauthForfait: oauth,
		runSecrets:   secrets.NewMemoryRunSecretsStore(),
		sealer:       sealer,
		logger:       testLogger(),
	}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	// 1. Interactive run owned by alice → her personal forfait wins.
	b := resolveBundle(t, p, rs, sealer, "run-user", "team1", "alice")
	if got := string(b.OAuthCredentials["claude_code"]); got == "" || !contains(got, "sk-ant-personal") {
		t.Fatalf("expected personal forfait, got %q", got)
	}

	// 2. Automated run with a synthetic owner (no personal forfait) →
	//    org fallback.
	b = resolveBundle(t, p, rs, sealer, "run-webhook", "team1", "webhook:cfg-1")
	if got := string(b.OAuthCredentials["claude_code"]); got == "" || !contains(got, "sk-ant-org") {
		t.Fatalf("expected org fallback forfait, got %q", got)
	}

	// 3. Owner without personal forfait AND no org credential → none.
	p2 := &Publisher{oauthForfait: secrets.NewMemoryOAuthStore(), runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rs2 := p2.runSecrets.(*secrets.MemoryRunSecretsStore)
	b = resolveBundle(t, p2, rs2, sealer, "run-none", "team1", "webhook:cfg-1")
	if len(b.OAuthCredentials) != 0 {
		t.Fatalf("expected no oauth creds, got %+v", b.OAuthCredentials)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
