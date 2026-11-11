package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// requestRoutingWriteGateCases are the two blocks the write gate refuses
// and the launch's reading of a stored layer (llmroute.ValidateStored)
// reads: a node cell beside its every-bot spelling on one field, and the
// gateway hint beside a model that is no gateway route.
var requestRoutingWriteGateCases = []struct {
	name    string
	cells   []llmroute.Override
	refusal string
}{
	{"a cell and its every-bot spelling on one field", []llmroute.Override{
		{Selector: "work", Backend: "claw"},
		{Selector: "*/work", Backend: "claude_code"},
	}, `"work" and "*/work" are one selector`},
	{"the gateway hint beside a model that is no gateway route", []llmroute.Override{
		{Selector: "work", Provider: "openai_compatible", Model: "anthropic/claude-opus-5"},
	}, "the gateway hint serves gateway models only"},
}

// acceptedRequestRouting is a block both rules accept — `work` and `*/work`
// on two fields — the control that the seam refuses the write gate's rules,
// not every block.
var acceptedRequestRouting = []llmroute.Override{
	{Selector: "work", Backend: "claw"},
	{Selector: "*/work", Model: "openai/gpt-5.5"},
}

// refusalOf reads the message of an error body ({"error": "..."}).
func refusalOf(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return string(body)
	}
	return out.Error
}

func routingJSON(t *testing.T, cells []llmroute.Override) string {
	t.Helper()
	raw, err := json.Marshal(llmroute.Policy{Overrides: cells})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A launch's own routing block meets the write gate at POST /api/runs
// (ADR-129 §3: a block the request supplies meets the write gate),
// refused 400 naming the field before anything launches: the launch reads
// every layer with the stored rule only (resolveRunLLMRoutePolicy), so
// nothing else applies the write gate's rules to this block. A stored
// team layer holding the same cells is read with the stored rule and
// launches.
func TestRequestRouting_ALaunchsOwnBlockMeetsTheWriteGate(t *testing.T) {
	for _, tc := range requestRoutingWriteGateCases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &botidCapture{}
			s, _, ctx := newBotidSplitServer(t, cap, "probe", gateSpec{id: "t1"})
			rec := postLaunch(t, s, ctx, `{"bot_id":"probe","llm_routing":`+routingJSON(t, tc.cells)+`}`)
			if msg := refusalOf(t, rec.Body.Bytes()); rec.Code != http.StatusBadRequest || !strings.HasPrefix(msg, "llm_routing: ") || !strings.Contains(msg, tc.refusal) {
				t.Fatalf("launch = %d %s, want a 400 naming llm_routing and %s", rec.Code, rec.Body.String(), tc.refusal)
			}
			cap.mu.Lock()
			published := len(cap.specs)
			cap.mu.Unlock()
			if published != 0 {
				t.Fatalf("the refused launch published %d specs", published)
			}
		})
		t.Run(tc.name+", stored at the team level", func(t *testing.T) {
			cap := &botidCapture{}
			s, _, ctx := newBotidSplitServer(t, cap, "probe", gateSpec{id: "t1"})
			factory, _ := routingPolicyStores()
			s.routingPolicyStoreFor = factory
			putRoutingPolicyRecord(t, factory(platformcfg.TeamRoutingPolicyID("t1")), &platformcfg.RoutingPolicyRecord{
				Policy: &llmroute.Policy{Overrides: tc.cells},
			})
			rec := postLaunch(t, s, ctx, `{"bot_id":"probe"}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("launch over the stored team layer = %d %s, want 202", rec.Code, rec.Body.String())
			}
			if pol := cap.only(t).LLMRoutePolicy; pol == nil || len(pol.Overrides) != len(tc.cells) {
				t.Fatalf("snapshot = %+v, want the team's %d cells folded as written", pol, len(tc.cells))
			}
		})
	}
	cap := &botidCapture{}
	s, _, ctx := newBotidSplitServer(t, cap, "probe", gateSpec{id: "t1"})
	if rec := postLaunch(t, s, ctx, `{"bot_id":"probe","llm_routing":`+routingJSON(t, acceptedRequestRouting)+`}`); rec.Code != http.StatusAccepted {
		t.Fatalf("launch with a block the write gate accepts = %d %s, want 202", rec.Code, rec.Body.String())
	}
}

// The credential preview's personal lane takes the caller's run level as
// the launch does, so it meets the same write gate: a block the launch
// refuses previews as refused, 400 naming the field, never as a whitelist.
func TestRequestRouting_APreviewsRunLevelMeetsTheWriteGate(t *testing.T) {
	id := auth.Identity{UserID: "human", TeamID: "t1", Role: identity.RoleMember}
	for _, tc := range requestRoutingWriteGateCases {
		t.Run(tc.name, func(t *testing.T) {
			s, rs := newTeamForkServer(t, &tierPublisher{})
			pub := &credentialPreviewPublisher{}
			s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
			w := callCredentialPreview(s, `{"source":{"kind":"personal"},"bot_id":"probe","llm_routing":`+routingJSON(t, tc.cells)+`}`, id)
			if msg := refusalOf(t, w.Body.Bytes()); w.Code != http.StatusBadRequest || !strings.HasPrefix(msg, "llm_routing: ") || !strings.Contains(msg, tc.refusal) {
				t.Fatalf("preview = %d %s, want a 400 naming llm_routing and %s", w.Code, w.Body.String(), tc.refusal)
			}
			if pub.preview.LLMRoutePolicy != nil {
				t.Fatalf("the refused preview reached the publisher with %+v", pub.preview.LLMRoutePolicy)
			}
		})
	}
	s, rs := newTeamForkServer(t, &tierPublisher{})
	pub := &credentialPreviewPublisher{}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	if w := callCredentialPreview(s, `{"source":{"kind":"personal"},"bot_id":"probe","llm_routing":`+routingJSON(t, acceptedRequestRouting)+`}`, id); w.Code != http.StatusOK {
		t.Fatalf("preview with a block the write gate accepts = %d %s, want 200", w.Code, w.Body.String())
	}
}
