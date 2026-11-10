package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// gatewayDefaultModelSection returns the paragraph of docs/backends.md that
// tells operator tooling how to give a team its default gateway model: from
// its bold lead to the next heading.
func gatewayDefaultModelSection(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "backends.md"))
	if err != nil {
		t.Fatalf("read docs/backends.md: %v", err)
	}
	page := string(raw)
	const lead = "**A team's own gateway key**"
	start := strings.Index(page, lead)
	if start < 0 {
		t.Fatalf("docs/backends.md lost its %q paragraph — the operator contract this test drives", lead)
	}
	section := page[start:]
	if end := strings.Index(section, "\n#"); end >= 0 {
		section = section[:end]
	}
	return section
}

// documentedGatewayCell decodes the section's JSON block as the override
// cell it documents. Strict: a block that is anything else — a whole
// routing body above all, which a verbatim PUT applies as the team's
// ENTIRE policy — fails here by name.
func documentedGatewayCell(t *testing.T, section string) llmroute.Override {
	t.Helper()
	const fence = "```json\n"
	i := strings.Index(section, fence)
	if i < 0 {
		t.Fatalf("the gateway default-model paragraph carries no ```json block")
	}
	block := section[i+len(fence):]
	block = block[:strings.Index(block, "```")]
	dec := json.NewDecoder(strings.NewReader(block))
	dec.DisallowUnknownFields()
	var cell llmroute.Override
	if err := dec.Decode(&cell); err != nil {
		t.Fatalf("the documented JSON is not an override cell (%v):\n%s\nOperator tooling sends what the page shows; a whole routing body PUT verbatim replaces the team's policy — its allowlist, locks and every other cell — so the page documents the cell and the read-modify-write that upserts it", err, block)
	}
	return cell
}

// upsertGatewayModel applies the documented edit to a policy read back from
// the GET: the model pin of a "*" cell is replaced when one exists, a "*"
// cell carrying the model is appended otherwise, and nothing else moves.
func upsertGatewayModel(p *llmroute.Policy, model string) {
	for i := range p.Overrides {
		if p.Overrides[i].Selector == "*" && p.Overrides[i].Model != "" {
			p.Overrides[i].Model = model
			return
		}
	}
	p.Overrides = append(p.Overrides, llmroute.Override{Selector: "*", Model: model})
}

func readTeamRoutingPolicy(t *testing.T, s *Server, ctx context.Context, url string) routingPolicyView {
	t.Helper()
	w := getRoute(s, ctx, url)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, w.Code, w.Body.String())
	}
	var view routingPolicyView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return view
}

func routingPutBody(t *testing.T, policy any, stamp string) string {
	t.Helper()
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.WriteString(`{"routing": `)
	b.Write(raw)
	if stamp != "" {
		b.WriteString(`, "expected_updated_at": "` + stamp + `"`)
	}
	b.WriteString("}")
	return b.String()
}

// The gateway section of docs/backends.md is the contract operator tooling
// implements to give a team its default gateway model. This drives what the
// page documents through the real team routing-policy handlers: the JSON it
// shows is the override cell, the read-modify-write it describes keeps
// every other field and cell of a policy the team already has, and the
// stamp it names turns a concurrent edit into a 409 — the zero time
// included, for a team with no policy yet. The last step holds the page's
// warning to the handler: a write replaces the whole policy.
func TestGatewayDefaultModelDoc_TheDocumentedWriteKeepsTheTeamPolicy(t *testing.T) {
	section := gatewayDefaultModelSection(t)
	cell := documentedGatewayCell(t, section)
	if cell.Selector != "*" || !strings.HasPrefix(cell.Model, "openai_compatible/") {
		t.Fatalf("documented cell = %+v, want selector * and an openai_compatible/… model", cell)
	}
	for _, named := range []string{"expected_updated_at", "409", "0001-01-01T00:00:00Z"} {
		if !strings.Contains(section, named) {
			t.Errorf("the paragraph never names %s — without it the documented write is a blind last-write-wins over a concurrent editor", named)
		}
	}

	s := newOrgTestServer(t)
	factory, _ := routingPolicyStores()
	s.routingPolicyStoreFor = factory
	seedRoutingPolicyWorld(t, s)
	admin := asUser("u-teamadmin")
	const teamURL = "/api/teams/team1/routing-policy"

	// A team that already routes: an allowlist, strict, a judge cell, and a
	// "*" cell holding an effort beside the model it pins.
	seeded := `{"routing": {"providers": ["openai_compatible", "anthropic"], "strict": true, "overrides": [
		{"selector": "judge", "backend": "claw"},
		{"selector": "*", "model": "openai/gpt-5.5", "effort": "high"}
	]}}`
	if w := putRoute(s, admin, teamURL, seeded); w.Code != http.StatusOK {
		t.Fatalf("seed PUT = %d: %s", w.Code, w.Body.String())
	}

	before := readTeamRoutingPolicy(t, s, admin, teamURL)
	if before.Policy == nil || before.UpdatedAt == nil {
		t.Fatalf("seeded view = %+v, want the stored policy and its stamp", before)
	}
	edited := *before.Policy
	edited.Overrides = slices.Clone(edited.Overrides)
	upsertGatewayModel(&edited, cell.Model)
	write := routingPutBody(t, edited, before.UpdatedAt.Format(time.RFC3339Nano))
	if w := putRoute(s, admin, teamURL, write); w.Code != http.StatusOK {
		t.Fatalf("the documented write = %d: %s", w.Code, w.Body.String())
	}

	after := readTeamRoutingPolicy(t, s, admin, teamURL).Policy
	if after == nil {
		t.Fatal("the documented write left no policy")
	}
	if !slices.Equal(after.Providers, []string{"openai_compatible", "anthropic"}) {
		t.Errorf("providers = %v — the documented write dropped the team's allowlist", after.Providers)
	}
	if after.Strict == nil || !*after.Strict {
		t.Errorf("strict = %v — the documented write dropped it", after.Strict)
	}
	want := []llmroute.Override{
		{Selector: "judge", Backend: "claw"},
		{Selector: "*", Model: cell.Model, Effort: "high"},
	}
	if !slices.Equal(after.Overrides, want) {
		t.Errorf("overrides = %+v, want %+v — the gateway model on the * cell, its effort and the judge cell kept", after.Overrides, want)
	}

	// The stamp the page names: a writer that read before this write loses
	// as a 409 instead of erasing it.
	if w := putRoute(s, admin, teamURL, write); w.Code != http.StatusConflict {
		t.Errorf("a write on a stale stamp = %d, want 409: %s", w.Code, w.Body.String())
	}

	// A team with no policy yet: the GET returns no stamp, and the zero
	// time the page names guards the first write the same way.
	orgAdmin := asUser("u-orgadmin")
	const freshURL = "/api/teams/team2/routing-policy"
	if v := readTeamRoutingPolicy(t, s, orgAdmin, freshURL); v.Policy != nil || v.UpdatedAt != nil {
		t.Fatalf("team2 view = %+v, want no policy and no stamp", v)
	}
	var first llmroute.Policy
	upsertGatewayModel(&first, cell.Model)
	firstWrite := routingPutBody(t, first, "0001-01-01T00:00:00Z")
	if w := putRoute(s, orgAdmin, freshURL, firstWrite); w.Code != http.StatusOK {
		t.Fatalf("the first write on the zero stamp = %d: %s", w.Code, w.Body.String())
	}
	if w := putRoute(s, orgAdmin, freshURL, firstWrite); w.Code != http.StatusConflict {
		t.Errorf("a second first-write on the zero stamp = %d, want 409 — a policy landed in between", w.Code)
	}

	// The warning the page carries: a write REPLACES the whole policy.
	blind := routingPutBody(t, llmroute.Policy{Overrides: []llmroute.Override{cell}}, "")
	if w := putRoute(s, admin, teamURL, blind); w.Code != http.StatusOK {
		t.Fatalf("blind PUT = %d: %s", w.Code, w.Body.String())
	}
	if got := readTeamRoutingPolicy(t, s, admin, teamURL).Policy; got == nil || got.Providers != nil || got.Strict != nil || len(got.Overrides) != 1 {
		t.Errorf("after a blind PUT the policy = %+v — the page warns that a write replaces the whole policy, so a handler that merges needs a page that says so", got)
	}
}
