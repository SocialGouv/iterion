package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// dialRetireBot is retireBot with deep's model an env dial, as Revi's
// models are: the publisher reads glm-5.3 alone from it (the named gateway
// route that seals the team's key), and a runner that does not set the
// variable runs deepseek.
const dialRetireBot = `agent deep:
  backend: claw
  model: "${ITERION_RETIRE_DIAL_DEEP:-openai_compatible/scaleway/deepseek-v4.1-flash}"
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

// putWideFetchedFeed stores, at docID, an enabled fetched feed whose fresh
// state prices n random ids (so nothing compresses or repeats), and
// answers the ids.
func putWideFetchedFeed(t *testing.T, live *priceFeedLive, docID string, n int) []string {
	t.Helper()
	entries := map[string]modelspecs.Spec{}
	ids := make([]string, n)
	for i := range ids {
		buf := make([]byte, 12)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		ids[i] = "openrouter/" + hex.EncodeToString(buf)
		entries[ids[i]] = modelspecs.Spec{ContextWindow: 131072, MaxOutputTokens: 16384, InputCostPerM: 0.3, OutputCostPerM: 1.2, CacheReadCostPerM: 0.03}
	}
	putFeedDoc(t, live.configs, docID, pricefeed.FeedConfig{Name: "wide", Type: pricefeed.TypeExec, Enabled: true,
		Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(time.Second), Params: map[string]string{"command": "/nonexistent"}})
	now := time.Now().UTC()
	if err := live.states.Put(context.Background(), pricefeed.StateDocID(docID, "wide"), &pricefeed.StateRecord{
		Feed: "wide", Type: pricefeed.TypeExec, Status: pricefeed.StateOK, Entries: entries,
		RefreshedAt: now, FreshFor: pricefeed.D(2 * time.Hour), LastAttempt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

// wideDeclaration answers n random gateway ids and a declaration pricing
// each with a window — more rows than the run channel's budget carries.
func wideDeclaration(t *testing.T, n int) ([]string, string) {
	t.Helper()
	ids := make([]string, n)
	var b strings.Builder
	b.WriteString("{")
	for i := range ids {
		buf := make([]byte, 12)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		ids[i] = "vendor/" + hex.EncodeToString(buf)
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%s":{"input_usd_per_mtok":1,"output_usd_per_mtok":2,"context_window":128000}`, ids[i])
	}
	b.WriteString("}")
	return ids, b.String()
}

// A run whose deepseek node is an env dial does not name deepseek to the
// publisher. With the team's own declaration pushing the folded table over
// the run channel's budget — more declared rows than the budget keeps — the
// run still seals the platform declaration's deepseek row, for its
// accepted efforts, beside glm-5.3's, which it names; on the default
// runner it sends, sizes and prices both exactly as the retirement E2E's
// run naming them does. (A declared id without efforts that only a dial
// reaches keeps its row while the declared rows fit: the view's test reads
// it from the publisher's fold for a run naming nothing.)
func TestPriceFeedOverBudgetDialRunSealsTheDeclaredRows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	for _, name := range []string{
		compatgw.CatalogModelsEnv, compatgw.ReasoningEffortsEnv, compatgw.CatalogProviderEnv,
		compatgw.FeedTableEnv, compatgw.ResolvedEnvName, "ITERION_RETIRE_DIAL_DEEP",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy",
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))

	live := newPriceFeedLive(t)
	if code, out := live.do(t, "PUT", "/api/admin/price-feeds", live.root, priceFeedPutFeedBody(declaredFeed("pliage", retirePliage))); code != http.StatusOK {
		t.Fatalf("the platform declaration was refused: %d %s", code, out)
	}
	teamRows, teamDoc := wideDeclaration(t, 1000)
	if code, out := live.do(t, "PUT", "/api/teams/team-pf/price-feeds", live.orgAdmin, priceFeedPutFeedBody(declaredFeed("team-rows", teamDoc))); code != http.StatusOK {
		t.Fatalf("the team declaration was refused: %d %.300s", code, out)
	}
	wf := compileRetireSource(t, dialRetireBot)
	if named := model.RouteGatewayModelIDs(wf, model.ModelOverrides{}); !slices.Equal(named, []string{"glm-5.3"}) {
		t.Fatalf("the publisher reads gateway ids %q from the dial bot, want glm-5.3 alone: the case does not hold", named)
	}
	if whole := foldPriceFeed(context.Background(), live.configs, live.states, "org1", "team-pf", pricefeed.FoldConfig{Budget: math.MaxInt}, nil).encode(nil); len(whole) <= pricefeed.DefaultBudget {
		t.Fatalf("the folded table is %d bytes, want it over the %d-byte budget", len(whole), pricefeed.DefaultBudget)
	}

	gw := newRecordingGateway(t)
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", gw.srv.URL+"/v1")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-runner-gateway")
	runID, bundle := launchRetireSource(t, live, dialRetireBot, wf)
	table := pricefeed.ParseTable(string(bundle.GatewayPriceFeed))
	if table == nil {
		t.Fatalf("the launch sealed no table (%d bytes)", len(bundle.GatewayPriceFeed))
	}
	for _, id := range []string{"scaleway/deepseek-v4.1-flash", "glm-5.3"} {
		if e, ok := table.Entries[id]; !ok || e.Feed != "pliage" {
			t.Errorf("the sealed table prices %s from %+v, want the platform declaration's row", id, e)
		}
	}
	if _, ok := table.Entries[teamRows[0]]; ok {
		t.Errorf("the sealed table prices the team's %s, which the run does not name and the budget cannot keep", teamRows[0])
	}
	assertRetiredPoolRun(t, runRetireWorkflow(t, compatgw.OverlayFeedTable(context.Background(), bundle.GatewayPriceFeed), wf, gw, runID))
}

// The effective view marks what a run naming no gateway id — its model an
// env dial — does not seal once the table exceeds the run channel's
// budget: a fetched id's entry (entry_named_only) in the listing and asked
// by id; never a declared row or an id with accepted efforts, which such
// a run keeps; and with the efforts alone over budget, every id's efforts
// (efforts_named_only). Each mark is what the publisher seals for a run
// naming nothing.
func TestPriceFeedEffectiveViewMarksWhatARunNamingNoIDLacks(t *testing.T) {
	type modelWire struct {
		ID               string                  `json:"id"`
		Entry            *pricefeed.Entry        `json:"entry"`
		Efforts          *pricefeed.EffortsEntry `json:"efforts"`
		EntryNamedOnly   bool                    `json:"entry_named_only"`
		EffortsNamedOnly bool                    `json:"efforts_named_only"`
	}
	view := func(t *testing.T, live *priceFeedLive, query string) map[string]modelWire {
		t.Helper()
		code, out := live.do(t, "GET", "/api/teams/team-pf/price-feeds/effective"+query, live.viewer, "")
		if code != http.StatusOK {
			t.Fatalf("the view%s: %d %.300s", query, code, out)
		}
		var v struct {
			Models []modelWire `json:"models"`
		}
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatalf("decode: %v", err)
		}
		byID := map[string]modelWire{}
		for _, m := range v.Models {
			byID[m.ID] = m
		}
		return byID
	}
	unnamed := func(t *testing.T, live *priceFeedLive) *pricefeed.Table {
		t.Helper()
		return pricefeed.ParseTable(string(mustTable(t)(PublisherPriceFeedEffective(live.configs, live.states, nil)(context.Background(), "org1", "team-pf", nil))))
	}

	t.Run("prices over budget", func(t *testing.T) {
		live := newPriceFeedLive(t)
		putFeedDoc(t, live.configs, pricefeed.PlatformConfigDocID, declaredFeed("pliage", retirePliage))
		wide := putWideFetchedFeed(t, live, pricefeed.TeamConfigDocID("team-pf"), 600)
		sealed := unnamed(t, live)
		for _, query := range []string{"", "?model=" + wide[0] + "&model=scaleway/deepseek-v4.1-flash&model=glm-5.3"} {
			models := view(t, live, query)
			for id, wantNamedOnly := range map[string]bool{wide[0]: true, "scaleway/deepseek-v4.1-flash": false, "glm-5.3": false} {
				m, ok := models[id]
				if !ok || m.Entry == nil {
					t.Fatalf("view%s: %s answers no entry", query, id)
				}
				if m.EntryNamedOnly != wantNamedOnly {
					t.Errorf("view%s: %s entry_named_only = %v, want %v", query, id, m.EntryNamedOnly, wantNamedOnly)
				}
				if _, sealedEntry := sealed.Entries[id]; sealedEntry == m.EntryNamedOnly {
					t.Errorf("view%s: %s entry_named_only = %v, yet a run naming no id seals its entry: %v", query, id, m.EntryNamedOnly, sealedEntry)
				}
				if m.EffortsNamedOnly {
					t.Errorf("view%s: %s efforts_named_only is set, yet every id's efforts fit", query, id)
				}
			}
		}
	})

	t.Run("efforts alone over budget", func(t *testing.T) {
		live := newPriceFeedLive(t)
		putFeedDoc(t, live.configs, pricefeed.PlatformConfigDocID, declaredFeed("big", bigDeclaration(3000, false)))
		if sealed := unnamed(t, live); sealed != nil {
			t.Fatalf("a run naming no id seals %d effort lists, want none: the case does not hold", len(sealed.Efforts))
		}
		for _, query := range []string{"", "?model=scaleway/deepseek-v4.1-flash"} {
			m, ok := view(t, live, query)["scaleway/deepseek-v4.1-flash"]
			if !ok || m.Efforts == nil || !m.EffortsNamedOnly {
				t.Errorf("view%s: deepseek's efforts %+v, efforts_named_only %v; want the declaration's, marked", query, m.Efforts, m.EffortsNamedOnly)
			}
		}
	})

	t.Run("within budget", func(t *testing.T) {
		live := newPriceFeedLive(t)
		putFeedDoc(t, live.configs, pricefeed.PlatformConfigDocID, declaredFeed("pliage", retirePliage))
		for id, m := range view(t, live, "") {
			if m.EntryNamedOnly || m.EffortsNamedOnly {
				t.Errorf("%s is marked named-only in a table every run seals whole", id)
			}
		}
	})
}
