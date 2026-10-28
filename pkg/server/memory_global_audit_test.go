package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// awaitPlatformAudit waits for a platform-scoped row with the given action.
// The insert is detached (goSafe), so the wait is on the CONDITION, never on
// a tick.
func awaitPlatformAudit(t *testing.T, st audit.Store, action string) audit.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, err := st.ListPlatform(context.Background(), audit.Page{})
		if err != nil {
			t.Fatalf("list platform audit: %v", err)
		}
		for _, e := range evs {
			if e.Action == action {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q platform audit row (got %d rows) — docs/memory-and-knowledge.md promises one on every global mutation", action, len(evs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMemoryRoutes_GlobalMutationsLeaveAnAuditRow closes the half the docs
// admitted was missing: "neither leaves an audit row — no memory mutation
// records one, so a super-admin overwrite of the shared catalogue does not
// show up". Platform scope, because the act is instance-wide.
func TestMemoryRoutes_GlobalMutationsLeaveAnAuditRow(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	s := newOrgTestServer(t)
	st := audit.NewMemoryStore()
	s.auditStore = st
	const gl = "visibility=global&name=shared"
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "admin", TeamID: "t1", IsSuperAdmin: true, Credential: auth.CredentialDevMode})

	w := httptest.NewRecorder()
	s.handleMemoryWriteDoc(w, httptest.NewRequest("PUT", "/api/memory/doc?"+gl+"&path=a.md", strings.NewReader("# A")).WithContext(admin))
	if w.Code != http.StatusOK {
		t.Fatalf("super-admin global write: %d %s", w.Code, w.Body.String())
	}
	ev := awaitPlatformAudit(t, st, "memory.global_write")
	if ev.ActorID != "admin" {
		t.Errorf("audit actor = %q, want the acting super-admin", ev.ActorID)
	}
	if ev.Target != "memory_space" || !strings.Contains(ev.TargetID, "shared") {
		t.Errorf("audit target = %q/%q, want the space it mutated", ev.Target, ev.TargetID)
	}
	if ev.Meta["path"] != "a.md" {
		t.Errorf("audit meta = %v, want the document path", ev.Meta)
	}

	w = httptest.NewRecorder()
	s.handleMemoryDeleteDoc(w, httptest.NewRequest("DELETE", "/api/memory/doc?"+gl+"&path=a.md", nil).WithContext(admin))
	if w.Code != http.StatusNoContent {
		t.Fatalf("super-admin global delete: %d %s", w.Code, w.Body.String())
	}
	awaitPlatformAudit(t, st, "memory.global_delete")

	// A team-scoped write is an ordinary member act: no platform row.
	member := auth.WithIdentity(context.Background(), auth.Identity{UserID: "m", TeamID: "t1", Credential: auth.CredentialDevMode})
	w = httptest.NewRecorder()
	s.handleMemoryWriteDoc(w, httptest.NewRequest("PUT", "/api/memory/doc?visibility=org&name=team&path=a.md", strings.NewReader("# A")).WithContext(member))
	if w.Code != http.StatusOK {
		t.Fatalf("member org write: %d %s", w.Code, w.Body.String())
	}
	evs, err := st.ListPlatform(context.Background(), audit.Page{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, e := range evs {
		if strings.HasPrefix(e.Action, "memory.global_") && e.Meta["visibility"] != string(knowledge.VisibilityGlobal) {
			t.Errorf("a team-scoped mutation produced a global audit row: %+v", e)
		}
	}
}

// TestMemoryRoutes_StoreRefusalIsA403NotA422: the store is the gate of
// record, so a handler that somehow reaches it without the grant must answer
// 403 — not the 422 a client reads as "bad document" nor the 500 it reads as
// "retry later". Driven through the PUBLIC handler with a request the gate
// never blessed, which is the shape a future handler forgetting the grant
// would produce.
func TestMemoryRoutes_StoreRefusalIsA403NotA422(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	s := New(Config{SkipProjectRegistration: true}, iterlog.New(iterlog.LevelError, nil))
	ref := knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "shared"}
	_, err := s.memoryStore().WriteDocument(context.Background(), ref, knowledge.DocumentInput{Path: "a.md", Content: []byte("x")})
	if err == nil {
		t.Fatal("premise lost: the store accepted an ungranted global write, so the status mapping below proves nothing")
	}
	if got := memoryMutationStatus(err, http.StatusUnprocessableEntity); got != http.StatusForbidden {
		t.Errorf("status for a store authority refusal = %d, want 403", got)
	}
	if got := memoryMutationStatus(err, http.StatusInternalServerError); got != http.StatusForbidden {
		t.Errorf("status for a store authority refusal (delete default) = %d, want 403", got)
	}
	// Every other store error keeps the handler's own default.
	_, other := s.memoryStore().WriteDocument(knowledge.WithPlatformWriteAuthority(context.Background()), ref, knowledge.DocumentInput{Path: "../escape.md", Content: []byte("x")})
	if other == nil {
		t.Fatal("premise lost: a traversal path was accepted")
	}
	if got := memoryMutationStatus(other, http.StatusUnprocessableEntity); got != http.StatusUnprocessableEntity {
		t.Errorf("a non-authority store error must keep the handler's default, got %d", got)
	}
}
