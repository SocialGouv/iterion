package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The routing block rides the platform-credentials record's PUT: an object
// REPLACES the stored block, null clears it back to the env dials, and a
// block the fold cannot read is refused with the field named.
func TestAdminPlatformCredentials_RoutingReplacesAndClears(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.PlatformCredentials]()
	s := New(Config{SkipProjectRegistration: true, PlatformCredentialsSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/platform-credentials", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutPlatformCredentials(w, r)
		return w
	}

	park := llmroute.RefusedPinnedPark
	if w := put(`{"routing":{"pair_order":["claw+anthropic_key"],"refused_pinned_key":"park","strict":true}}`); w.Code != http.StatusOK {
		t.Fatalf("routing set = %d: %s", w.Code, w.Body.String())
	}
	rec, _ := st.Get(context.Background())
	if rec == nil || rec.Routing == nil || len(rec.Routing.PairOrder) != 1 || rec.Routing.RefusedPinnedKey != park || rec.Routing.Strict == nil || !*rec.Routing.Strict {
		t.Fatalf("stored = %+v, want the whole block replaced", rec)
	}

	// A second write REPLACES the block, never merges: the pair order that
	// named claw is gone.
	if w := put(`{"routing":{"triggers":["usage_window"]}}`); w.Code != http.StatusOK {
		t.Fatalf("routing replace = %d: %s", w.Code, w.Body.String())
	}
	rec, _ = st.Get(context.Background())
	if rec == nil || rec.Routing == nil || rec.Routing.PairOrder != nil || len(rec.Routing.Triggers) != 1 {
		t.Fatalf("stored = %+v, want a wholesale replacement (no claw pair left)", rec)
	}

	// null clears back to the env dials.
	if w := put(`{"routing":null}`); w.Code != http.StatusOK {
		t.Fatalf("routing clear = %d: %s", w.Code, w.Body.String())
	}
	if rec, _ := st.Get(context.Background()); rec != nil && rec.Routing != nil {
		t.Fatalf("stored = %+v, want the block cleared", rec)
	}

	// A block the fold cannot read is refused with the field named.
	if w := put(`{"routing":{"pair_order":["claude_code+not_a_slot"]}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid routing = %d: %s", w.Code, w.Body.String())
	}
	if rec, _ := st.Get(context.Background()); rec != nil && rec.Routing != nil {
		t.Fatalf("a refused write must not land: %+v", rec.Routing)
	}
	if w := put(`{"routing":{"triggers":["budget"]}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("budget trigger = %d: %s", w.Code, w.Body.String())
	}
	if w := put(`{"routing":{"unknown_field":1}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d: %s", w.Code, w.Body.String())
	}
}

// The effective routing answers with provenance even when the record says
// nothing: the env dials then the built-in defaults — the operator sees
// what the NEXT launch will resolve, not just what is stored.
func TestAdminPlatformCredentials_RoutingEffectiveDefaults(t *testing.T) {
	t.Setenv(llmroute.EnvRefusedPinnedKey, llmroute.RefusedPinnedPark)
	st := platformcfg.NewMemoryStore[platformcfg.PlatformCredentials]()
	s := New(Config{SkipProjectRegistration: true, PlatformCredentialsSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})
	r := httptest.NewRequest("GET", "/api/admin/settings/platform-credentials", nil).WithContext(admin)
	w := httptest.NewRecorder()
	s.handleAdminGetPlatformCredentials(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", w.Code, w.Body.String())
	}
	var view struct {
		RoutingEffective *store.RunLLMRoutePolicy `json:"routing_effective"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.RoutingEffective == nil {
		t.Fatal("routing_effective is nil — the operator cannot see what the next launch resolves")
	}
	if view.RoutingEffective.RefusedPinnedKey != llmroute.RefusedPinnedPark {
		t.Fatalf("effective refused_pinned_key = %q, want the env dial's park", view.RoutingEffective.RefusedPinnedKey)
	}
	if got := view.RoutingEffective.Sources["refused_pinned_key"]; got != llmroute.SourceEnv {
		t.Fatalf("refused_pinned_key provenance = %q, want env", got)
	}
	if got := view.RoutingEffective.Sources["pair_order"]; got != llmroute.SourceDefault {
		t.Fatalf("pair_order provenance = %q, want default", got)
	}
}
