package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// newAdminUsageReadingsServer boots a cloud-shaped server through the real
// New()/routes() path with a memory readings ledger, and returns the
// ledger, the audit store, the live server and a super-admin + plain
// member bearer.
func newAdminUsageReadingsServer(t *testing.T) (*usagecap.MemStore, audit.Store, *httptest.Server, string, string) {
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
	ledger := usagecap.NewMemStore()
	auditStore := audit.NewMemoryStore()
	s := New(Config{
		WorkDir:                 t.TempDir(),
		SkipProjectRegistration: true,
		AuthService:             svc,
		AuthSigner:              signer,
		Audit:                   auditStore,
		UsageCaps:               ledger,
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
	return ledger, auditStore, hs, adminTok, userTok
}

// #690 point 3 — the scalpel. An operator who KNOWS the provider reset a
// window early clears that one credential's readings: every key the
// credential was metered under is forgotten, other credentials keep
// theirs, the call is audited with the count, and a second call reports
// zero rather than failing.
func TestAdminUsageReadings_ClearForgetsOneCredential(t *testing.T) {
	ledger, auditStore, hs, adminTok, _ := newAdminUsageReadingsServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	stale := usagecap.Reading{Window: usagecap.WindowSevenDay, Utilization: 0.99, Status: usagecap.StatusWarning,
		ResetsAt: now.Add(72 * time.Hour), ObservedAt: now.Add(-17 * time.Hour)}
	platform := usagecap.Key("claude_code", usagecap.ScopePlatform, "e4ecd2283afb305f")
	tenant := usagecap.Key("claude_code", usagecap.TenantScope("team-1"), "e4ecd2283afb305f")
	other := usagecap.Key("claude_code", usagecap.TenantScope("team-1"), "0b5c74421234abcd")
	for _, k := range []string{platform, tenant, other} {
		if err := ledger.Record(ctx, k, stale); err != nil {
			t.Fatal(err)
		}
	}

	code, body := llmDo(t, hs, "DELETE", "/api/admin/usage-readings/e4ecd2283afb305f", adminTok, "")
	if code != 200 {
		t.Fatalf("DELETE: %d %s", code, body)
	}
	var view usageReadingsClearedView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if view.Fingerprint != "e4ecd2283afb305f" || view.Deleted != 2 {
		t.Fatalf("view = %+v, want fingerprint e4ecd2283afb305f and 2 readings dropped (platform + tenant meters)", view)
	}
	for _, k := range []string{platform, tenant} {
		if got, _ := ledger.Latest(ctx, k); len(got) != 0 {
			t.Fatalf("%s still holds %d reading(s) after the clear", k, len(got))
		}
	}
	if got, _ := ledger.Latest(ctx, other); len(got) != 1 {
		t.Fatalf("another credential's reading was dropped")
	}

	// Audited with the count, in the platform log. The write is detached
	// from the request, so poll briefly.
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, err := auditStore.ListPlatform(ctx, audit.Page{Limit: 10})
		if err != nil {
			t.Fatalf("audit list: %v", err)
		}
		if len(events) > 0 {
			e := events[0]
			if e.Action != "platform.usage_readings.cleared" || e.TargetID != "e4ecd2283afb305f" {
				t.Fatalf("audit event = %s/%s, want platform.usage_readings.cleared on the fingerprint", e.Action, e.TargetID)
			}
			if e.ActorID != "root" || e.ActorKind != "super_admin" {
				t.Fatalf("actor: %q/%q", e.ActorID, e.ActorKind)
			}
			if n, ok := e.Meta["deleted"].(int); !ok || n != 2 {
				t.Fatalf("audit meta deleted = %v, want 2", e.Meta["deleted"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("audit entry never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Idempotent: nothing left to forget is a zero, not an error.
	code, body = llmDo(t, hs, "DELETE", "/api/admin/usage-readings/e4ecd2283afb305f", adminTok, "")
	if code != 200 {
		t.Fatalf("second DELETE: %d %s", code, body)
	}
	if err := json.Unmarshal(body, &view); err != nil || view.Deleted != 0 {
		t.Fatalf("second DELETE view = %+v (%v), want 0 deleted", view, err)
	}
}

// Auth: a plain (even team-admin) member is rejected; so is anonymous —
// and neither touches the ledger.
func TestAdminUsageReadings_NonAdminRejected(t *testing.T) {
	ledger, _, hs, _, userTok := newAdminUsageReadingsServer(t)
	ctx := context.Background()
	key := usagecap.Key("claude_code", usagecap.ScopePlatform, "e4ecd2283afb305f")
	if err := ledger.Record(ctx, key, usagecap.Reading{Window: usagecap.WindowSevenDay, Utilization: 0.99, ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tok  string
		want int
	}{{userTok, 403}, {"", 401}} {
		code, _ := llmDo(t, hs, "DELETE", "/api/admin/usage-readings/e4ecd2283afb305f", tc.tok, "")
		if code != tc.want {
			t.Errorf("token=%v: want %d, got %d", tc.tok != "", tc.want, code)
		}
	}
	if got, _ := ledger.Latest(ctx, key); len(got) != 1 {
		t.Fatalf("a rejected caller cleared the ledger")
	}
}
