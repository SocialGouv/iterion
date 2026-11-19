package server

import (
	"net/http"
	"sort"
	"strings"
	"testing"
)

// carrierEntry is how a run-addressed action is held to the run its request
// expected (plan §1 D1): B the service call runs under the bound context; P
// a bound pre-check before an ID-keyed signal; E the expectation travels
// into the service; own the entity's own ID; adm admission only.
type carrierEntry struct{ carrier, reason string }

// mutationCarriers is the frozen inventory of every non-GET run-wildcard
// route, every ladder GET action (runScopeLadderRoutes) and every envelope
// the run socket's act rung guards (runWSMutatingTypes). An action added
// without an entry, or an entry nothing matches any more, fails
// TestEveryRunMutationHasACarrier. The E rows and the envelopes name the
// carrier their phase gives them.
var mutationCarriers = map[string]carrierEntry{
	"POST /api/runs/{id}/merge":                               {"B", "load, claim and every merge CAS under the binding; forge token and git act for the record the bound load returned"},
	"POST /api/runs/{id}/commit-and-finalize":                 {"B", "load, finalize SaveRun, re-read and the flip CAS under the binding; git is outside the store"},
	"POST /api/runs/{id}/merge/conflicts/resolve":             {"B", "the bound load and the refresh; the git staging is outside the store"},
	"POST /api/runs/{id}/merge/conflicts/resolve-with-agent":  {"B", "the bound load precedes the model call; no run write"},
	"POST /api/runs/{id}/merge/conflicts/finalize":            {"B", "bound load and CAS; git and the push are outside the store"},
	"POST /api/runs/{id}/merge/conflicts/abort":               {"B", "bound load and CAS; git reset is outside the store"},
	"POST /api/runs/{id}/rename":                              {"B", "bound load and SaveRun"},
	"DELETE /api/runs/{id}":                                   {"B", "bound load and DeleteRun (the bound door on the filesystem, the transaction on Mongo)"},
	"PUT /api/runs/{id}/tags":                                 {"B", "bound tenant pre-check and SetRunTags"},
	"POST /api/runs/{id}/notes":                               {"B", "bound tenant pre-check and AppendRunNote"},
	"POST /api/runs/{id}/watch/{issueID}":                     {"B", "bound AddWatchedIssues (Mongo: the metadata predicate)"},
	"DELETE /api/runs/{id}/watch/{issueID}":                   {"B", "bound RemoveWatchedIssues (Mongo: the metadata predicate)"},
	"POST /api/runs/{id}/queue-message":                       {"B", "bound load and inbox append"},
	"DELETE /api/runs/{id}/queue-message/{msgID}":             {"B", "bound inbox CAS"},
	"PUT /api/runs/{id}/files/content":                        {"B", "the bound load; the workspace write is outside the store"},
	"POST /api/runs/{id}/cancel":                              {"P", "bound pre-check of the ID-keyed cancel; the inactive branch's flip bound (B); the cloud flip is D8"},
	"POST /api/runs/{id}/pause":                               {"P", "bound pre-check of the ID-keyed pause"},
	"POST /api/runs/{id}/bump-loop":                           {"P", "the service's loads are the bound pre-checks of the steering signal"},
	"POST /api/runs/{id}/raise-budget":                        {"P", "the service's loads are the bound pre-checks of the steering signal"},
	"POST /api/runs/{id}/browser/attach":                      {"P", "bound pre-check of the ID-keyed browser start"},
	"POST /api/runs/{id}/answer-human":                        {"E", "continues an execution: the expectation travels into Resume (D4d)"},
	"POST /api/runs/{id}/interactions/{iid}/answer":           {"E", "record and queue bound, the resume carries the expectation (D4d)"},
	"POST /api/runs/{id}/resume":                              {"E", "the handler's load stays unbound and Resume verifies (D4a-b); its failures go through the confirm rule"},
	"POST /api/runs/{id}/rewind":                              {"E", "Rewind verifies after its load (D4e)"},
	"POST /api/runs/{id}/fork":                                {"E", "the fork verifies its source; the child is created unbound (D4e)"},
	"POST /api/runs/{id}/host-event":                          {"E", "verified after the delivery's load; the resume carries the expectation (D4d)"},
	"POST /api/runs/{id}/assistant-watches":                   {"E", "the target verified (D4e); the assistant run is body-carried; its failures go through the confirm rule"},
	"DELETE /api/runs/{id}/assistant-watches/{watchID}":       {"own", "stops the watch by its own ID"},
	"POST /api/runs/{id}/assistant-missions":                  {"E", "the target verified after the mission's load (D4e)"},
	"POST /api/runs/{id}/assistant-missions/{missionID}/stop": {"own", "stops the mission by its own ID"},

	"ladder browser/cdp": {"adm", "a GET action: admitted at the upgrade, the pump keeps no binding"},
	"ladder shell":       {"adm", "a GET action: admitted at the upgrade, the PTY keeps no binding"},

	"ws " + wsTypeCancel:              {"P", "pre-check of the ID-keyed cancel; the inactive flip bound on an expectation socket (D5)"},
	"ws " + wsTypePause:               {"P", "pre-check of the ID-keyed pause (D5)"},
	"ws " + wsTypeAnswer:              {"E", "the resume carries the expectation on an expectation socket (D5)"},
	"ws " + wsTypeQueueMessage:        {"B", "the append under the socket's binding on an expectation socket (D5)"},
	"ws " + wsTypeCancelQueuedMessage: {"B", "the inbox CAS under the socket's binding on an expectation socket (D5)"},
	"ws " + wsTypeBumpLoop:            {"P", "the service's loads are the pre-checks (D5)"},
	"ws " + wsTypeRaiseBudget:         {"P", "the service's loads are the pre-checks (D5)"},
}

// mutationSweep is what the inventory is compared with: the non-GET
// run-wildcard routes as "METHOD pattern", the ladder actions and the
// envelope types the act rung guards.
type mutationSweep struct {
	routes   []string
	ladder   map[string]bool
	envelope map[string]bool
}

// gaps lists every action without a carrier and every inventory entry
// nothing matches any more.
func (sw mutationSweep) gaps(carriers map[string]carrierEntry) []string {
	var out []string
	seen := map[string]bool{}
	need := func(key string) {
		seen[key] = true
		if _, ok := carriers[key]; !ok {
			out = append(out, "no carrier: "+key)
		}
	}
	for _, rt := range sw.routes {
		need(rt)
	}
	for key := range sw.ladder {
		need("ladder " + key)
	}
	for typ, mutating := range sw.envelope {
		if mutating {
			need("ws " + typ)
		}
	}
	for key := range carriers {
		if !seen[key] {
			out = append(out, "stale carrier entry: "+key)
		}
	}
	sort.Strings(out)
	return out
}

// nonGETRunRoutes lists the registered run-wildcard routes a non-GET method
// reaches. A route registered without a method answers every method, so it
// is listed as "ANY".
func nonGETRunRoutes(srv *Server) []string {
	var out []string
	for _, route := range srv.mux.Routes() {
		if !routeIsRunWildcard(route.Pattern) {
			continue
		}
		switch route.Method {
		case http.MethodGet, http.MethodHead:
			continue
		case "":
			out = append(out, "ANY "+route.Pattern)
		default:
			out = append(out, route.Method+" "+route.Pattern)
		}
	}
	sort.Strings(out)
	return out
}

func productionMutationSweep(srv *Server) mutationSweep {
	return mutationSweep{routes: nonGETRunRoutes(srv), ladder: runScopeLadderRoutes, envelope: runWSMutatingTypes}
}

// W6: every action that reaches one run — the non-GET run-wildcard routes
// of the mux, the ladder's GET actions, the envelopes the act rung guards —
// has a carrier in the frozen inventory, and the inventory names nothing
// that no longer exists.
func TestEveryRunMutationHasACarrier(t *testing.T) {
	fx := newGateFixture(t, true)
	sw := productionMutationSweep(fx.srv)
	if len(sw.routes) < 30 || len(sw.envelope) < 7 {
		t.Fatalf("the sweep found %d routes and %d envelopes: it does not see the server", len(sw.routes), len(sw.envelope))
	}
	if gaps := sw.gaps(mutationCarriers); len(gaps) != 0 {
		t.Fatalf("run mutation inventory:\n%s", strings.Join(gaps, "\n"))
	}
}

// The sweep can fail: a planted mutating route on the mux, a planted ladder
// action, a planted guarded envelope and a stale entry are each reported.
func TestRunMutationSweepFindsPlantedShapes(t *testing.T) {
	base := productionMutationSweep(newGateFixture(t, true).srv)
	fx := newGateFixture(t, true)
	fx.srv.mux.HandleFunc("POST /api/runs/{id}/rva-mutation-probe", func(http.ResponseWriter, *http.Request) {})
	ladder := map[string]bool{"rva-ladder-probe": true}
	for k, v := range runScopeLadderRoutes {
		ladder[k] = v
	}
	envelope := map[string]bool{"rva_probe": true}
	for k, v := range runWSMutatingTypes {
		envelope[k] = v
	}
	for _, tc := range []struct {
		name string
		sw   mutationSweep
		want string
	}{
		{"M7a-route", productionMutationSweep(fx.srv), "no carrier: POST /api/runs/{id}/rva-mutation-probe"},
		{"M7b-ladder", mutationSweep{routes: base.routes, ladder: ladder, envelope: base.envelope}, "no carrier: ladder rva-ladder-probe"},
		{"M7w-envelope", mutationSweep{routes: base.routes, ladder: base.ladder, envelope: envelope}, "no carrier: ws rva_probe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gaps := tc.sw.gaps(mutationCarriers)
			if len(gaps) != 1 || gaps[0] != tc.want {
				t.Fatalf("planted shape: got %v, want exactly %q", gaps, tc.want)
			}
		})
	}
	stale := map[string]carrierEntry{"POST /api/runs/{id}/gone": {"B", "planted"}}
	for k, v := range mutationCarriers {
		stale[k] = v
	}
	if gaps := base.gaps(stale); len(gaps) != 1 || gaps[0] != "stale carrier entry: POST /api/runs/{id}/gone" {
		t.Fatalf("a stale entry was not reported alone: %v", gaps)
	}
}
