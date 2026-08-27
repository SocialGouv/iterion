package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// Platform runtime settings routes: the super-admin surface that makes the
// usage-cap percentages a DB record instead of a frozen env var.

// newAdminSettingsServer boots a cloud-shaped server through the real
// New()/routes() path (production auth middleware included) with a memory
// settings store, and returns the store, the audit store, the live server
// and a super-admin + plain-member bearer.
func newAdminSettingsServer(t *testing.T) (*usagecap.MemorySettingsStore, audit.Store, *httptest.Server, string, string) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := auth.NewService(auth.Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	settings := usagecap.NewMemorySettingsStore()
	auditStore := audit.NewMemoryStore()
	s := New(Config{
		WorkDir:                 t.TempDir(),
		SkipProjectRegistration: true,
		AuthService:             svc,
		AuthSigner:              signer,
		Audit:                   auditStore,
		UsageCapSettings:        settings,
	}, iterlog.New(iterlog.LevelError, nil))

	adminTok, _, err := signer.IssueAccess(auth.Identity{UserID: "root", IsSuperAdmin: true, TeamID: "team-root"})
	if err != nil {
		t.Fatalf("issue admin token: %v", err)
	}
	userTok, _, err := signer.IssueAccess(auth.Identity{UserID: "u1", TeamID: "team-1", Role: identity.RoleAdmin})
	if err != nil {
		t.Fatalf("issue user token: %v", err)
	}
	hs := httptest.NewServer(s.handler)
	t.Cleanup(hs.Close)
	return settings, auditStore, hs, adminTok, userTok
}

const usageCapsPath = "/api/admin/settings/usage-caps"

func decodeCapsView(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var view map[string]any
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode view: %v (%s)", err, body)
	}
	return view
}

// Env-only fallback: no record → the env percentages are the effective
// ones and the source reads env.
func TestAdminUsageCaps_EnvFallback(t *testing.T) {
	t.Setenv(usagecap.EnvFiveHour, "85")
	t.Setenv(usagecap.EnvWeek, "75")
	_, _, hs, adminTok, _ := newAdminSettingsServer(t)

	code, body := llmDo(t, hs, "GET", usageCapsPath, adminTok, "")
	if code != 200 {
		t.Fatalf("GET: %d %s", code, body)
	}
	view := decodeCapsView(t, body)
	if view["record"] != nil {
		t.Fatalf("no record written, got %v", view["record"])
	}
	eff := view["effective"].(map[string]any)
	if eff["five_hour_pct"].(float64) != 85 || eff["week_pct"].(float64) != 75 {
		t.Fatalf("effective must be env values, got %v", eff)
	}
	if view["source"] != "env" {
		t.Fatalf("source must read env, got %v", view["source"])
	}
	if view["propagation_bound_seconds"].(float64) <= 0 {
		t.Fatalf("propagation bound must be advertised, got %v", view["propagation_bound_seconds"])
	}
}

// A DB update overrides env, is answered with the fresh effective view,
// audits old/new/caller, and reaches /healthz without any restart.
func TestAdminUsageCaps_UpdateOverridesEnvAndAudits(t *testing.T) {
	t.Setenv(usagecap.EnvFiveHour, "85")
	t.Setenv(usagecap.EnvWeek, "75")
	settings, auditStore, hs, adminTok, _ := newAdminSettingsServer(t)

	code, body := llmDo(t, hs, "PUT", usageCapsPath, adminTok, `{"five_hour_pct": 40}`)
	if code != 200 {
		t.Fatalf("PUT: %d %s", code, body)
	}
	view := decodeCapsView(t, body)
	eff := view["effective"].(map[string]any)
	if eff["five_hour_pct"].(float64) != 40 {
		t.Fatalf("effective five-hour must be the DB value, got %v", eff)
	}
	if eff["week_pct"].(float64) != 75 {
		t.Fatalf("untouched week must stay env, got %v", eff)
	}
	if view["source"] != "db+env" {
		t.Fatalf("source must read db+env, got %v", view["source"])
	}

	// The record landed with the caller stamped.
	rec, err := settings.GetSettings(context.Background())
	if err != nil || rec == nil || rec.FiveHourPct == nil || *rec.FiveHourPct != 40 {
		t.Fatalf("stored record wrong: %+v err=%v", rec, err)
	}
	if rec.UpdatedBy != "root" {
		t.Fatalf("UpdatedBy must name the caller, got %q", rec.UpdatedBy)
	}

	// /healthz echoes the EFFECTIVE value — the operator's no-DB-access
	// verification surface. (The audit write is detached; poll both.)
	deadline := time.Now().Add(5 * time.Second)
	var health map[string]any
	for {
		_, hbody := llmDo(t, hs, "GET", "/healthz", "", "")
		health = decodeCapsView(t, hbody)
		if strings.Contains(health["usage_cap"].(string), "5h=40%") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthz never showed the runtime value: %v", health)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if health["usage_cap_source"] != "db+env" {
		t.Fatalf("healthz source must read db+env, got %v", health["usage_cap_source"])
	}

	// Audit entry: action, actor, old and new values.
	for {
		events, err := auditStore.ListPlatform(context.Background(), audit.Page{Limit: 10})
		if err != nil {
			t.Fatalf("audit list: %v", err)
		}
		if len(events) > 0 {
			e := events[0]
			if e.Action != "platform.settings.usage_caps.updated" {
				t.Fatalf("action: %q", e.Action)
			}
			if e.ActorID != "root" || e.ActorKind != "super_admin" {
				t.Fatalf("actor: %q/%q", e.ActorID, e.ActorKind)
			}
			if e.Meta["old_five_hour_pct"] != nil {
				t.Fatalf("old value must be nil (no prior record), got %v", e.Meta["old_five_hour_pct"])
			}
			if got, ok := e.Meta["new_five_hour_pct"].(int); !ok || got != 40 {
				t.Fatalf("new value must be 40, got %v", e.Meta["new_five_hour_pct"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit entry never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Explicit null clears the override back to env; merge semantics keep
	// what the body does not name.
	code, body = llmDo(t, hs, "PUT", usageCapsPath, adminTok, `{"week_pct": 60}`)
	if code != 200 {
		t.Fatalf("PUT week: %d %s", code, body)
	}
	code, body = llmDo(t, hs, "PUT", usageCapsPath, adminTok, `{"five_hour_pct": null}`)
	if code != 200 {
		t.Fatalf("PUT clear: %d %s", code, body)
	}
	view = decodeCapsView(t, body)
	eff = view["effective"].(map[string]any)
	if eff["five_hour_pct"].(float64) != 85 {
		t.Fatalf("cleared five-hour must fall back to env 85, got %v", eff)
	}
	if eff["week_pct"].(float64) != 60 {
		t.Fatalf("merge must keep the week override, got %v", eff)
	}
}

// Auth: a plain (even team-admin) member is rejected; so is anonymous.
func TestAdminUsageCaps_NonAdminRejected(t *testing.T) {
	settings, _, hs, _, userTok := newAdminSettingsServer(t)

	for _, tc := range []struct {
		method, tok string
		want        int
	}{
		{"GET", userTok, 403},
		{"PUT", userTok, 403},
		{"GET", "", 401},
		{"PUT", "", 401},
	} {
		code, _ := llmDo(t, hs, tc.method, usageCapsPath, tc.tok, `{"five_hour_pct": 10}`)
		if code != tc.want {
			t.Errorf("%s with token=%v: want %d, got %d", tc.method, tc.tok != "", tc.want, code)
		}
	}
	if rec, _ := settings.GetSettings(context.Background()); rec != nil {
		t.Fatalf("rejected callers must not write, got %+v", rec)
	}
}

// Invalid input is rejected loudly, with the reason, and writes nothing.
func TestAdminUsageCaps_InvalidValuesRejected(t *testing.T) {
	settings, _, hs, adminTok, _ := newAdminSettingsServer(t)

	for _, tc := range []struct{ body, wantIn string }{
		{`{"five_hour_pct": 101}`, "0–100"},
		{`{"week_pct": -1}`, "0–100"},
		{`{"five_hour_pct": 80.5}`, "not an integer"},
		{`{"five_hour_pct": "eighty"}`, "not an integer"},
		{`{"5h": 80}`, "unknown field"},
		{`{}`, "empty update"},
		{`nonsense`, "invalid JSON"},
	} {
		code, body := llmDo(t, hs, "PUT", usageCapsPath, adminTok, tc.body)
		if code != 400 {
			t.Errorf("PUT %s: want 400, got %d (%s)", tc.body, code, body)
			continue
		}
		if !strings.Contains(string(body), tc.wantIn) {
			t.Errorf("PUT %s: rejection must state the reason (%q), got %s", tc.body, tc.wantIn, body)
		}
	}
	if rec, _ := settings.GetSettings(context.Background()); rec != nil {
		t.Fatalf("invalid updates must not write, got %+v", rec)
	}
}
