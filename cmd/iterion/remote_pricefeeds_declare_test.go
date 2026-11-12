package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
	"github.com/SocialGouv/iterion/pkg/server"
)

// priceFeedsLive is the real pkg/server surface in dev mode — every request
// a super-admin — on the price feed's memory stores, holding org org-pf and
// its team team-pf. The CLI's environment points at it with team-pf as the
// default team; requests records each request's "METHOD path?query".
type priceFeedsLive struct {
	configs *pricefeed.MemoryConfigStore
	states  *pricefeed.MemoryStateStore

	mu       sync.Mutex
	requests []string
}

func newPriceFeedsLive(t *testing.T) *priceFeedsLive {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	ids := identity.NewMemoryStore()
	svc, err := auth.NewService(auth.Config{
		Store: ids, Sessions: auth.NewMemorySessionStore(), Signer: signer,
		SignupMode: auth.SignupOpen, RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	ctx := context.Background()
	if _, err := ids.CreateOrg(ctx, identity.Org{ID: "org-pf", Name: "org-pf", Slug: "org-pf", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := ids.CreateTeam(ctx, identity.Team{ID: "team-pf", Name: "team-pf", Slug: "team-pf", OrgID: "org-pf", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	live := &priceFeedsLive{configs: pricefeed.NewMemoryConfigStore(), states: pricefeed.NewMemoryStateStore()}
	s := server.New(server.Config{
		WorkDir: t.TempDir(), Bind: "127.0.0.1", SkipProjectRegistration: true, DisableAuth: true,
		AuthService: svc, AuthSigner: signer,
		PriceFeedConfigs: live.configs, PriceFeedStates: live.states,
	}, iterlog.New(iterlog.LevelError, nil))
	handler := s.Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		line := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			line += "?" + r.URL.RawQuery
		}
		live.mu.Lock()
		live.requests = append(live.requests, line)
		live.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	priceFeedsScratchEnv(t)
	t.Setenv("ITERION_REMOTE_URL", hs.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "dev-mode-token-not-a-secret")
	t.Setenv("ITERION_REMOTE_TEAM", "team-pf")
	return live
}

// priceFeedsScratchEnv points the CLI's home, temp dir and iterion home at
// directories the test owns, and clears every remote scope the operator's
// environment could carry.
func priceFeedsScratchEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv("ITERION_TOKEN", "")
	t.Setenv("ITERION_REMOTE_ORG", "")
}

// puts counts the PUT requests the server received.
func (l *priceFeedsLive) puts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.requests {
		if strings.HasPrefix(r, "PUT ") {
			n++
		}
	}
	return n
}

// record answers the stored document of docID, failing the test when there
// is none.
func (l *priceFeedsLive) record(t *testing.T, docID string) *pricefeed.ConfigRecord {
	t.Helper()
	rec, err := l.configs.Get(context.Background(), docID)
	if err != nil || rec == nil {
		t.Fatalf("read %s: %v (record %v)", docID, err, rec)
	}
	return rec
}

// seed stores feeds as docID's document.
func (l *priceFeedsLive) seed(t *testing.T, docID string, feeds ...pricefeed.FeedConfig) *pricefeed.ConfigRecord {
	t.Helper()
	if err := l.configs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: feeds, UpdatedBy: "seed"}); err != nil {
		t.Fatalf("seed %s: %v", docID, err)
	}
	return l.record(t, docID)
}

// declaredEntriesOf parses a declared feed's document the way the fold
// reads it.
func declaredEntriesOf(t *testing.T, feed pricefeed.FeedConfig) map[string]modelspecs.Spec {
	t.Helper()
	src, ok := pricefeed.SourceFor(pricefeed.TypeDeclared)
	if !ok {
		t.Fatal("no declared source registered")
	}
	entries, err := src.(pricefeed.StaticSource).Entries(feed.Params)
	if err != nil {
		t.Fatalf("feed %s's stored document does not parse: %v", feed.Name, err)
	}
	return entries
}

func writeTestFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func feedNames(rec *pricefeed.ConfigRecord) []string {
	names := make([]string, 0, len(rec.Feeds))
	for _, f := range rec.Feeds {
		names = append(names, f.Name)
	}
	return names
}

func hourlyFeed(name, typ string, params map[string]string) pricefeed.FeedConfig {
	return pricefeed.FeedConfig{Name: name, Type: typ, Enabled: true,
		Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(10 * time.Second), Params: params}
}

func declaredTestFeed(name, entries string, interval time.Duration) pricefeed.FeedConfig {
	return pricefeed.FeedConfig{Name: name, Type: pricefeed.TypeDeclared, Enabled: true,
		Interval: pricefeed.D(interval), Timeout: pricefeed.D(time.Second), Params: map[string]string{"entries": entries}}
}

const (
	oldPliage = `{"glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15},"retired-id":{"input_usd_per_mtok":1,"output_usd_per_mtok":2}}`
	newPliage = `{
  "glm-5.3": {"context_window": 256000, "max_output_tokens": 16384, "input_usd_per_mtok": 2.01, "output_usd_per_mtok": 6.15},
  "scaleway/deepseek-v4.1-flash": {"reasoning_efforts": ["low", "high", "xhigh", "max"]}
}`
)

// declare replaces ONE feed of the scope's list: the named declaration's
// document, the other feeds as stored and in their place, the declaration's
// own cadence and switch kept. --first moves it ahead of the others; a new
// declaration lands last, enabled, at the 30-day/1-second cadence a
// declaration ignores. The per-id diff names what moved.
func TestRemotePriceFeedsDeclareUpsertsOneFeed(t *testing.T) {
	live := newPriceFeedsLive(t)
	scaleway := hourlyFeed("scaleway", pricefeed.TypeScaleway, nil)
	public := hourlyFeed("public", pricefeed.TypeModelsDev, map[string]string{"provider": "scaleway", "url": "https://models.dev/api.json"})
	live.seed(t, pricefeed.PlatformConfigDocID, scaleway, declaredTestFeed("pliage", oldPliage, 7*24*time.Hour), public)
	doc := writeTestFile(t, "pliage.json", newPliage)

	out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "declare", "pliage", "--platform", "--file", doc)
	if err != nil {
		t.Fatalf("declare: %v\n%s", err, out)
	}
	rec := live.record(t, pricefeed.PlatformConfigDocID)
	if got := feedNames(rec); !reflect.DeepEqual(got, []string{"scaleway", "pliage", "public"}) {
		t.Fatalf("feed order %v, want the declaration in its place", got)
	}
	if !reflect.DeepEqual(rec.Feeds[0], scaleway) || !reflect.DeepEqual(rec.Feeds[2], public) {
		t.Errorf("declare rewrote another feed: %+v, want %+v and %+v", rec.Feeds, scaleway, public)
	}
	pliage := rec.Feeds[1]
	if pliage.Type != pricefeed.TypeDeclared || !pliage.Enabled || pliage.Interval != pricefeed.D(7*24*time.Hour) || pliage.Timeout != pricefeed.D(time.Second) {
		t.Errorf("the declaration's own config moved: %+v", pliage)
	}
	want, err := pricefeed.ParseFeedEntries(newPliage, "want")
	if err != nil {
		t.Fatal(err)
	}
	if got := declaredEntriesOf(t, pliage); !reflect.DeepEqual(got, want) {
		t.Errorf("stored declaration %v, want the file's %v", got, want)
	}
	for _, line := range []string{
		"retired-id",
		"scaleway/deepseek-v4.1-flash",
		"(absent) → unpriced efforts low,high,xhigh,max",
		"window 256000",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the diff misses %q:\n%s", line, out)
		}
	}

	if out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "declare", "pliage", "--platform", "--file", doc, "--first"); err != nil {
		t.Fatalf("declare --first: %v\n%s", err, out)
	}
	if got := feedNames(live.record(t, pricefeed.PlatformConfigDocID)); !reflect.DeepEqual(got, []string{"pliage", "scaleway", "public"}) {
		t.Fatalf("--first left the order %v", got)
	}

	if out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "declare", "extra", "--platform", "--file", doc); err != nil {
		t.Fatalf("declare a new feed: %v\n%s", err, out)
	}
	rec = live.record(t, pricefeed.PlatformConfigDocID)
	if got := feedNames(rec); !reflect.DeepEqual(got, []string{"pliage", "scaleway", "public", "extra"}) {
		t.Fatalf("a new declaration landed at %v, want last", got)
	}
	extra := rec.Feeds[3]
	if !extra.Enabled || extra.Interval != pricefeed.D(pricefeed.MaxInterval) || extra.Timeout != pricefeed.D(pricefeed.MinTimeout) {
		t.Errorf("a new declaration's config: %+v", extra)
	}
}

// --dry-run reads the scope, prints the diff and the document it would
// store, and writes nothing.
func TestRemotePriceFeedsDeclareDryRunWritesNothing(t *testing.T) {
	live := newPriceFeedsLive(t)
	before := live.seed(t, pricefeed.PlatformConfigDocID, declaredTestFeed("pliage", oldPliage, 30*24*time.Hour))
	doc := writeTestFile(t, "pliage.json", newPliage)

	out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "declare", "pliage", "--platform", "--file", doc, "--dry-run")
	if err != nil {
		t.Fatalf("declare --dry-run: %v\n%s", err, out)
	}
	if n := live.puts(); n != 0 {
		t.Fatalf("--dry-run sent %d PUT(s)", n)
	}
	if after := live.record(t, pricefeed.PlatformConfigDocID); !reflect.DeepEqual(after, before) {
		t.Fatalf("--dry-run changed the stored document: %+v, was %+v", after, before)
	}
	if !strings.Contains(out, "retired-id") || !strings.Contains(out, `"scaleway/deepseek-v4.1-flash":{"reasoning_efforts":["low","high","xhigh","max"]}`) {
		t.Fatalf("--dry-run must print the diff and the document it would store:\n%s", out)
	}
}

// --models-file and --efforts-file take a pool's two env tables as written —
// OPENAI_COMPATIBLE_MODELS and OPENAI_COMPATIBLE_REASONING_EFFORTS — and
// declare them as one document: each priced entry with its efforts beside
// it, whatever spelling each table used for the id, and an id only the
// efforts table names declared by its efforts alone. Either file alone is a
// declaration too.
func TestRemotePriceFeedsDeclareMergesAPoolsTwoTables(t *testing.T) {
	live := newPriceFeedsLive(t)
	models := writeTestFile(t, "models.json", `{
  "Scaleway/DeepSeek-v4.1-Flash": {"context_window": 128000, "max_output_tokens": 8192, "input_usd_per_mtok": 0.2, "output_usd_per_mtok": 0.8, "cache_read_usd_per_mtok": 0.02},
  "glm-5.3": {"context_window": 256000, "max_output_tokens": 16384, "input_usd_per_mtok": 2.01, "output_usd_per_mtok": 6.15}
}`)
	efforts := writeTestFile(t, "efforts.json", `{"scaleway/deepseek-v4.1-flash": ["low", "high", "xhigh", "max"], "mistral/magistral": ["LOW", "high"]}`)

	out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "declare", "pliage", "--platform", "--models-file", models, "--efforts-file", efforts)
	if err != nil {
		t.Fatalf("declare the pool's tables: %v\n%s", err, out)
	}
	rec := live.record(t, pricefeed.PlatformConfigDocID)
	if len(rec.Feeds) != 1 {
		t.Fatalf("stored %d feeds, want the one declaration", len(rec.Feeds))
	}
	want := map[string]modelspecs.Spec{
		"scaleway/deepseek-v4.1-flash": {ContextWindow: 128000, MaxOutputTokens: 8192, InputCostPerM: 0.2, OutputCostPerM: 0.8, CacheReadCostPerM: 0.02,
			ReasoningEfforts: []string{"low", "high", "xhigh", "max"}},
		"glm-5.3":           {ContextWindow: 256000, MaxOutputTokens: 16384, InputCostPerM: 2.01, OutputCostPerM: 6.15},
		"mistral/magistral": {ReasoningEfforts: []string{"low", "high"}},
	}
	if got := declaredEntriesOf(t, rec.Feeds[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("the merged declaration:\n got %+v\nwant %+v", got, want)
	}

	if out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "declare", "efforts-only", "--platform", "--efforts-file", efforts); err != nil {
		t.Fatalf("declare an efforts table alone: %v\n%s", err, out)
	}
	rec = live.record(t, pricefeed.PlatformConfigDocID)
	if got := declaredEntriesOf(t, rec.Feeds[1]); len(got) != 2 || got["glm-5.3"].InputCostPerM != 0 || len(got["scaleway/deepseek-v4.1-flash"].ReasoningEfforts) != 4 {
		t.Fatalf("an efforts table alone declared %+v", got)
	}
}

// The scope's view renders some params redacted (a credential-shaped key, a
// URL's userinfo or query): writing the list back would store the redaction
// over the credential. declare refuses, naming the feed and the remedy, and
// the stored credential stays.
func TestRemotePriceFeedsDeclareRefusesToWriteBackARedactedParam(t *testing.T) {
	for _, other := range []pricefeed.FeedConfig{
		hourlyFeed("gw", pricefeed.TypeLiteLLM, map[string]string{"url": "https://gw.example/model/info", "token": "litellm-bearer-kept"}),
		hourlyFeed("tariffs", pricefeed.TypeModelsDev, map[string]string{"provider": "scaleway", "url": "https://tariffs.example/api.json?token=kept"}),
	} {
		t.Run(other.Name, func(t *testing.T) {
			live := newPriceFeedsLive(t)
			before := live.seed(t, pricefeed.PlatformConfigDocID, other, declaredTestFeed("pliage", oldPliage, 30*24*time.Hour))
			doc := writeTestFile(t, "pliage.json", newPliage)
			out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "declare", "pliage", "--platform", "--file", doc)
			if err == nil || !strings.Contains(err.Error(), `"`+other.Name+`"`) || !strings.Contains(err.Error(), "price-feeds set") {
				t.Fatalf("declare over a redacted %s: err %v — want a refusal naming the feed and the remedy\n%s", other.Name, err, out)
			}
			if after := live.record(t, pricefeed.PlatformConfigDocID); !reflect.DeepEqual(after, before) {
				t.Fatalf("the refusal wrote: %+v, was %+v", after, before)
			}
		})
	}
}

// A name already taken by a fetched feed is not a declaration to replace:
// declare refuses and writes nothing.
func TestRemotePriceFeedsDeclareRefusesAFeedOfAnotherType(t *testing.T) {
	live := newPriceFeedsLive(t)
	before := live.seed(t, pricefeed.PlatformConfigDocID, hourlyFeed("pliage", pricefeed.TypeScaleway, nil))
	doc := writeTestFile(t, "pliage.json", newPliage)
	out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "declare", "pliage", "--platform", "--file", doc)
	if err == nil || !strings.Contains(err.Error(), "scaleway") {
		t.Fatalf("declare over a scaleway feed: err %v\n%s", err, out)
	}
	if after := live.record(t, pricefeed.PlatformConfigDocID); !reflect.DeepEqual(after, before) {
		t.Fatalf("the refusal wrote: %+v", after)
	}
}

// A document the declared source refuses, a bad feed name, and a malformed
// set of input flags are refused before any request — the same parser the
// server applies, run locally.
func TestRemotePriceFeedsDeclareValidatesBeforeAnyRequest(t *testing.T) {
	windowOnly := writeTestFile(t, "window.json", `{"glm-5.3":{"context_window":256000}}`)
	good := writeTestFile(t, "good.json", newPliage)
	badEfforts := writeTestFile(t, "efforts.json", `{"glm-5.3":["low","turbo"]}`)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"window without prices", []string{"pliage", "--file", windowOnly}, `"glm-5.3"`},
		{"bad feed name", []string{"Pliage_1", "--file", good}, "Pliage_1"},
		{"unknown effort level", []string{"pliage", "--efforts-file", badEfforts}, "turbo"},
		{"two inputs", []string{"pliage", "--file", good, "--models-file", good}, "file"},
		{"no input", []string{"pliage"}, "file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, out, err := priceFeedsServe(t, func(*http.Request) (int, string) { return http.StatusOK, priceFeedsListAnswer },
				append([]string{"remote", "price-feeds", "declare"}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want a refusal naming %q\n%s", err, tc.want, out)
			}
			if len(got) != 0 {
				t.Fatalf("a refused declaration reached the server: %q", got)
			}
		})
	}
}

// The write is a CAS on the stamp declare read: a concurrent write's 409 and
// any other server refusal come back as the server said them.
func TestRemotePriceFeedsDeclareRelaysTheServersRefusals(t *testing.T) {
	doc := writeTestFile(t, "pliage.json", newPliage)
	view := `{"scope":"team","origin":"db","updated_at":"2026-11-12T08:00:00Z","source_types":["declared"],"feeds":[]}`
	for _, tc := range []struct {
		code int
		body string
	}{
		{http.StatusConflict, `{"error":"price feeds changed concurrently — re-read and retry"}`},
		{http.StatusBadRequest, `{"error":"feed \"pliage\": a rule only the server knows"}`},
	} {
		got, out, err := priceFeedsServe(t, func(r *http.Request) (int, string) {
			if r.Method == http.MethodPut {
				return tc.code, tc.body
			}
			return http.StatusOK, view
		}, "remote", "price-feeds", "declare", "pliage", "--file", doc)
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(tc.body), &refusal)
		if err == nil || !strings.Contains(err.Error(), refusal.Error) {
			t.Fatalf("HTTP %d: err %v, want the server's words %q\n%s", tc.code, err, refusal.Error, out)
		}
		if len(got) != 2 || !strings.HasPrefix(got[1], "PUT /api/teams/team-a/price-feeds ") || !strings.Contains(got[1], `"expected_updated_at":"2026-11-12T08:00:00Z"`) {
			t.Fatalf("the write is not a CAS on the stamp read: %q", got)
		}
	}
}
