package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2696 / F18. Mutate the handler to omit its resolver: the legacy GET fails.
// Mutate the projection to read the store ahead of attempts: frozen history
// changes after the edit, then returns 404 after deletion. A cached legacy
// graph must not mask a deleted row or fall through to a baked namesake.
func TestRunWorkflowStoredLegacyAndImmutableAttempt(t *testing.T) {
	s := newOrgTestServer(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.runs = newTestRunviewService(t, "", runview.WithStore(st))
	s.botSources = botsource.NewMemoryStore()
	ctx := store.WithTenant(context.Background(), "team-a")
	row, err := s.botSources.Create(ctx, botsource.BotSource{TenantID: "team-a", Slug: "team-bot", Files: map[string]string{"main.bot": testBotMain}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy", "frozen"} {
		r, err := st.CreateRun(ctx, id, "main", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.BotID, r.BotSourceTenant, r.BotSourceTier = "team-bot", "team-a", store.BotSourceTierTeam
		r.FilePath = "/server-has-no-team-bot/main.bot"
		r.WorkflowHash = "captured-hash"
		if err := st.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	_, err = st.RecordAttempt(ctx, "frozen", &store.AttemptSnapshot{StartedAt: time.Now().UTC(), WorkflowHash: "captured-hash", Source: &workflowfile.Snapshot{Main: "main.bot", Files: map[string]string{"main.bot": testBotMain}}})
	if err != nil {
		t.Fatal(err)
	}
	get := func(id string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/runs/"+id+"/workflow", nil).WithContext(ctx)
		req.SetPathValue("id", id)
		w := httptest.NewRecorder()
		s.handleGetRunWorkflow(w, req)
		return w
	}
	assertEntry := func(id, entry, provenance string) {
		t.Helper()
		w := get(id)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", id, w.Code, w.Body.String())
		}
		var wf runview.WireWorkflow
		if err := json.Unmarshal(w.Body.Bytes(), &wf); err != nil {
			t.Fatal(err)
		}
		if wf.Entry != entry || wf.Provenance != provenance {
			t.Fatalf("GET %s = %+v", id, wf)
		}
	}
	assertEntry("legacy", "work", "bot_source_legacy")
	assertEntry("frozen", "work", "attempt_snapshot")
	row.Files["main.bot"] = strings.NewReplacer("tool work:", "tool edited:", "entry: work", "entry: edited", "work ->", "edited ->").Replace(testBotMain)
	if _, err := s.botSources.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	assertEntry("legacy", "edited", "bot_source_legacy")
	assertEntry("frozen", "work", "attempt_snapshot")
	if err := s.botSources.Delete(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	assertEntry("frozen", "work", "attempt_snapshot")
	w := get("legacy")
	if w.Code != http.StatusNotFound {
		t.Fatalf("deleted source = %d %s", w.Code, w.Body.String())
	}
	var refusal map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal["error_code"] != "bot_source_missing" || refusal["source"] != "team-bot" || strings.Contains(w.Body.String(), "/server-has-no") {
		t.Fatalf("untyped or path-based 404: %s", w.Body.String())
	}
}
