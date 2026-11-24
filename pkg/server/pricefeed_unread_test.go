package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The fold a launch or a resume seals reads every level it folds, or the
// run is refused: a level the server cannot read is an answer it does not
// know, and on a default runner a missing level's accepted efforts send a
// level the gateway refuses (ADR-166 §4). Each level reads under its own
// deadline, and a declaration answers from its config without any state
// read. A level that holds no document, and a fetched feed that is stale or
// never refreshed, still step down: those answers are known.

// unreadConfigs fails the read of one config document with a database
// error, and delays the reads of others, honouring the read's deadline as
// the Mongo driver does.
type unreadConfigs struct {
	pricefeed.ConfigStore
	failDoc string
	delay   map[string]time.Duration
}

func (s unreadConfigs) Get(ctx context.Context, docID string) (*pricefeed.ConfigRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if docID == s.failDoc {
		return nil, errors.New("server selection error: context deadline exceeded, current topology: { Type: ReplicaSetNoPrimary, Servers: [{ Addr: mongo-0.mongo.internal:27017 }] }")
	}
	if d := s.delay[docID]; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.ConfigStore.Get(ctx, docID)
}

// countingStates fails every state read and counts them.
type countingStates struct {
	pricefeed.StateStore
	mu    sync.Mutex
	reads []string
}

func (s *countingStates) Get(_ context.Context, docID string) (*pricefeed.StateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, docID)
	return nil, errors.New("connection reset by peer")
}

func (s *countingStates) read() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reads...)
}

// unreadTestEnv is a default runner's environment: no operator table, no
// accepted-efforts table, no catalog provider.
func unreadTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ITERION_HOME", t.TempDir())
	for _, name := range []string{compatgw.CatalogModelsEnv, compatgw.ReasoningEffortsEnv, compatgw.CatalogProviderEnv, compatgw.FeedTableEnv, compatgw.ResolvedEnvName} {
		t.Setenv(name, "")
	}
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
}

// sealedEfforts answers the accepted efforts a default runner reads for
// wire from the table a bundle sealed, and their source.
func sealedEfforts(raw []byte, wire string) ([]string, string) {
	compatgw.ResetFeedTableCache()
	return compatgw.AcceptedEfforts(wire, func(name string) string {
		if name == compatgw.FeedTableEnv {
			return string(raw)
		}
		return ""
	}, func(string, ...any) {})
}

const unreadDeepseek = "scaleway/deepseek-v4.1-flash"

// A level whose config cannot be read refuses the launch, naming the level,
// and publishes nothing — never a table sealed without it.
func TestPriceFeedFoldRefusesALaunchOverAnUnreadLevel(t *testing.T) {
	unreadTestEnv(t)
	live := newPriceFeedLive(t)
	putFeedDoc(t, live.configs, pricefeed.PlatformConfigDocID, declaredFeed("pliage", retirePliage))
	launch := submitRetireLaunch(t, live, compileRetireBot(t), unreadConfigs{ConfigStore: live.configs, failDoc: pricefeed.PlatformConfigDocID}, live.states)
	if launch.err == nil || len(launch.published) != 0 {
		t.Fatalf("the launch was accepted (%d published, err %v) with the platform level unread", len(launch.published), launch.err)
	}
	var unread *PriceFeedUnreadError
	if !errors.As(launch.err, &unread) || !slices.Equal(unread.Levels, []string{"platform"}) || !unread.Retryable() {
		t.Fatalf("the refusal %v is not a retryable *PriceFeedUnreadError naming the platform", launch.err)
	}
	if strings.Contains(launch.err.Error(), "mongo-0") {
		t.Errorf("the refusal %q carries the read error, which names internal topology", launch.err)
	}
}

// The launch and resume answers carry the refusal as a retryable outage:
// 503, error_code PRICE_FEED_UNREAD and retryable (a 5xx body's message is
// the public one; the server log keeps the refusal's).
func TestPriceFeedUnreadLaunchAndResumeAnswerARetryableOutage(t *testing.T) {
	refusal := fmt.Errorf("cloudpublisher: the price-feed fold of team t1: %w", &PriceFeedUnreadError{Levels: []string{"org o1"}, Err: errors.New("org o1: context deadline exceeded")})
	check := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
		}
		var body struct {
			Error     string `json:"error"`
			ErrorCode string `json:"error_code"`
			Retryable bool   `json:"retryable"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		if body.ErrorCode != PriceFeedUnreadErrorCode || !body.Retryable {
			t.Errorf("answer = %+v, want PRICE_FEED_UNREAD and retryable", body)
		}
	}
	t.Run("resume", func(t *testing.T) {
		rec := httptest.NewRecorder()
		(&Server{}).writeResumeError(rec, httptest.NewRequest(http.MethodPost, "/api/runs/r1/resume", nil), refusal)
		check(t, rec)
	})
	t.Run("launch", func(t *testing.T) {
		srv := newQueueOutageHTTPTestServer(t)
		installQueueOutageTestPublisher(t, srv, refusal)
		body, err := json.Marshal(map[string]any{"file_path": "pf_unread.bot", "source": "workflow pf_unread:\n  entry: done\n", "run_id": "run-pf-unread-http"})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleLaunchRun(rec, req)
		check(t, rec)
	})
}

// A declaration answers from its config: no state document is read for it,
// so a state store that cannot answer leaves its efforts sealed. A disabled
// fetched feed reads no state either: the fold never reads it.
func TestPriceFeedFoldReadsNoStateForADeclarationOrADisabledFeed(t *testing.T) {
	unreadTestEnv(t)
	t.Run("a declaration", func(t *testing.T) {
		live := newPriceFeedLive(t)
		putFeedDoc(t, live.configs, pricefeed.PlatformConfigDocID, declaredFeed("pliage", retirePliage))
		states := &countingStates{StateStore: live.states}
		launch := submitRetireLaunch(t, live, compileRetireBot(t), live.configs, states)
		if launch.err != nil || len(launch.published) != 1 {
			t.Fatalf("the launch was refused (%d published): %v", len(launch.published), launch.err)
		}
		bundle := launch.open(t, launch.published[0])
		if got, src := sealedEfforts(bundle.GatewayPriceFeed, unreadDeepseek); src != "feed:pliage" || strings.Join(got, ",") != "low,high,xhigh,max" {
			t.Errorf("the declaration's efforts sealed %v from %q (table %s), want pliage's", got, src, bundle.GatewayPriceFeed)
		}
		if reads := states.read(); len(reads) != 0 {
			t.Errorf("the fold read the state of %v for a declaration", reads)
		}
	})
	t.Run("a disabled fetched feed", func(t *testing.T) {
		live := newPriceFeedLive(t)
		putFeedDoc(t, live.configs, pricefeed.PlatformConfigDocID, pricefeed.FeedConfig{
			Name: "lt", Type: pricefeed.TypeLiteLLM, Enabled: false,
			Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(time.Second),
			Params: map[string]string{"url": "https://gw.example/model/info"},
		})
		states := &countingStates{StateStore: live.states}
		launch := submitRetireLaunch(t, live, compileRetireBot(t), live.configs, states)
		if launch.err != nil || len(launch.published) != 1 {
			t.Fatalf("the launch was refused (%d published): %v", len(launch.published), launch.err)
		}
		if reads := states.read(); len(reads) != 0 {
			t.Errorf("the fold read the state of %v for a disabled feed", reads)
		}
	})
}

// Each level reads under its own deadline: two levels each slower than half
// of one deadline both answer, the team's declaration — read last — beside
// the platform's.
func TestPriceFeedFoldGivesEachLevelItsOwnReadDeadline(t *testing.T) {
	unreadTestEnv(t)
	live := newPriceFeedLive(t)
	putFeedDoc(t, live.configs, pricefeed.PlatformConfigDocID, declaredFeed("pliage", retirePliage))
	putFeedDoc(t, live.configs, pricefeed.TeamConfigDocID("team-pf"), declaredFeed("team-decl", `{"glm-5.3":{"reasoning_efforts":["high"]}}`))
	slow := unreadConfigs{ConfigStore: live.configs, delay: map[string]time.Duration{
		pricefeed.PlatformConfigDocID:        priceFeedReadTimeout * 6 / 10,
		pricefeed.TeamConfigDocID("team-pf"): priceFeedReadTimeout * 6 / 10,
	}}
	launch := submitRetireLaunch(t, live, compileRetireBot(t), slow, live.states)
	if launch.err != nil || len(launch.published) != 1 {
		t.Fatalf("the launch was refused (%d published): %v", len(launch.published), launch.err)
	}
	bundle := launch.open(t, launch.published[0])
	if _, src := sealedEfforts(bundle.GatewayPriceFeed, unreadDeepseek); src != "feed:pliage" {
		t.Errorf("the platform level is missing from the sealed table %s", bundle.GatewayPriceFeed)
	}
	if _, src := sealedEfforts(bundle.GatewayPriceFeed, "glm-5.3"); src != "feed:team-decl" {
		t.Errorf("the team level lost its read to the platform's share of one deadline: table %s", bundle.GatewayPriceFeed)
	}
}

// What the fold knows still steps down rather than refuse: a level with no
// document, and a fetched feed that never refreshed. With no feed anywhere
// the bundle carries no table, as with no feed store at all.
func TestPriceFeedFoldStepsDownWhatItKnows(t *testing.T) {
	unreadTestEnv(t)
	t.Run("no feed", func(t *testing.T) {
		live := newPriceFeedLive(t)
		launch := submitRetireLaunch(t, live, compileRetireBot(t), live.configs, live.states)
		if launch.err != nil || len(launch.published) != 1 {
			t.Fatalf("the launch was refused (%d published): %v", len(launch.published), launch.err)
		}
		if raw := launch.open(t, launch.published[0]).GatewayPriceFeed; raw != nil {
			t.Errorf("a deployment with no feed sealed a table: %s", raw)
		}
	})
	t.Run("a feed that never refreshed", func(t *testing.T) {
		live := newPriceFeedLive(t)
		putFeedDoc(t, live.configs, pricefeed.TeamConfigDocID("team-pf"), pricefeed.FeedConfig{
			Name: "lt", Type: pricefeed.TypeLiteLLM, Enabled: true,
			Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(time.Second),
			Params: map[string]string{"url": "https://gw.example/model/info"},
		})
		launch := submitRetireLaunch(t, live, compileRetireBot(t), live.configs, live.states)
		if launch.err != nil || len(launch.published) != 1 {
			t.Fatalf("a feed with no state refused the launch (%d published): %v", len(launch.published), launch.err)
		}
		if raw := launch.open(t, launch.published[0]).GatewayPriceFeed; raw != nil {
			t.Errorf("a feed with no state sealed a table: %s", raw)
		}
	})
}

// mustTable answers a fold's table, failing the test on a refused fold.
func mustTable(t *testing.T) func(json.RawMessage, error) json.RawMessage {
	t.Helper()
	return func(raw json.RawMessage, err error) json.RawMessage {
		t.Helper()
		if err != nil {
			t.Fatalf("the fold refused: %v", err)
		}
		return raw
	}
}
