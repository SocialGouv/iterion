package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// #629 pt 4 — the operator-facing half. A key the provider is refusing is
// invisible on the keys view: it shows a name, a fingerprint, maybe a
// last_used_at, and nothing that says "every run on this key is being
// turned away right now". The launch walk knows (it skips it, or honours a
// pin over it); the human reading the view did not.
func TestListApiKeys_ReportsAFreshRefusal(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	caps := usagecap.NewMemStore()
	srv := &Server{
		apiKeys: keys, sealer: sealer, logger: iterlog.Nop(),
		usageCaps: caps, usageCapTrust: usagecap.DefaultTrust(),
	}
	ctx := store.WithTenant(context.Background(), "team-a")
	mkKey := func(id, fp string, prov secrets.Provider) {
		t.Helper()
		if err := keys.Create(ctx, secrets.ApiKey{
			ID: id, TenantID: "team-a", ScopeTeamID: "team-a", Provider: prov,
			Name: id, Fingerprint: fp, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mkKey("dead", "fp-dead", secrets.ProviderAnthropic)
	mkKey("healthy", "fp-healthy", secrets.ProviderAnthropic)
	mkKey("legacy", "", secrets.ProviderAnthropic)

	if err := caps.Record(ctx, usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team-a"), "fp-dead"),
		usagecap.Reading{Window: usagecap.WindowAuth, Status: usagecap.StatusRejected, ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/teams/team-a/api-keys", nil)
	r.SetPathValue("id", "team-a")
	r = r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	w := httptest.NewRecorder()
	srv.handleListTeamApiKeys(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Keys []apiKeyView `json:"keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	byID := map[string]apiKeyView{}
	for _, k := range body.Keys {
		byID[k.ID] = k
	}
	dead := byID["dead"]
	if dead.RefusedUntil == nil {
		t.Fatalf("dead key view = %+v, want refused_until present", dead)
	}
	if !strings.Contains(strings.ToLower(dead.RefusedReason), "credential") {
		t.Fatalf("refused_reason = %q, want it to name the auth rejection", dead.RefusedReason)
	}
	if byID["healthy"].RefusedUntil != nil {
		t.Fatalf("a key with no refusal reported refused_until=%v", *byID["healthy"].RefusedUntil)
	}
	if byID["legacy"].RefusedUntil != nil {
		t.Fatal("a fingerprint-less row names a slot, not a credential — it can carry no refusal")
	}
}
