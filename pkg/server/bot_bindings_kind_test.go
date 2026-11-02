package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/pluginsource"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

const bindingsBase = "/api/teams/t1/bots/doki/bindings"

// A context binding is stored, served, and materialised into every launch of
// its bot — on a deployment that HAS the materialiser's ingredients (the
// binding store and the pinned-source fetcher). A server without them keeps
// refusing the kind: a binding an operator can create, see listed, and watch
// do nothing is the defect #1908 reports, rebuilt by its own fix.
func TestBotBinding_CreatingTheContextKindIsRefusedUntilItIsMaterialised(t *testing.T) {
	s := newBindingTestServer(t)
	ctx := superAdminCtx()

	body := `{"kind":"context","secret_name_for_workflow":"handbook","context":{"git_url":"https://example.invalid/o/c.git","ref":"v1.0.0","path":"handbook.pdf"}}`
	w := httptest.NewRecorder()
	s.handleCreateBotBinding(w, bindReq(ctx, "POST", bindingsBase, body, "t1", "doki", ""))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("create of the context kind without a fetcher: code=%d body=%s, want 501", w.Code, w.Body.String())
	}
	// A 5xx body is redacted down to its correlation ids and its stable
	// error_code (httpx.WriteErrorJSON), so the code is what a client can act
	// on — asserting the prose would be asserting the shared writer's floor.
	if got := w.Body.String(); !strings.Contains(got, BindingContextNotMaterialisedCode) {
		t.Errorf("the refusal carries no actionable code: %q", got)
	}
	list, err := s.botBindings.ListByTenantBot(ctx, "t1", "doki")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("a refused create still stored a binding: %+v", list)
	}
}

// The same create on a server that CAN materialise (the fetcher is wired)
// is accepted: the refusal is a deployment's honest "I would do nothing
// with this", never a property of the kind.
func TestBotBinding_CreatingTheContextKindIsAcceptedWhenMaterialised(t *testing.T) {
	s := newBindingTestServer(t)
	s.pluginSourceFetcher = &pluginsource.Fetcher{}
	ctx := superAdminCtx()

	body := `{"kind":"context","secret_name_for_workflow":"handbook","context":{"git_url":"https://example.invalid/o/c.git","ref":"v1.0.0","path":"handbook.pdf"}}`
	w := httptest.NewRecorder()
	s.handleCreateBotBinding(w, bindReq(ctx, "POST", bindingsBase, body, "t1", "doki", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create of the context kind with a fetcher: code=%d body=%s, want 201", w.Code, w.Body.String())
	}
	var created secrets.BotSecretBinding
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.EffectiveKind() != secrets.BindingKindContext || created.Context == nil || created.Context.Path != "handbook.pdf" {
		t.Errorf("the stored binding lost its payload: %+v", created)
	}
	// And it lists with its health readout (empty — never materialised yet,
	// never failed either).
	w = httptest.NewRecorder()
	s.handleListBotBindings(w, bindReq(ctx, "GET", bindingsBase, "", "t1", "doki", ""))
	var lr struct {
		Bindings []secrets.BotSecretBinding `json:"bindings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &lr); err != nil {
		t.Fatal(err)
	}
	if len(lr.Bindings) != 1 || lr.Bindings[0].Degraded() {
		t.Errorf("list = %+v, want the one healthy context binding", lr.Bindings)
	}
}

// The name a bot can bind once is enforced by a unique index in the cloud and
// by the memory twin locally, and both now say so with one typed error. Before
// the translation the caller read a driver error and answered 500 — a conflict
// the client cannot act on.
func TestBotBinding_ASecondBindingOnTheSameNameIsAConflict(t *testing.T) {
	s := newBindingTestServer(t)
	ctx := superAdminCtx()
	if err := s.genericSecrets.Create(ctx, secrets.GenericSecret{ID: "sec1", ScopeTeamID: "t1", Name: "org_gitlab", SealedSecret: []byte("x"), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.genericSecrets.Create(ctx, secrets.GenericSecret{ID: "sec2", ScopeTeamID: "t1", Name: "org_other", SealedSecret: []byte("y"), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleCreateBotBinding(w, bindReq(ctx, "POST", bindingsBase, `{"secret_id":"sec1","secret_name_for_workflow":"GITLAB_TOKEN"}`, "t1", "doki", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("first create: code=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleCreateBotBinding(w, bindReq(ctx, "POST", bindingsBase, `{"secret_id":"sec2","secret_name_for_workflow":"GITLAB_TOKEN"}`, "t1", "doki", ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("second create on the same name: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
}

// Each kind's payload is checked in one place, so a route cannot accept a
// shape the resolver will not understand.
func TestBotBinding_APayloadThatContradictsItsKindIsRefused(t *testing.T) {
	s := newBindingTestServer(t)
	ctx := superAdminCtx()
	if err := s.genericSecrets.Create(ctx, secrets.GenericSecret{ID: "sec1", ScopeTeamID: "t1", Name: "org_gitlab", SealedSecret: []byte("x"), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"an unknown kind", `{"kind":"wishful","secret_name_for_workflow":"x","secret_id":"sec1"}`, "unknown bot binding kind"},
		{"a context kind with no source", `{"kind":"context","secret_name_for_workflow":"x"}`, "needs its pinned source"},
		{"a context kind exposing a secret", `{"kind":"context","secret_name_for_workflow":"x","secret_id":"sec1","context":{"git_url":"https://example.invalid/o/c.git","ref":"v1","path":"p.pdf"}}`, "exposes no secret to the workflow"},
		{"a context kind with no pin", `{"kind":"context","secret_name_for_workflow":"x","context":{"git_url":"https://example.invalid/o/c.git","path":"p.pdf"}}`, "context.git_url, context.ref and context.path"},
		{"a secret kind carrying a context", `{"secret_name_for_workflow":"x","secret_id":"sec1","context":{"git_url":"https://example.invalid/o/c.git","ref":"v1","path":"p.pdf"}}`, "carries no context payload"},
		{"no declared name", `{"secret_id":"sec1"}`, "needs the workflow name it resolves under"},
		// The read credential a context pins is dereferenced by the same
		// shared automation policy as a bound secret, so it answers to the
		// same tenancy predicate. Checked BEFORE the not-implemented refusal,
		// so the rule is enforced today rather than owed to the slice that
		// opens the kind.
		{"a context kind pinning an unbindable read credential", `{"kind":"context","secret_name_for_workflow":"x","context":{"git_url":"https://example.invalid/o/c.git","ref":"v1","path":"p.pdf","secret_id":"ghost"}}`, "context.secret_id is not a team-scoped secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleCreateBotBinding(w, bindReq(ctx, "POST", bindingsBase, tc.body, "t1", "doki", ""))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%s, want 400", w.Code, w.Body.String())
			}
			if got := w.Body.String(); !strings.Contains(got, tc.want) {
				t.Errorf("refusal = %q, want it to name %q", got, tc.want)
			}
		})
	}
}
