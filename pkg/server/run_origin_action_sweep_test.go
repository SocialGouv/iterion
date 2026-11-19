package server

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	"POST /api/runs/{id}/host-event":                          {"E", "the delivery loads unverified; the resume it triggers carries and verifies the expectation (D4d)"},
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

// runWSReadOnlyTypes are the cases of (*runConn).dispatch that change
// nothing: the act rung does not guard them and they need no carrier. A new
// case is either guarded (runWSMutatingTypes, then a carrier) or listed here
// with its reason.
var runWSReadOnlyTypes = map[string]string{
	wsTypeSubscribe:      "opens the event stream under the socket's binding",
	wsTypeUnsubscribe:    "closes the event stream",
	wsTypeSubscribeLogs:  "opens the log stream under the socket's binding",
	wsTypeUnsubscribeLog: "closes the log stream",
	wsTypePing:           "answers pong",
}

// mutationSweep is what the inventory is compared with: the non-GET
// run-wildcard routes as "METHOD pattern", the ladder actions, the
// envelope types the act rung guards and the envelope types the socket's
// dispatch handles (parsed from its source).
type mutationSweep struct {
	routes   []string
	ladder   map[string]bool
	envelope map[string]bool
	dispatch []string
}

// gaps lists every action without a carrier, every dispatch case neither
// guarded nor read-only, every guarded type no case handles, and every
// inventory entry nothing matches any more.
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
	handled := map[string]bool{}
	for _, typ := range sw.dispatch {
		handled[typ] = true
		if sw.envelope[typ] {
			continue
		}
		if _, ok := runWSReadOnlyTypes[typ]; !ok {
			out = append(out, "dispatch case neither guarded nor read-only: "+typ)
		}
	}
	for typ, mutating := range sw.envelope {
		if mutating && !handled[typ] {
			out = append(out, "guarded envelope no dispatch case handles: "+typ)
		}
	}
	for typ := range runWSReadOnlyTypes {
		if !handled[typ] {
			out = append(out, "stale read-only envelope: "+typ)
		}
	}
	sort.Strings(out)
	return out
}

// dispatchCases returns the envelope types the switch on env.Type in
// (*runConn).dispatch names in its cases, in src (a source file of this
// package), each constant resolved against consts.
func dispatchCases(src []byte, consts map[string]string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "runs_ws.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "dispatch" || fd.Recv == nil || len(fd.Recv.List) != 1 {
			continue
		}
		if star, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "runConn" {
				fn = fd
			}
		}
	}
	if fn == nil {
		return nil, fmt.Errorf("no (*runConn).dispatch in the source")
	}
	var cases, bad []string
	switches := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		tag, ok := sw.Tag.(*ast.SelectorExpr)
		if !ok || tag.Sel.Name != "Type" {
			return true
		}
		switches++
		for _, stmt := range sw.Body.List {
			for _, e := range stmt.(*ast.CaseClause).List {
				switch v := e.(type) {
				case *ast.Ident:
					if s, ok := consts[v.Name]; ok {
						cases = append(cases, s)
					} else {
						bad = append(bad, v.Name)
					}
				case *ast.BasicLit:
					if s, err := strconv.Unquote(v.Value); err == nil && v.Kind == token.STRING {
						cases = append(cases, s)
					} else {
						bad = append(bad, v.Value)
					}
				default:
					bad = append(bad, fmt.Sprintf("%T", e))
				}
			}
		}
		return false
	})
	if switches != 1 {
		return nil, fmt.Errorf("dispatch holds %d switches on the envelope type, want 1", switches)
	}
	if len(bad) != 0 {
		return nil, fmt.Errorf("dispatch cases the sweep cannot resolve: %v", bad)
	}
	return cases, nil
}

// packageStringConsts maps every string constant declared in the package's
// non-test files to its value.
func packageStringConsts() (map[string]string, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							out[id.Name] = s
						}
					}
				}
			}
		}
	}
	return out, nil
}

// sweptDispatchCases parses src as this package's runs_ws.go.
func sweptDispatchCases(t *testing.T, src []byte) []string {
	t.Helper()
	consts, err := packageStringConsts()
	if err != nil {
		t.Fatal(err)
	}
	cases, err := dispatchCases(src, consts)
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

// readRunsWS reads this package's runs_ws.go. RUN_MUTATION_SWEEP_OVERLAY
// names the `go test -overlay` file a mutant run compiles with: the sweep
// then parses the replacement it maps runs_ws.go to, as the compiler does —
// a plain read of the file would not see the mutant.
func readRunsWS(t *testing.T) []byte {
	t.Helper()
	path := "runs_ws.go"
	if overlay := os.Getenv("RUN_MUTATION_SWEEP_OVERLAY"); overlay != "" {
		raw, err := os.ReadFile(overlay)
		if err != nil {
			t.Fatal(err)
		}
		var o struct{ Replace map[string]string }
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("RUN_MUTATION_SWEEP_OVERLAY %s: %v", overlay, err)
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		if replacement, ok := o.Replace[abs]; ok {
			path = replacement
		}
	}
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func without(list []string, drop string) []string {
	var out []string
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
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

func productionMutationSweep(t *testing.T, srv *Server) mutationSweep {
	t.Helper()
	return mutationSweep{
		routes:   nonGETRunRoutes(srv),
		ladder:   runScopeLadderRoutes,
		envelope: runWSMutatingTypes,
		dispatch: sweptDispatchCases(t, readRunsWS(t)),
	}
}

// W6: every action that reaches one run — the non-GET run-wildcard routes
// of the mux, the ladder's GET actions, the envelopes the act rung guards —
// has a carrier in the frozen inventory; every case the socket's dispatch
// handles (parsed from its source) is guarded or named read-only; and the
// inventory names nothing that no longer exists.
func TestEveryRunMutationHasACarrier(t *testing.T) {
	fx := newGateFixture(t, true)
	sw := productionMutationSweep(t, fx.srv)
	if len(sw.routes) < 30 || len(sw.envelope) < 7 || len(sw.dispatch) < 12 {
		t.Fatalf("the sweep found %d routes, %d envelopes and %d dispatch cases: it does not see the server", len(sw.routes), len(sw.envelope), len(sw.dispatch))
	}
	if gaps := sw.gaps(mutationCarriers); len(gaps) != 0 {
		t.Fatalf("run mutation inventory:\n%s", strings.Join(gaps, "\n"))
	}
}

// The sweep can fail: a planted mutating route on the mux, a planted ladder
// action, a planted guarded envelope, a planted dispatch case the act rung
// does not guard (M7c, parsed from a copy of runs_ws.go), a guarded type no
// case handles and a stale entry are each reported.
func TestRunMutationSweepFindsPlantedShapes(t *testing.T) {
	base := productionMutationSweep(t, newGateFixture(t, true).srv)
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
	// A case added to dispatch without its entry in runWSMutatingTypes would
	// bypass the act rung and the carrier inventory both.
	src := string(readRunsWS(t))
	const anchor = "\tcase wsTypePing:\n"
	if strings.Count(src, anchor) != 1 {
		t.Fatal("fixture: the ping case of dispatch moved; plant the probe case elsewhere")
	}
	planted := strings.Replace(src, anchor, "\tcase \"rva_probe\":\n\t\tc.handleCancel(env)\n"+anchor, 1)
	for _, tc := range []struct {
		name string
		sw   mutationSweep
		want string
	}{
		{"M7a-route", productionMutationSweep(t, fx.srv), "no carrier: POST /api/runs/{id}/rva-mutation-probe"},
		{"M7b-ladder", mutationSweep{routes: base.routes, ladder: ladder, envelope: base.envelope, dispatch: base.dispatch}, "no carrier: ladder rva-ladder-probe"},
		{"M7w-envelope", mutationSweep{routes: base.routes, ladder: base.ladder, envelope: envelope, dispatch: append(append([]string(nil), base.dispatch...), "rva_probe")}, "no carrier: ws rva_probe"},
		{"M7c-dispatch-case", mutationSweep{routes: base.routes, ladder: base.ladder, envelope: base.envelope, dispatch: sweptDispatchCases(t, []byte(planted))}, "dispatch case neither guarded nor read-only: rva_probe"},
		{"guarded-type-without-a-case", mutationSweep{routes: base.routes, ladder: base.ladder, envelope: base.envelope, dispatch: without(base.dispatch, wsTypePause)}, "guarded envelope no dispatch case handles: " + wsTypePause},
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
