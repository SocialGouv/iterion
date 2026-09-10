package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #659 pt 3 — the key view answers "is this key in use right now?" with
// the SAME count the launch walk's ceiling asks: alive runs stamped with
// the key's fingerprint that are executing a model node. Absent, not zero,
// for a row with no fingerprint.
func TestListApiKeys_ReportsAliveRuns(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	runs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{apiKeys: keys, sealer: sealer, logger: iterlog.Nop(), cfg: Config{Store: runs}}
	ctx := store.WithTenant(context.Background(), "team-a")
	mkKey := func(id, fp string, ceiling int) {
		t.Helper()
		if err := keys.Create(ctx, secrets.ApiKey{
			ID: id, TenantID: "team-a", ScopeTeamID: "team-a", Provider: secrets.ProviderZAI,
			Name: id, Fingerprint: fp, MaxConcurrentRuns: ceiling, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mkKey("facade", "fp-facade", 2)
	mkKey("legacy", "", 0)

	mkRun := func(id string, status store.RunStatus, idle bool, fps ...string) {
		t.Helper()
		if _, err := runs.CreateRun(ctx, id, "demo", nil); err != nil {
			t.Fatal(err)
		}
		r, err := runs.LoadRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		r.Status = status
		if err := runs.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := runs.SetRunCredStamp(ctx, id, store.RunCredStamp{Fingerprints: fps}); err != nil {
			t.Fatal(err)
		}
		if idle {
			now := time.Now().UTC()
			if err := runs.SetRunLLMIdle(ctx, id, &now); err != nil {
				t.Fatal(err)
			}
		}
	}
	mkRun("spending", store.RunStatusRunning, false, "fp-facade")
	mkRun("queued", store.RunStatusQueued, false, "fp-facade")
	mkRun("in-verify-gate", store.RunStatusRunning, true, "fp-facade")
	mkRun("parked", store.RunStatusFailedResumable, false, "fp-facade")

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
	facade, ok := byID["facade"]
	if !ok || facade.AliveRuns == nil {
		t.Fatalf("facade key view = %+v, want alive_runs present", facade)
	}
	if *facade.AliveRuns != 2 || facade.MaxConcurrentRuns != 2 {
		t.Fatalf("facade alive_runs = %d / %d, want 2 / 2 (spending + queued; the idle and parked runs hold no slot)", *facade.AliveRuns, facade.MaxConcurrentRuns)
	}
	if legacy := byID["legacy"]; legacy.AliveRuns != nil {
		t.Fatalf("a fingerprint-less row reported alive_runs=%d — there is nothing to count with", *legacy.AliveRuns)
	}
}
