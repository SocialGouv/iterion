package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The price feed's END-TO-END chain (ADR-163), deterministic, no model
// spend: the tenant stores → the manual refresh (the reconciler's own
// body) → the launch-time fold → the sealed bundle's round trip → the
// runner's environment overlay → the catalog's feed rung → the meter's
// price → the provenance that rides the node. Every hop is the
// production one; only the source is an exec script and the stores are
// the memory twins the conformance suite pins.
func TestPriceFeedEndToEndRunChain(t *testing.T) {
	s, _ := priceFeedFixtureServer(t) // arms the exec hatch for the fixture feed

	// 1. The operator declares a feed; the manual sync fills its state.
	script := writeTariffScript(t, 0.42, 1.4)
	docID := pricefeed.TeamConfigDocID("team1")
	if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{priceFeedFeed(script)}}); err != nil {
		t.Fatal(err)
	}
	outcome, err := s.priceFeedRefresh(context.Background(), docID, priceFeedFeed(script))
	if err != nil || outcome.Status != pricefeed.StateOK {
		t.Fatalf("the sync did not fill the feed: %+v err %v", outcome, err)
	}

	// 2. The launch-time fold answers the table the bundle carries. The
	// operator floor (this process's env) is set BELOW the feed's price:
	// the fold is what decides, not the deployment's cheaper table.
	t.Setenv(compatgw.CatalogModelsEnv, `{"glm-5.3":{"context_window":131072,"input_usd_per_mtok":9,"output_usd_per_mtok":9}}`)
	t.Setenv(compatgw.CatalogProviderEnv, "")
	t.Setenv(compatgw.ResolvedEnvName, "")
	t.Setenv(compatgw.FeedTableEnv, "")
	compatgw.ResetCatalogCaches()
	raw := EffectivePriceFeed(context.Background(), s.priceFeedConfigs, s.priceFeedStates, "org1", "team1", []string{"glm-5.3"}, nil)
	if raw == nil {
		t.Fatal("the launch fold answered no table")
	}

	// 3. The bundle seals and reopens with the table verbatim — the
	// runner's one channel that needs no database.
	sealer, err := secrets.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, keyID, err := secrets.SealRunBundle(sealer, "team1", "", "run-1", secrets.RunBundle{GatewayPriceFeed: raw})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := secrets.OpenRunBundle(sealer, "team1", "", "run-1", keyID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(bundle.GatewayPriceFeed) != string(raw) {
		t.Fatalf("the table did not survive the seal: %s vs %s", bundle.GatewayPriceFeed, raw)
	}

	// 4. The runner plants it on the run's environment — the ONE
	// bundle-to-context adapter the runner itself calls.
	ctx := compatgw.OverlayFeedTable(context.Background(), bundle.GatewayPriceFeed)
	get := func(name string) string {
		value, _ := credroute.LookupEnv(ctx, name)
		return value
	}

	// 5. The dispatch-side catalog resolution answers from the feed,
	// before the operator table, with the feed's identity and age.
	res := compatgw.ResolveCatalog("glm-5.3", nil, get)
	if res.Source != compatgw.SourceFeed("scw") {
		t.Fatalf("the dispatch did not resolve from the feed: %+v", res)
	}
	if res.Spec.InputCostPerM != 0.42 || res.Spec.OutputCostPerM != 1.4 {
		t.Fatalf("the feed's prices did not answer: %+v", res.Spec)
	}

	// 6. The meter prices the SAME usage at the SAME table through the
	// SAME environment — the stamp and the price cannot disagree.
	const in, out, cache = 200_000, 100_000, 800_000
	priced := cost.EstimateUSDWithCacheInEnv(ctx, modelroute.OpenAICompatible+"/glm-5.3", in, out, cache)
	// The script's table publishes no cache-read rate, so cached input
	// prices at the input rate — the pre-split arithmetic, bit for bit.
	want := (float64(in)*0.42 + float64(out)*1.4) / 1e6
	if priced != want {
		t.Fatalf("the meter priced at %v, the feed table says %v", priced, want)
	}
	// The env-less meter (the process floor) answers differently — the
	// divergence the ctx-aware form exists to close.
	if floored := cost.EstimateUSDWithCache("openai_compatible/glm-5.3", in, out, cache); floored == priced {
		t.Fatalf("the process floor priced identically to the feed: %v", floored)
	}

	// 7. The record the crossing carries names the feed and its age, and
	// the container reading it back resolves the same answer.
	forward := compatgw.ForwardableRecord("glm-5.3", res)
	if forward.Source != compatgw.SourceFeed("scw") || forward.RefreshedAt == nil {
		t.Fatalf("the forwardable record lost the feed provenance: %+v", forward)
	}
	containerEnv := map[string]string{compatgw.ResolvedEnvName: marshalJSON(t, forward)}
	containerGet := func(name string) string { return containerEnv[name] }
	back := compatgw.ResolveCatalog("glm-5.3", nil, containerGet)
	if back.Source != compatgw.SourceFeed("scw") || back.Spec.InputCostPerM != 0.42 {
		t.Fatalf("the in-container catalog did not answer from the record: %+v", back)
	}

	// 8. Staleness is judged at the FOLD, and the fold's judgment travels:
	// the same table aged past its window still answers for THIS run
	// (the invocation holds the answer it was dispatched with), while a
	// NEW fold over the aged STATE answers nothing — the operator floor.
	aged := pricefeed.ParseTable(string(raw))
	for id, e := range aged.Entries {
		e.RefreshedAt = time.Now().Add(-3 * time.Hour)
		aged.Entries[id] = e
	}
	agedJSON := marshalJSON(t, aged)
	agedCtx := compatgw.OverlayFeedTable(context.Background(), []byte(agedJSON))
	agedGet := func(name string) string {
		value, _ := credroute.LookupEnv(agedCtx, name)
		return value
	}
	if aged := compatgw.ResolveCatalog("glm-5.3", nil, agedGet); aged.Source != compatgw.SourceFeed("scw") || aged.Spec.InputCostPerM != 0.42 {
		t.Fatalf("the run's sealed table stopped answering mid-invocation: %+v", aged)
	}
	// The FOLD over the aged state refuses: the next run folds fresh.
	agedState, err := s.priceFeedStates.Get(context.Background(), pricefeed.StateDocID(pricefeed.TeamConfigDocID("team1"), "scw"))
	if err != nil || agedState == nil {
		t.Fatalf("no state to age: %+v err %v", agedState, err)
	}
	agedState.RefreshedAt = time.Now().Add(-3 * time.Hour)
	if err := s.priceFeedStates.Put(context.Background(), pricefeed.StateDocID(pricefeed.TeamConfigDocID("team1"), "scw"), agedState); err != nil {
		t.Fatal(err)
	}
	if stale := EffectivePriceFeed(context.Background(), s.priceFeedConfigs, s.priceFeedStates, "org1", "team1", []string{"glm-5.3"}, nil); stale != nil {
		t.Fatalf("a fold over aged state answered a table: %s", stale)
	}
}

func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The pool-retirement proof (ADR-166), deterministic, no model spend: a
// run on the DEFAULT runner — every OPENAI_COMPATIBLE_* table absent from
// its process — maps its effort, sizes its output and prices its usage
// from a PLATFORM declaration, and a twin run on a pool runner whose env
// tables state the same values sends the same model, reasoning_effort and
// max_tokens on the wire at the same cost; only the provenance differs.
// The hops are the production ones: the super-admin's PUT through the
// real route, the publisher's fold (EffectivePriceFeed over the named ids
// the publisher walks), the sealed bundle's round trip, the runner's
// bundle-to-context adapter for the table, the runner binary's credential
// lookup, the executor every plane builds (runview.BuildExecutor) and the
// engine; only the gateway is a loopback stub that records what it
// received. The stub is the runner's env-funded gateway
// (OPENAI_COMPATIBLE_BASE_URL / _API_KEY, which are no table): a sealed
// team key refuses a loopback http base URL
// (secrets.ValidateGatewayBaseURL), and which credential funds the call
// is not what this proves.
func TestPriceFeedEndToEndRetiresThePool(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	for _, name := range []string{
		compatgw.CatalogModelsEnv, compatgw.ReasoningEffortsEnv, compatgw.CatalogProviderEnv,
		compatgw.FeedTableEnv, compatgw.ResolvedEnvName,
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy",
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
	// The runner binary installs the run-credentials lookup at boot.
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))

	// 1. The super-admin declares the platform feed through the real route.
	live := newPriceFeedLive(t)
	if code, out := live.do(t, "PUT", "/api/admin/price-feeds", live.root, priceFeedPutFeedBody(declaredFeed("pliage", retirePliage))); code != http.StatusOK {
		t.Fatalf("the platform declaration was refused: %d %s", code, out)
	}

	gw := newRecordingGateway(t)
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", gw.srv.URL+"/v1")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-runner-gateway")
	wf := compileRetireBot(t)
	named := model.RouteGatewayModelIDs(wf, model.ModelOverrides{})

	// 2. The publisher's fold for the team, sealed in the run's bundle.
	raw := EffectivePriceFeed(context.Background(), live.configs, live.states, "org1", "team-pf", named, nil)
	if raw == nil {
		// Not fatal: the run below then shows what a default runner does
		// without the declaration, assertion by assertion.
		t.Error("the launch fold answered no table")
	}
	feedRun := runRetireWorkflow(t, wf, gw, raw)

	// 3. What the gateway RECEIVED, and what the run recorded.
	deep, glm := feedRun.bodies["scaleway/deepseek-v4.1-flash"], feedRun.bodies["glm-5.3"]
	if deep.ReasoningEffort != "high" {
		t.Errorf("the gateway received reasoning_effort %q for deepseek, want high (medium mapped by the platform declaration)", deep.ReasoningEffort)
	}
	if glm.MaxTokens != 16384 {
		t.Errorf("the gateway received max_tokens %d for glm-5.3, want the declared max output 16384", glm.MaxTokens)
	}
	if adj := feedRun.adjusted["deep"]; adj["requested"] != "medium" || adj["applied"] != "high" || adj["source"] != "feed:pliage" {
		t.Errorf("reasoning_effort_adjusted = %v, want medium -> high from feed:pliage", adj)
	}
	for node, spec := range feedRun.specs {
		if spec.Source != "feed:pliage" {
			t.Errorf("%s: _gateway_spec.source = %q, want feed:pliage", node, spec.Source)
		}
	}
	if w := feedRun.specs["glm"].ContextWindow; w != 256000 {
		t.Errorf("glm: _gateway_spec.context_window = %d, want 256000", w)
	}
	for _, node := range []string{"deep", "glm"} {
		if feedRun.costs[node] <= 0 {
			t.Errorf("%s: _cost_usd = %v, want a priced node", node, feedRun.costs[node])
		}
		if feedRun.sampled[node] <= 0 {
			t.Errorf("%s: no usage_progress sample carried a USD figure (%v)", node, feedRun.sampled[node])
		}
	}

	// 4. The parity twin: the pool's env tables, no feed in the bundle.
	compatgw.ResetCatalogCaches()
	t.Setenv(compatgw.CatalogModelsEnv, retirePoolModels)
	t.Setenv(compatgw.ReasoningEffortsEnv, retirePoolEfforts)
	poolRun := runRetireWorkflow(t, wf, gw, nil)
	for id, body := range feedRun.bodies {
		if poolRun.bodies[id] != body {
			t.Errorf("%s: the wire differs between the declaration and the pool tables: %+v vs %+v", id, body, poolRun.bodies[id])
		}
	}
	for _, node := range []string{"deep", "glm"} {
		if poolRun.costs[node] != feedRun.costs[node] || poolRun.sampled[node] != feedRun.sampled[node] {
			t.Errorf("%s: cost %v / sample %v from the declaration, %v / %v from the pool tables", node, feedRun.costs[node], feedRun.sampled[node], poolRun.costs[node], poolRun.sampled[node])
		}
		if poolRun.specs[node].Source != compatgw.SourceOperator {
			t.Errorf("%s: the pool twin's provenance is %q, want operator", node, poolRun.specs[node].Source)
		}
	}
	if src := poolRun.adjusted["deep"]["source"]; src != compatgw.SourceReasoningEffortsEnv {
		t.Errorf("the pool twin's effort source is %v, want %s", src, compatgw.SourceReasoningEffortsEnv)
	}
}

// retirePliage is the platform declaration: deepseek priced with its
// accepted efforts, glm-5.3 priced with its window and max output.
const retirePliage = `{"scaleway/deepseek-v4.1-flash":{"context_window":128000,"max_output_tokens":8192,"input_usd_per_mtok":0.6,"output_usd_per_mtok":2.4,"reasoning_efforts":["low","high","xhigh","max"]},"glm-5.3":{"context_window":256000,"max_output_tokens":16384,"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`

// retirePoolModels and retirePoolEfforts are the same facts as the
// pool's two env tables.
const (
	retirePoolModels  = `{"scaleway/deepseek-v4.1-flash":{"context_window":128000,"max_output_tokens":8192,"input_usd_per_mtok":0.6,"output_usd_per_mtok":2.4},"glm-5.3":{"context_window":256000,"max_output_tokens":16384,"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`
	retirePoolEfforts = `{"scaleway/deepseek-v4.1-flash":["low","high","xhigh","max"]}`
)

const retireBot = `agent deep:
  backend: claw
  model: "openai_compatible/scaleway/deepseek-v4.1-flash"
  reasoning_effort: medium
  user: "Review the change."

agent glm:
  backend: claw
  model: "openai_compatible/glm-5.3"
  user: "Summarize the review."

workflow w:
  worktree: none
  sandbox: none
  entry: deep
  deep -> glm
  glm -> done
`

func compileRetireBot(t *testing.T) *ir.Workflow {
	t.Helper()
	pr := parsertest.Parse("retire.bot", retireBot)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("fixture does not parse: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("fixture does not compile: %s", d.Error())
		}
	}
	return cr.Workflow
}

// retireWire is the part of a chat-completions body the proof compares.
type retireWire struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	MaxTokens       int    `json:"max_tokens"`
}

// recordingGateway is a loopback chat-completions endpoint: it records
// each request's wire fields by model and answers one streamed text turn
// reporting a usage large enough to clear the usage_progress USD floor.
type recordingGateway struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen map[string]retireWire
}

func newRecordingGateway(t *testing.T) *recordingGateway {
	t.Helper()
	g := &recordingGateway{seen: map[string]retireWire{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("gateway: read body: %v", err)
			return
		}
		var req retireWire
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("gateway: decode body: %v (%s)", err, body)
			return
		}
		g.mu.Lock()
		g.seen[req.Model] = req
		g.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":100000,"completion_tokens":2000}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *recordingGateway) take() map[string]retireWire {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.seen
	g.seen = map[string]retireWire{}
	return out
}

// retireRun is what one run left: the wire per model, the effort
// adjustment per node, and per node its `_gateway_spec`, `_cost_usd` and
// the largest usage_progress USD figure.
type retireRun struct {
	bodies   map[string]retireWire
	adjusted map[string]map[string]any
	specs    map[string]compatgw.ResolvedEnvRecord
	costs    map[string]float64
	sampled  map[string]float64
}

// runRetireWorkflow seals a bundle carrying the folded table (none when
// raw is nil), opens it, plants it through the runner's adapter and runs
// the workflow through the executor every plane builds.
func runRetireWorkflow(t *testing.T, wf *ir.Workflow, gw *recordingGateway, raw json.RawMessage) retireRun {
	t.Helper()
	runID := fmt.Sprintf("run-retire-%d", time.Now().UnixNano())
	dek, err := secrets.NewRunBundleDEK()
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewAESGCMSealer(dek)
	if err != nil {
		t.Fatal(err)
	}
	sealed, keyID, err := secrets.SealRunBundle(sealer, "team-pf", "", runID, secrets.RunBundle{GatewayPriceFeed: raw})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := secrets.OpenRunBundle(sealer, "team-pf", "", runID, keyID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	// The runner's bundle-to-context step for the table: the one adapter
	// injectCredentials calls.
	ctx := compatgw.OverlayFeedTable(context.Background(), bundle.GatewayPriceFeed)

	storeDir := t.TempDir()
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	// The run below executes with no sandbox (WithSandboxOverride("none")),
	// so the spec says it knows the sandbox tiers and names its bot the way
	// every launch surface does.
	exec, err := runview.BuildExecutor(runview.ExecutorSpec{
		Ctx: ctx, Workflow: wf, Store: st, RunID: runID, Logger: iterlog.Nop(), WorkDir: workDir,
		BotID:           runview.ResolveBotID("pricefeed-e2e", "", ""),
		SandboxOverride: "none", SandboxTiersKnown: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	if err := runtime.New(wf, st, exec, runtime.WithWorkDir(workDir), runtime.WithSandboxOverride("none")).Run(ctx, runID, nil); err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	events, err := st.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run := retireRun{
		bodies:   gw.take(),
		adjusted: map[string]map[string]any{},
		specs:    map[string]compatgw.ResolvedEnvRecord{},
		costs:    map[string]float64{},
		sampled:  map[string]float64{},
	}
	for _, evt := range events {
		switch evt.Type {
		case store.EventReasoningEffortAdjusted:
			run.adjusted[evt.NodeID] = evt.Data
		case store.EventUsageProgress:
			if used, _ := evt.Data["used"].(float64); used > run.sampled[evt.NodeID] {
				run.sampled[evt.NodeID] = used
			}
		case store.EventNodeFinished:
			out, _ := evt.Data["output"].(map[string]any)
			if out == nil {
				continue
			}
			rawSpec, err := json.Marshal(out["_gateway_spec"])
			if err != nil {
				t.Fatal(err)
			}
			var spec compatgw.ResolvedEnvRecord
			if err := json.Unmarshal(rawSpec, &spec); err == nil {
				run.specs[evt.NodeID] = spec
			}
			run.costs[evt.NodeID], _ = out["_cost_usd"].(float64)
		}
	}
	for _, node := range []string{"deep", "glm"} {
		if _, ok := run.specs[node]; !ok {
			t.Fatalf("inert bench: node %s left no node_finished output (%d events)", node, len(events))
		}
	}
	if len(run.bodies) != 2 {
		t.Fatalf("inert bench: the gateway received %v, want one request per node", run.bodies)
	}
	return run
}
