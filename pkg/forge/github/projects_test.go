package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The fixtures below are RECORDED shapes: they were captured from the real
// GitHub GraphQL API against the project this epic targets
// (github.com/orgs/SocialGouv/projects/203), then trimmed. Keeping the real
// shape — including the `__typename` discriminators and the field values that
// carry no `field` (ProjectV2ItemFieldRepositoryValue) — is what makes these
// tests able to catch a decoding assumption that only holds on paper.

const fixtureProjectFields = `{
  "data": {
    "organization": {
      "projectV2": {
        "id": "PVT_kwDOAh0HH84BiOg8",
        "title": "Iterion",
        "number": 203,
        "url": "https://github.com/orgs/SocialGouv/projects/203",
        "fields": {
          "nodes": [
            {"__typename":"ProjectV2Field","id":"PVTF_title","name":"Title","dataType":"TITLE"},
            {"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_status","name":"Status","dataType":"SINGLE_SELECT",
             "options":[{"id":"fb92b7a2","name":"Inbox"},{"id":"6b7641c9","name":"Planned"},{"id":"d360bd91","name":"In progress"},{"id":"6b20abeb","name":"Blocked"},{"id":"27139072","name":"Done"}]},
            {"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_area","name":"Area","dataType":"SINGLE_SELECT",
             "options":[{"id":"8377b935","name":"engine"},{"id":"568d6b97","name":"cloud/ops"}]},
            {"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_mode","name":"Mode","dataType":"SINGLE_SELECT",
             "options":[{"id":"c9116deb","name":"dogfood"},{"id":"15864718","name":"direct"}]},
            {"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_prio","name":"Priority","dataType":"SINGLE_SELECT",
             "options":[{"id":"ebacc6b0","name":"P0"},{"id":"f9253403","name":"P1"},{"id":"b4c36e57","name":"P2"}]}
          ]
        }
      }
    }
  }
}`

const fixtureItemsPage1 = `{
  "data": {
    "organization": {
      "projectV2": {
        "items": {
          "pageInfo": {"hasNextPage": true, "endCursor": "CURSOR1"},
          "nodes": [
            {
              "id": "PVTI_one",
              "updatedAt": "2026-09-04T06:54:54Z",
              "isArchived": false,
              "type": "ISSUE",
              "content": {"__typename":"Issue","number":573,"title":"the override lane is dead","url":"https://github.com/SocialGouv/iterion/issues/573","state":"CLOSED","repository":{"nameWithOwner":"SocialGouv/iterion"}},
              "fieldValues": {"nodes": [
                {"__typename":"ProjectV2ItemFieldRepositoryValue"},
                {"__typename":"ProjectV2ItemFieldTextValue","text":"the override lane is dead","field":{"id":"PVTF_title","name":"Title"}},
                {"__typename":"ProjectV2ItemFieldSingleSelectValue","name":"Done","optionId":"27139072","updatedAt":"2026-09-04T06:54:54Z","field":{"id":"PVTSSF_status","name":"Status"}},
                {"__typename":"ProjectV2ItemFieldSingleSelectValue","name":"P2","optionId":"b4c36e57","updatedAt":"2026-09-02T12:35:08Z","field":{"id":"PVTSSF_prio","name":"Priority"}}
              ]}
            }
          ]
        }
      }
    }
  }
}`

const fixtureItemsPage2 = `{
  "data": {
    "organization": {
      "projectV2": {
        "items": {
          "pageInfo": {"hasNextPage": false, "endCursor": "CURSOR2"},
          "nodes": [
            {
              "id": "PVTI_two",
              "updatedAt": "2026-09-02T12:35:09Z",
              "isArchived": true,
              "type": "ISSUE",
              "content": {"__typename":"Issue","number":562,"title":"Diagnostic for duplicate fan-out edges","url":"https://github.com/SocialGouv/iterion/issues/562","state":"OPEN","repository":{"nameWithOwner":"SocialGouv/iterion"}},
              "fieldValues": {"nodes": [
                {"__typename":"ProjectV2ItemFieldSingleSelectValue","name":"Planned","optionId":"6b7641c9","updatedAt":"2026-09-02T12:17:44Z","field":{"id":"PVTSSF_status","name":"Status"}},
                {"__typename":"ProjectV2ItemFieldSingleSelectValue","name":"cloud/ops","optionId":"568d6b97","updatedAt":"2026-09-02T12:17:44Z","field":{"id":"PVTSSF_area","name":"Area"}}
              ]}
            },
            {
              "id": "PVTI_draft",
              "updatedAt": "2026-09-01T09:00:00Z",
              "isArchived": false,
              "type": "DRAFT_ISSUE",
              "content": {"__typename":"DraftIssue","title":"a thought"},
              "fieldValues": {"nodes": []}
            }
          ]
        }
      }
    }
  }
}`

// fixtureNotFound is the real envelope GitHub returns for a missing project:
// HTTP 200, a populated `data` with a null leaf, AND an errors[] entry. Reading
// only `data` here yields a zero-value Project and no error at all.
const fixtureNotFound = `{
  "data": {"organization": {"projectV2": null}},
  "errors": [{"type":"NOT_FOUND","path":["organization","projectV2"],"message":"Could not resolve to a ProjectV2 with the number 99999."}]
}`

// gqlFake is a fake GitHub GraphQL endpoint. It routes on distinctive
// substrings of the incoming query/mutation and records every request body so
// a test can assert on the variables actually sent.
type gqlFake struct {
	t        *testing.T
	srv      *httptest.Server
	requests []gqlRequest
	// respond, when set, fully overrides routing.
	respond func(q string, vars map[string]any) (int, string)
	// itemPages is consumed in order by successive items queries.
	itemPages []string
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
	AuthZ     string         `json:"-"`
	Path      string         `json:"-"`
}

func newGQLFake(t *testing.T) *gqlFake {
	t.Helper()
	f := &gqlFake{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req gqlRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("fake graphql: bad request body %q: %v", raw, err)
		}
		req.AuthZ = r.Header.Get("Authorization")
		req.Path = r.URL.Path
		f.requests = append(f.requests, req)

		code, body := f.route(req.Query, req.Variables)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *gqlFake) route(q string, vars map[string]any) (int, string) {
	if f.respond != nil {
		return f.respond(q, vars)
	}
	switch {
	case strings.Contains(q, "addProjectV2ItemById"):
		return 200, `{"data":{"addProjectV2ItemById":{"item":{"id":"PVTI_added","updatedAt":"2026-09-05T10:00:00Z","isArchived":false,"type":"ISSUE","content":{"__typename":"Issue","number":613,"title":"epic","url":"https://github.com/SocialGouv/iterion/issues/613","state":"OPEN","repository":{"nameWithOwner":"SocialGouv/iterion"}},"fieldValues":{"nodes":[]}}}}}`
	case strings.Contains(q, "updateProjectV2ItemFieldValue"):
		return 200, `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_one"}}}}`
	case strings.Contains(q, "issue(number:"):
		return 200, `{"data":{"repository":{"issue":{"id":"I_kwDOissue613"}}}}`
	case strings.Contains(q, "items("):
		if len(f.itemPages) > 0 {
			page := f.itemPages[0]
			f.itemPages = f.itemPages[1:]
			return 200, page
		}
		return 200, fixtureItemsPage2
	case strings.Contains(q, "fields("):
		return 200, fixtureProjectFields
	}
	f.t.Errorf("fake graphql: unrouted query: %s", q)
	return 500, `{"errors":[{"message":"unrouted"}]}`
}

func (f *gqlFake) client() *AdminClient {
	// APIBase points at the fake; GraphQLURL derives /graphql from it.
	return &AdminClient{HTTP: f.srv.Client(), APIBase: f.srv.URL, Token: "tok"}
}

func (f *gqlFake) lastRequest() gqlRequest {
	f.t.Helper()
	if len(f.requests) == 0 {
		f.t.Fatal("no graphql request recorded")
	}
	return f.requests[len(f.requests)-1]
}

func mustProjectRef(t *testing.T, s string) forge.ProjectRef {
	t.Helper()
	ref, err := forge.ParseProjectRef(s)
	if err != nil {
		t.Fatalf("ParseProjectRef(%q): %v", s, err)
	}
	return ref
}

// ---------------------------------------------------------------------------
// the GraphQL transport
// ---------------------------------------------------------------------------

// TestGraphQLSurfacesErrorsArray pins the rule the repo's explicit-errors
// discipline demands and that GitHub makes easy to get wrong: a GraphQL
// response is HTTP 200 with a populated `data` even when it failed. Decoding
// only `data` yields a zero value and a nil error — a silent fallback.
func TestGraphQLSurfacesErrorsArray(t *testing.T) {
	f := newGQLFake(t)
	f.respond = func(string, map[string]any) (int, string) { return 200, fixtureNotFound }

	var out struct {
		Organization struct {
			ProjectV2 *struct {
				ID string `json:"id"`
			} `json:"projectV2"`
		} `json:"organization"`
	}
	err := f.client().GraphQL(context.Background(), "query { organization { projectV2 { id } } }", nil, &out)
	if err == nil {
		t.Fatal("want an error for a response carrying errors[], got nil")
	}
	if !strings.Contains(err.Error(), "Could not resolve to a ProjectV2") {
		t.Errorf("error must quote GitHub's own message, got %q", err)
	}
	if !strings.Contains(err.Error(), "NOT_FOUND") {
		t.Errorf("error must carry the GraphQL error type, got %q", err)
	}
}

func TestGraphQLSendsAuthAndPath(t *testing.T) {
	f := newGQLFake(t)
	f.respond = func(string, map[string]any) (int, string) { return 200, `{"data":{"ok":true}}` }

	var out map[string]any
	if err := f.client().GraphQL(context.Background(), "query { ok }", map[string]any{"n": 3}, &out); err != nil {
		t.Fatalf("GraphQL: %v", err)
	}
	req := f.lastRequest()
	if req.Path != "/graphql" {
		t.Errorf("path = %q, want /graphql", req.Path)
	}
	if req.AuthZ != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", req.AuthZ)
	}
	if got := req.Variables["n"]; got != float64(3) {
		t.Errorf("variables not forwarded: %#v", req.Variables)
	}
}

func TestGraphQLMapsHTTPStatus(t *testing.T) {
	f := newGQLFake(t)
	f.respond = func(string, map[string]any) (int, string) { return 401, `{"message":"Bad credentials"}` }

	var out map[string]any
	err := f.client().GraphQL(context.Background(), "query { ok }", nil, &out)
	if !errors.Is(err, forge.ErrUnauthorized) {
		t.Fatalf("want forge.ErrUnauthorized, got %v", err)
	}
}

func TestGraphQLURLForEnterprise(t *testing.T) {
	for _, tc := range []struct{ web, want string }{
		{"https://github.com", "https://api.github.com/graphql"},
		{"", "https://api.github.com/graphql"},
		{"https://ghe.example.com", "https://ghe.example.com/api/graphql"},
	} {
		if got := GraphQLURLFor(tc.web); got != tc.want {
			t.Errorf("GraphQLURLFor(%q) = %q, want %q", tc.web, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// GetProject — discovery by NAME
// ---------------------------------------------------------------------------

func TestGetProjectDiscoversFieldsByName(t *testing.T) {
	f := newGQLFake(t)
	p, err := f.client().GetProject(context.Background(), mustProjectRef(t, "SocialGouv/203"))
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.ID != "PVT_kwDOAh0HH84BiOg8" || p.Number != 203 || p.Title != "Iterion" {
		t.Fatalf("project identity wrong: %+v", p)
	}
	status, ok := p.Field("Status")
	if !ok {
		t.Fatal("Status field not discovered")
	}
	if status.ID != "PVTSSF_status" || !status.SingleSelect() {
		t.Fatalf("Status field wrong: %+v", status)
	}
	// Every mapped native state must resolve to an option id, by name.
	for _, m := range forge.DefaultStatusMapping() {
		opt, ok := status.Option(m.Status)
		if !ok {
			t.Errorf("status option %q (state %q) not discovered", m.Status, m.State)
			continue
		}
		if opt.ID == "" {
			t.Errorf("status option %q has no id", m.Status)
		}
	}
	// Case-insensitive, trimmed lookup: a board renamed "in progress" binds.
	if _, ok := status.Option(" in PROGRESS "); !ok {
		t.Error("option lookup must be case-insensitive and trimmed")
	}
	if _, ok := p.Field("area"); !ok {
		t.Error("field lookup must be case-insensitive")
	}
}

func TestGetProjectNotFoundIsAnError(t *testing.T) {
	f := newGQLFake(t)
	f.respond = func(string, map[string]any) (int, string) { return 200, fixtureNotFound }

	_, err := f.client().GetProject(context.Background(), mustProjectRef(t, "SocialGouv/99999"))
	if err == nil {
		t.Fatal("a null projectV2 with errors[] must be an error, not an empty Project")
	}
	if !errors.Is(err, forge.ErrProjectNotFound) {
		t.Errorf("want forge.ErrProjectNotFound, got %v", err)
	}
}

func TestGetProjectUsesOwnerKindEntryPoint(t *testing.T) {
	f := newGQLFake(t)
	ref := mustProjectRef(t, "someone/7")
	ref.OwnerKind = forge.ProjectOwnerUser
	f.respond = func(q string, _ map[string]any) (int, string) {
		if !strings.Contains(q, "user(login:") {
			t.Errorf("a user-owned project must query user(login:), got %s", q)
		}
		return 200, strings.Replace(fixtureProjectFields, `"organization"`, `"user"`, 1)
	}
	if _, err := f.client().GetProject(context.Background(), ref); err != nil {
		t.Fatalf("GetProject(user): %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListProjectItems — pagination + normalization
// ---------------------------------------------------------------------------

func TestListProjectItemsNormalizesAndPaginates(t *testing.T) {
	f := newGQLFake(t)
	f.itemPages = []string{fixtureItemsPage1, fixtureItemsPage2}
	c := f.client()
	ref := mustProjectRef(t, "SocialGouv/203")

	page1, err := c.ListProjectItems(context.Background(), ref, forge.ProjectItemListOptions{PerPage: 50})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if !page1.HasNext || page1.NextCursor != "CURSOR1" {
		t.Fatalf("page 1 pagination wrong: hasNext=%v cursor=%q", page1.HasNext, page1.NextCursor)
	}
	if len(page1.Items) != 1 {
		t.Fatalf("page 1 items = %d, want 1", len(page1.Items))
	}
	it := page1.Items[0]
	if it.ID != "PVTI_one" {
		t.Errorf("item id = %q", it.ID)
	}
	if it.Content.Kind != forge.ProjectContentIssue || it.Content.Repo != "SocialGouv/iterion" || it.Content.Number != 573 {
		t.Errorf("content wrong: %+v", it.Content)
	}
	if it.Content.State != "closed" {
		t.Errorf("issue state must be normalized to lowercase open/closed, got %q", it.Content.State)
	}
	st, ok := it.Field("Status")
	if !ok {
		t.Fatal("Status value not decoded")
	}
	if st.Value != "Done" || st.OptionID != "27139072" || st.FieldID != "PVTSSF_status" {
		t.Errorf("status value wrong: %+v", st)
	}
	want := time.Date(2026, 9, 4, 6, 54, 54, 0, time.UTC)
	if !st.UpdatedAt.Equal(want) {
		t.Errorf("status UpdatedAt = %v, want %v — the conflict rule needs the FIELD timestamp", st.UpdatedAt, want)
	}

	// The cursor must be forwarded on the next call.
	page2, err := c.ListProjectItems(context.Background(), ref, forge.ProjectItemListOptions{PerPage: 50, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if got := f.lastRequest().Variables["after"]; got != "CURSOR1" {
		t.Errorf("cursor not forwarded: after=%#v", got)
	}
	if page2.HasNext {
		t.Error("page 2 must be the last page")
	}
	if len(page2.Items) != 2 {
		t.Fatalf("page 2 items = %d, want 2 (issue + draft)", len(page2.Items))
	}
	if !page2.Items[0].Archived {
		t.Error("archived flag must be reported, not silently filtered")
	}
	if page2.Items[1].Content.Kind != forge.ProjectContentDraft {
		t.Errorf("draft item kind = %q, want %q", page2.Items[1].Content.Kind, forge.ProjectContentDraft)
	}
	if page2.Items[1].Content.Repo != "" || page2.Items[1].Content.Number != 0 {
		t.Errorf("a draft has no repo/number: %+v", page2.Items[1].Content)
	}
}

// ---------------------------------------------------------------------------
// writes
// ---------------------------------------------------------------------------

func TestSetSingleSelectSendsTheMutation(t *testing.T) {
	f := newGQLFake(t)
	err := f.client().SetSingleSelect(context.Background(), "PVT_p", "PVTI_one", "PVTSSF_status", "d360bd91")
	if err != nil {
		t.Fatalf("SetSingleSelect: %v", err)
	}
	req := f.lastRequest()
	if !strings.Contains(req.Query, "updateProjectV2ItemFieldValue") {
		t.Fatalf("wrong operation: %s", req.Query)
	}
	for k, want := range map[string]string{
		"projectId": "PVT_p", "itemId": "PVTI_one", "fieldId": "PVTSSF_status", "optionId": "d360bd91",
	} {
		if got, _ := req.Variables[k].(string); got != want {
			t.Errorf("variable %s = %q, want %q", k, got, want)
		}
	}
}

func TestSetSingleSelectSurfacesGraphQLError(t *testing.T) {
	f := newGQLFake(t)
	f.respond = func(string, map[string]any) (int, string) {
		return 200, `{"data":{"updateProjectV2ItemFieldValue":null},"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`
	}
	err := f.client().SetSingleSelect(context.Background(), "PVT_p", "PVTI_one", "PVTSSF_status", "opt")
	if err == nil {
		t.Fatal("a failed mutation must be an error")
	}
	if !strings.Contains(err.Error(), "not accessible by integration") {
		t.Errorf("error must name the cause, got %q", err)
	}
}

func TestAddItemReturnsTheItem(t *testing.T) {
	f := newGQLFake(t)
	it, err := f.client().AddItem(context.Background(), "PVT_p", "I_kwDOissue613")
	if err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if it.ID != "PVTI_added" || it.Content.Number != 613 {
		t.Fatalf("added item wrong: %+v", it)
	}
	req := f.lastRequest()
	if got, _ := req.Variables["contentId"].(string); got != "I_kwDOissue613" {
		t.Errorf("contentId = %q", got)
	}
}

func TestIssueContentIDResolvesNodeID(t *testing.T) {
	f := newGQLFake(t)
	id, err := f.client().IssueContentID(context.Background(), "SocialGouv/iterion", 613)
	if err != nil {
		t.Fatalf("IssueContentID: %v", err)
	}
	if id != "I_kwDOissue613" {
		t.Fatalf("content id = %q", id)
	}
	req := f.lastRequest()
	if got, _ := req.Variables["owner"].(string); got != "SocialGouv" {
		t.Errorf("owner = %q", got)
	}
	if got, _ := req.Variables["name"].(string); got != "iterion" {
		t.Errorf("name = %q", got)
	}
	if got := req.Variables["number"]; got != float64(613) {
		t.Errorf("number = %#v", got)
	}
}

func TestIssueContentIDRejectsMalformedRepo(t *testing.T) {
	f := newGQLFake(t)
	f.respond = func(string, map[string]any) (int, string) {
		t.Error("a malformed repo must fail before any API call")
		return 500, "{}"
	}
	if _, err := f.client().IssueContentID(context.Background(), "iterion", 1); err == nil {
		t.Fatal("want an error for a repo without an owner")
	}
}

// TestAdminClientIsABoardClient pins the optional-capability wiring: the
// GitHub admin client must satisfy forge.BoardClient so AsBoardClient finds it.
func TestAdminClientIsABoardClient(t *testing.T) {
	var admin forge.Admin = New(nil, "https://github.com", "tok")
	if _, ok := forge.AsBoardClient(admin); !ok {
		t.Fatal("github AdminClient must implement forge.BoardClient")
	}
}
