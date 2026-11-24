package pricefeed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

func stubHTTP(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	prev := httpClient
	server := httptest.NewServer(handler)
	httpClient = server.Client()
	t.Cleanup(func() {
		httpClient = prev
		server.Close()
	})
	t.Setenv("PRICEFEED_TEST_URL", server.URL)
}

// The modelsdev adapter reads the aggregator's own shape and answers BOTH
// spellings of a provider's ids — the bare name a bare-id gateway serves,
// the qualified key a provider-first gateway serves. Neither entry may
// attach another provider's numbers.
func TestModelsDevSourceFetchesProvider(t *testing.T) {
	stubHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"scaleway": {"models": {
				"glm-5.3": {"limit": {"context": 131072, "output": 8192}, "cost": {"input": 0.6, "output": 1.8}, "tool_call": true},
				"gpt-oss-120b": {"limit": {"context": 131072}, "cost": {"input": 0.2, "output": 0.6}}
			}},
			"openai": {"models": {"gpt-5": {"cost": {"input": 9, "output": 9}}}}
		}`))
	})
	src, ok := SourceFor(TypeModelsDev)
	if !ok {
		t.Fatal("modelsdev not registered")
	}
	table, err := src.Fetch(context.Background(), map[string]string{"provider": "scaleway", "url": os.Getenv("PRICEFEED_TEST_URL")})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"glm-5.3", "scaleway/glm-5.3", "gpt-oss-120b", "scaleway/gpt-oss-120b"} {
		if _, ok := table[id]; !ok {
			t.Errorf("id %q missing — both spellings fold", id)
		}
	}
	if _, wrong := table["gpt-5"]; wrong {
		t.Error("another provider's model leaked in")
	}
	if table["glm-5.3"].InputCostPerM != 0.6 || table["glm-5.3"].ContextWindow != 131072 {
		t.Errorf("entry decoded wrong: %+v", table["glm-5.3"])
	}
	if table["glm-5.3"].ToolCall == nil || !*table["glm-5.3"].ToolCall {
		t.Errorf("a stated tool_call flag did not survive: %+v", table["glm-5.3"])
	}
	if err := src.ValidateParams(map[string]string{}); err == nil {
		t.Error("modelsdev without a provider validated")
	}
}

// scaleway is the modelsdev adapter with its qualifier pinned: the same
// parser, the same spellings, one less knob.
func TestScalewaySourceIsThePinnedPreset(t *testing.T) {
	stubHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"scaleway":{"models":{"glm-5.3":{"cost":{"input":0.6,"output":1.8}}}}}`))
	})
	src, _ := SourceFor(TypeScaleway)
	if err := src.ValidateParams(map[string]string{}); err != nil {
		t.Fatalf("the preset needs no params: %v", err)
	}
	table, err := src.Fetch(context.Background(), map[string]string{"url": os.Getenv("PRICEFEED_TEST_URL")})
	if err != nil {
		t.Fatal(err)
	}
	if table["glm-5.3"].InputCostPerM != 0.6 {
		t.Fatalf("preset did not read the scaleway provider: %+v", table)
	}
}

// The litellm adapter scales per-token costs to per-mtok once, and keeps
// the window figures the proxy publishes.
func TestLiteLLMSourceScalesPerToken(t *testing.T) {
	var gotAuth string
	stubHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[
			{"model_name":"scaleway/glm-5.3","model_info":{"max_tokens":131072,"max_output_tokens":8192,"input_cost_per_token":6.1e-7,"output_cost_per_token":1.8e-6}},
			{"model_name":"no-info"}
		]}`))
	})
	src, _ := SourceFor(TypeLiteLLM)
	if err := src.ValidateParams(map[string]string{}); err == nil {
		t.Error("litellm without a url validated")
	}
	table, err := src.Fetch(context.Background(), map[string]string{"url": os.Getenv("PRICEFEED_TEST_URL"), "token": " k3 "})
	if err != nil {
		t.Fatal(err)
	}
	e := table["scaleway/glm-5.3"]
	if e.InputCostPerM != 0.61 || e.OutputCostPerM < 1.79 || e.OutputCostPerM > 1.81 {
		t.Fatalf("per-token costs not scaled to per-mtok: %+v", e)
	}
	if e.ContextWindow != 131072 || e.MaxOutputTokens != 8192 {
		t.Fatalf("window figures lost: %+v", e)
	}
	if _, ok := table["no-info"]; ok {
		t.Error("an entry with no model_info folded")
	}
	if gotAuth != "Bearer k3" {
		t.Errorf("token not sent as a trimmed bearer: %q", gotAuth)
	}
}

// The exec adapter: an operator-supplied command printing the operator
// table's JSON on stdout — no shell, args split on spaces.
func TestExecSourceReadsStdout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "tariffs.sh")
	body := "#!/bin/sh\n[ \"$1\" = --bogus ] && exit 3\nprintf '%s' '{\"glm\":{\"input_usd_per_mtok\":0.6,\"output_usd_per_mtok\":1.8}}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	src, _ := SourceFor(TypeExec)
	if err := src.ValidateParams(map[string]string{}); err == nil {
		t.Error("exec without a command validated")
	}
	table, err := src.Fetch(context.Background(), map[string]string{"command": script})
	if err != nil {
		t.Fatal(err)
	}
	if table["glm"].InputCostPerM != 0.6 {
		t.Fatalf("stdout not parsed as the operator table: %+v", table)
	}
	if _, err := src.Fetch(context.Background(), map[string]string{"command": script, "args": "--bogus"}); err == nil {
		t.Error("a failing command fetched a table")
	}
	empty := filepath.Join(dir, "empty.sh")
	os.WriteFile(empty, []byte("#!/bin/sh\n"), 0o755)
	if _, err := src.Fetch(context.Background(), map[string]string{"command": empty}); err == nil {
		t.Error("an empty stdout fetched a table")
	}
}

func TestValidateFeedBounds(t *testing.T) {
	good := FeedConfig{Name: "scw", Type: TypeExec, Enabled: true, Interval: d(60), Timeout: d(10), Params: map[string]string{"command": "x"}}
	if err := ValidateFeed(good); err != nil {
		t.Fatalf("a valid feed refused: %v", err)
	}
	for _, mutate := range []func(*FeedConfig){
		func(f *FeedConfig) { f.Name = "Bad Name" },
		func(f *FeedConfig) { f.Type = "nope" },
		func(f *FeedConfig) { f.Interval = d(30) },
		func(f *FeedConfig) { f.Timeout = d(90) }, // > interval
		func(f *FeedConfig) { delete(f.Params, "command") },
	} {
		bad := good
		mutate(&bad)
		if err := ValidateFeed(bad); err == nil {
			t.Errorf("feed %+v validated", bad)
		}
	}
	if err := ValidateConfig(&ConfigRecord{Feeds: []FeedConfig{good, good}}); err == nil {
		t.Error("a duplicated feed name validated")
	}
}

// The view's redaction marker is never a param's value: a feed whose param
// holds it — a token, a url, any param — is refused, naming the feed and
// every param holding it, sorted. The same feed with the stored values
// validates.
func TestValidateFeedRefusesTheRedactionMarker(t *testing.T) {
	gw := FeedConfig{Name: "gw", Type: TypeLiteLLM, Enabled: true, Interval: d(3600), Timeout: d(10),
		Params: map[string]string{"url": "https://gw.example/model/info", "token": "sk-litellm-kept", "region": "fr-par"}}
	if err := ValidateFeed(gw); err != nil {
		t.Fatalf("the feed with its stored values refused: %v", err)
	}
	for _, tc := range []struct {
		params []string
		want   string
	}{
		{[]string{"token"}, `feed "gw": params token hold`},
		{[]string{"url"}, `feed "gw": params url hold`},
		{[]string{"token", "region"}, `feed "gw": params region, token hold`},
	} {
		bad := gw
		bad.Params = map[string]string{}
		for k, v := range gw.Params {
			bad.Params[k] = v
		}
		for _, p := range tc.params {
			bad.Params[p] = RedactedParamValue
		}
		err := ValidateFeed(bad)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), RedactedParamValue) {
			t.Errorf("params %v holding the marker: err %v, want a refusal containing %q and the marker", tc.params, err, tc.want)
		}
		if err := ValidateConfig(&ConfigRecord{Feeds: []FeedConfig{bad}}); err == nil {
			t.Errorf("params %v holding the marker: the list validated", tc.params)
		}
	}
}

func TestDueAndBackoff(t *testing.T) {
	now := time.Now()
	feed := FeedConfig{Name: "f", Type: TypeExec, Interval: d(3600), Timeout: d(10)}
	if !Due(feed, nil, now) {
		t.Fatal("a feed with no state is not due")
	}
	fresh := &StateRecord{LastAttempt: now.Add(-time.Minute)}
	if Due(feed, fresh, now) {
		t.Fatal("a feed refreshed a minute ago is due on an hour interval")
	}
	failing := &StateRecord{LastAttempt: now.Add(-3 * time.Minute), LastError: "boom"}
	if !Due(feed, failing, now) {
		t.Fatal("a failing feed waited its full interval — the backoff must retry sooner")
	}
}

// The drift report names every id where the fresh feed and the operator
// table disagree on price — the surfacing, never the resolution.
func TestDriftReport(t *testing.T) {
	feed := map[string]modelspecs.Spec{"a": {InputCostPerM: 1, OutputCostPerM: 2}, "b": {InputCostPerM: 3, OutputCostPerM: 4}, "feed-only": {InputCostPerM: 9, OutputCostPerM: 9}}
	operator := map[string]modelspecs.Spec{"a": {InputCostPerM: 1, OutputCostPerM: 2}, "b": {InputCostPerM: 3.5, OutputCostPerM: 4}, "op-only": {InputCostPerM: 8, OutputCostPerM: 8}}
	drift := DriftReport("scw", feed, operator)
	if len(drift) != 1 || drift[0].ID != "b" {
		t.Fatalf("only the price disagreement is drift, got %+v", drift)
	}
	if line := FormatDrift(drift[0]); !strings.Contains(line, "scw") || !strings.Contains(line, `"b"`) {
		t.Fatalf("the drift line does not name the feed and the id: %q", line)
	}
}

// Round 3's HIGH, witnessed: a descendant holding ONLY stdout (its stderr
// closed — `helper 2>/dev/null &`'s shape) does not park the fetch past
// the deadline; the fetch refuses by name instead.
func TestExecSource_DescendantHoldingStdoutIsBounded(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "leaky.sh")
	body := "#!/bin/sh\n" +
		"printf '%s' '{\"glm\":{\"input_usd_per_mtok\":0.6,\"output_usd_per_mtok\":1.8}}'\n" +
		"sh -c 'exec 2>&-; exec sleep 90' &\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	src, _ := SourceFor(TypeExec)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := src.Fetch(ctx, map[string]string{"command": script})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("the leaky fetch succeeded")
	}
	if !strings.Contains(err.Error(), "did not close within its bound") {
		t.Fatalf("the refusal does not name the held pipe: %v", err)
	}
	// ctx 2s + grace ~6s: bounded well under the sleeper's 90s.
	if elapsed > 30*time.Second {
		t.Fatalf("the fetch parked %v past its bound", elapsed)
	}
	t.Logf("bounded at %v: %v", elapsed, err)
}
