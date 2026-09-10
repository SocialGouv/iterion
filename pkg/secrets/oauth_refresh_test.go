package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// freshRetrySchedule swaps the package-level retry schedule with a
// zero-delay one for the duration of a test. Returns a cleanup that
// restores the original schedule.
func freshRetrySchedule(t *testing.T) {
	t.Helper()
	orig := refreshRetrySchedule
	refreshRetrySchedule = []time.Duration{0, 0, 0}
	t.Cleanup(func() { refreshRetrySchedule = orig })
}

// readFormBody reads and parses a form-encoded request body, returning
// the parsed values. Helpful for asserting what the refresh client sent.
func readFormBody(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	out := map[string]string{}
	for _, kv := range strings.Split(string(body), "&") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}

// fakeOAuthServer spins up an httptest.Server that returns a single
// canned response body + status code for /oauth/token POSTs. Used by
// every test below; the per-test url override is via the consts that
// would normally be hard-coded but we just monkey-patch via a wrapped
// http.Client that rewrites the request URL.
type fakeOAuthServer struct {
	*httptest.Server
	hits int32
}

func newFakeOAuthServer(body string, status int) *fakeOAuthServer {
	f := &fakeOAuthServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.hits, 1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return f
}

// redirectingClient produces an *http.Client whose transport rewrites
// every request URL to point at `target` (preserving the path/query).
// Used so we can drive RefreshAnthropic/RefreshCodex (which hard-code
// production URLs) at a local httptest server.
type redirectingTransport struct {
	target string
	base   http.RoundTripper
}

func (rt *redirectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target := rt.target
	clone := req.Clone(req.Context())
	clone.URL, _ = clone.URL.Parse(target)
	clone.Host = ""
	return rt.base.RoundTrip(clone)
}

func redirectingClient(target string) *http.Client {
	return &http.Client{Transport: &redirectingTransport{target: target, base: http.DefaultTransport}}
}

// -----------------------------------------------------------------
// RefreshAnthropic
// -----------------------------------------------------------------

func TestRefreshAnthropic_HappyPath(t *testing.T) {
	freshRetrySchedule(t)
	body := `{"access_token":"sk-ant-newaccess1234567890abcdef","refresh_token":"rf-new","expires_in":3600,"scope":"read write","token_type":"Bearer"}`
	srv := newFakeOAuthServer(body, http.StatusOK)
	defer srv.Close()

	res, err := RefreshAnthropic(context.Background(), redirectingClient(srv.URL), "client-id", "rf-old")
	if err != nil {
		t.Fatalf("RefreshAnthropic: %v", err)
	}
	if res.AccessToken != "sk-ant-newaccess1234567890abcdef" {
		t.Errorf("access token: got %q", res.AccessToken)
	}
	if res.RefreshToken != "rf-new" {
		t.Errorf("refresh token: got %q", res.RefreshToken)
	}
	if len(res.Scopes) != 2 || res.Scopes[0] != "read" {
		t.Errorf("scopes: got %v", res.Scopes)
	}
	if res.ExpiresAt.IsZero() || time.Until(res.ExpiresAt) > time.Hour+time.Minute {
		t.Errorf("expires_at: got %v", res.ExpiresAt)
	}
}

func TestRefreshAnthropic_KeepsOldRefreshTokenWhenServerOmits(t *testing.T) {
	freshRetrySchedule(t)
	// Servers commonly omit refresh_token on refresh, expecting the
	// caller to keep using the old one.
	body := `{"access_token":"sk-ant-newaccess1234567890abcdef","expires_in":3600}`
	srv := newFakeOAuthServer(body, http.StatusOK)
	defer srv.Close()

	res, err := RefreshAnthropic(context.Background(), redirectingClient(srv.URL), "client-id", "rf-old-keep")
	if err != nil {
		t.Fatalf("RefreshAnthropic: %v", err)
	}
	if res.RefreshToken != "rf-old-keep" {
		t.Errorf("expected old refresh token to be preserved, got %q", res.RefreshToken)
	}
}

func TestRefreshAnthropic_MissingArgs(t *testing.T) {
	cases := []struct{ clientID, refreshToken string }{
		{"", "rf"},
		{"cid", ""},
		{"", ""},
	}
	for _, tc := range cases {
		_, err := RefreshAnthropic(context.Background(), nil, tc.clientID, tc.refreshToken)
		if err == nil {
			t.Errorf("expected error for clientID=%q refresh=%q", tc.clientID, tc.refreshToken)
		}
	}
}

func TestRefreshAnthropic_4xxIsTerminal(t *testing.T) {
	freshRetrySchedule(t)
	srv := newFakeOAuthServer(`{"error":"invalid_grant"}`, http.StatusBadRequest)
	defer srv.Close()

	_, err := RefreshAnthropic(context.Background(), redirectingClient(srv.URL), "cid", "rf")
	if err == nil {
		t.Fatal("expected error on 400")
	}
	// 400 must not retry — single hit.
	if got := atomic.LoadInt32(&srv.hits); got != 1 {
		t.Errorf("4xx should not retry; hits=%d", got)
	}
}

func TestRefreshAnthropic_5xxRetries(t *testing.T) {
	freshRetrySchedule(t)
	srv := newFakeOAuthServer(`{"error":"upstream"}`, http.StatusBadGateway)
	defer srv.Close()

	_, err := RefreshAnthropic(context.Background(), redirectingClient(srv.URL), "cid", "rf")
	if err == nil {
		t.Fatal("expected error after retry exhaustion")
	}
	if got := atomic.LoadInt32(&srv.hits); got != int32(len(refreshRetrySchedule)) {
		t.Errorf("5xx should retry; hits=%d, want %d", got, len(refreshRetrySchedule))
	}
}

func TestRefreshAnthropic_ImplausiblyShortToken(t *testing.T) {
	freshRetrySchedule(t)
	srv := newFakeOAuthServer(`{"access_token":"tiny","expires_in":3600}`, http.StatusOK)
	defer srv.Close()

	_, err := RefreshAnthropic(context.Background(), redirectingClient(srv.URL), "cid", "rf")
	if err == nil {
		t.Fatal("expected error for short access_token")
	}
	if !strings.Contains(err.Error(), "implausibly short") {
		t.Errorf("expected 'implausibly short' in err, got: %v", err)
	}
}

func TestRefreshAnthropic_EmptyToken(t *testing.T) {
	freshRetrySchedule(t)
	srv := newFakeOAuthServer(`{"access_token":"","expires_in":3600}`, http.StatusOK)
	defer srv.Close()

	_, err := RefreshAnthropic(context.Background(), redirectingClient(srv.URL), "cid", "rf")
	if err == nil {
		t.Fatal("expected error for empty access_token")
	}
}

func TestRefreshAnthropic_ContextCancellationStopsRetries(t *testing.T) {
	freshRetrySchedule(t)
	// 5xx forces retry; the ctx cancellation must stop the loop.
	srv := newFakeOAuthServer(`{}`, http.StatusInternalServerError)
	defer srv.Close()
	refreshRetrySchedule = []time.Duration{0, 200 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := RefreshAnthropic(ctx, redirectingClient(srv.URL), "cid", "rf")
	if err == nil {
		t.Fatal("expected error on context cancel")
	}
}

func TestRefreshAnthropic_PostsCorrectForm(t *testing.T) {
	freshRetrySchedule(t)
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = readFormBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"access_token":"sk-ant-validacctok1234567890abc","expires_in":3600}`)
	}))
	defer srv.Close()

	_, err := RefreshAnthropic(context.Background(), redirectingClient(srv.URL), "cid", "rf-x")
	if err != nil {
		t.Fatalf("RefreshAnthropic: %v", err)
	}
	if got["grant_type"] != "refresh_token" {
		t.Errorf("grant_type: got %q", got["grant_type"])
	}
	if got["client_id"] != "cid" {
		t.Errorf("client_id: got %q", got["client_id"])
	}
	if got["refresh_token"] != "rf-x" {
		t.Errorf("refresh_token: got %q", got["refresh_token"])
	}
}

// -----------------------------------------------------------------
// RefreshCodex
// -----------------------------------------------------------------

func TestRefreshCodex_HappyPath(t *testing.T) {
	freshRetrySchedule(t)
	body := `{"access_token":"oa-newaccess123456789abcdef","refresh_token":"rfc-new","id_token":"idtoken","expires_in":900,"scope":"openid"}`
	srv := newFakeOAuthServer(body, http.StatusOK)
	defer srv.Close()

	res, err := RefreshCodex(context.Background(), redirectingClient(srv.URL), "cli-cid", "rfc-old")
	if err != nil {
		t.Fatalf("RefreshCodex: %v", err)
	}
	if res.AccessToken == "" || res.RefreshToken != "rfc-new" || res.IDToken != "idtoken" {
		t.Errorf("fields: %+v", res)
	}
}

func TestRefreshCodex_MissingArgs(t *testing.T) {
	_, err := RefreshCodex(context.Background(), nil, "", "rfc")
	if err == nil {
		t.Error("expected err when client_id empty")
	}
	_, err = RefreshCodex(context.Background(), nil, "cid", "")
	if err == nil {
		t.Error("expected err when refresh empty")
	}
}

// -----------------------------------------------------------------
// ApplyAnthropicRefresh / ApplyCodexRefresh
// -----------------------------------------------------------------

func TestApplyAnthropicRefresh_PreservesOuterAndMergesInner(t *testing.T) {
	original := []byte(`{"other":"untouched","claudeAiOauth":{"keep":"yes","accessToken":"old"}}`)
	res := RefreshResult{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		ExpiresAt:    time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
		Scopes:       []string{"a", "b"},
	}
	got, err := ApplyAnthropicRefresh(original, res)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var roundtrip map[string]any
	if err := json.Unmarshal(got, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundtrip["other"] != "untouched" {
		t.Errorf("outer field stripped: %v", roundtrip)
	}
	inner := roundtrip["claudeAiOauth"].(map[string]any)
	if inner["keep"] != "yes" {
		t.Errorf("inner sibling stripped: %v", inner)
	}
	if inner["accessToken"] != "new-access" {
		t.Errorf("accessToken not updated: %v", inner["accessToken"])
	}
	if inner["refreshToken"] != "new-refresh" {
		t.Errorf("refreshToken not updated: %v", inner["refreshToken"])
	}
	if int64(inner["expiresAt"].(float64)) != res.ExpiresAt.UnixMilli() {
		t.Errorf("expiresAt: %v", inner["expiresAt"])
	}
}

func TestApplyAnthropicRefresh_SeedsInnerWhenMissing(t *testing.T) {
	original := []byte(`{"other":"x"}`)
	res := RefreshResult{AccessToken: "a"}
	got, err := ApplyAnthropicRefresh(original, res)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(string(got), `"accessToken": "a"`) {
		t.Errorf("expected accessToken in output: %s", got)
	}
}

func TestApplyAnthropicRefresh_RejectsMalformed(t *testing.T) {
	_, err := ApplyAnthropicRefresh([]byte("not json"), RefreshResult{AccessToken: "a"})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestApplyRefresh_NullPayloadDoesNotPanic pins the hardening for a
// `null` (or empty) credentials blob: json.Unmarshal leaves the target
// map nil without an error, so the subsequent map writes would panic
// ("assignment to entry in nil map"). Both Apply* funcs must instead
// surface a graceful error.
func TestApplyRefresh_NullPayloadDoesNotPanic(t *testing.T) {
	res := RefreshResult{AccessToken: "sk-ant-validacctok1234567890abc"}
	for _, payload := range [][]byte{[]byte("null"), {}, []byte("  null  ")} {
		if _, err := ApplyAnthropicRefresh(payload, res); err == nil {
			t.Errorf("ApplyAnthropicRefresh(%q): expected error, got nil", payload)
		}
		if _, err := ApplyCodexRefresh(payload, res); err == nil {
			t.Errorf("ApplyCodexRefresh(%q): expected error, got nil", payload)
		}
	}
}

func TestApplyCodexRefresh_PreservesOuterAndStampsTime(t *testing.T) {
	original := []byte(`{"meta":"keep","tokens":{"access_token":"old","other":"untouched"}}`)
	res := RefreshResult{
		AccessToken:  "new-access",
		RefreshToken: "new-rf",
		IDToken:      "new-id",
	}
	got, err := ApplyCodexRefresh(original, res)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var roundtrip map[string]any
	if err := json.Unmarshal(got, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundtrip["meta"] != "keep" {
		t.Error("outer field stripped")
	}
	tokens := roundtrip["tokens"].(map[string]any)
	if tokens["access_token"] != "new-access" || tokens["refresh_token"] != "new-rf" || tokens["id_token"] != "new-id" {
		t.Errorf("tokens not merged: %v", tokens)
	}
	if tokens["other"] != "untouched" {
		t.Error("sibling token field stripped")
	}
	if _, ok := roundtrip["last_refresh"].(string); !ok {
		t.Error("last_refresh not stamped")
	}
}

// -----------------------------------------------------------------
// validateAccessToken (pure)
// -----------------------------------------------------------------

func TestValidateAccessToken(t *testing.T) {
	cases := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{"empty", "", true},
		{"too short (15 bytes)", "abc123abc123abc", true},
		{"min length (16 bytes)", "abc123abc123abcd", false},
		{"realistic", "sk-ant-realisticlongerexample-token", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAccessToken("test", tc.token)
			if (err != nil) != tc.wantErr {
				t.Errorf("got err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// -----------------------------------------------------------------
// doWithRetry resource handling
// -----------------------------------------------------------------

// trackingBody records whether Close was called so tests can assert
// doWithRetry releases the body of every response it abandons.
type trackingBody struct {
	*strings.Reader
	closed *int32
}

func (b trackingBody) Close() error {
	atomic.StoreInt32(b.closed, 1)
	return nil
}

func cannedResp(status int, body string, closedFlag *int32) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       trackingBody{Reader: strings.NewReader(body), closed: closedFlag},
		Header:     make(http.Header),
	}
}

// sequencedTransport hands back queued responses in order, one per
// RoundTrip. doWithRetry calls hc.Do sequentially, so no locking needed.
type sequencedTransport struct {
	responses []*http.Response
	idx       int
}

func (t *sequencedTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	if t.idx >= len(t.responses) {
		return nil, fmt.Errorf("sequencedTransport: unexpected call %d", t.idx)
	}
	resp := t.responses[t.idx]
	t.idx++
	return resp, nil
}

// TestDoWithRetry_ClosesAbandoned5xxBodyOnRetrySuccess pins the fix for
// the connection/fd leak where a 5xx attempt's response body was never
// closed once a subsequent retry succeeded. Without the fix the first
// (502) body stays open, pinning its TCP connection until GC.
func TestDoWithRetry_ClosesAbandoned5xxBodyOnRetrySuccess(t *testing.T) {
	freshRetrySchedule(t)
	var firstClosed, secondClosed int32
	okBody := `{"access_token":"sk-ant-validacctok1234567890abc","expires_in":3600}`
	hc := &http.Client{Transport: &sequencedTransport{responses: []*http.Response{
		cannedResp(http.StatusBadGateway, `{"error":"upstream"}`, &firstClosed),
		cannedResp(http.StatusOK, okBody, &secondClosed),
	}}}

	if _, err := RefreshAnthropic(context.Background(), hc, "cid", "rf"); err != nil {
		t.Fatalf("RefreshAnthropic: %v", err)
	}
	if atomic.LoadInt32(&firstClosed) != 1 {
		t.Error("abandoned 5xx response body was not closed (connection/fd leak)")
	}
	if atomic.LoadInt32(&secondClosed) != 1 {
		t.Error("successful response body was not closed")
	}
}

// -----------------------------------------------------------------
// Token-endpoint env override
// -----------------------------------------------------------------

// hostPinnedTransport refuses any request aimed somewhere other than
// want, so a test asserting that an endpoint override reaches a local
// server never leaves the machine when the override is not honoured: it
// fails naming the host that was actually dialled.
type hostPinnedTransport struct {
	want string
	base http.RoundTripper
}

func (t *hostPinnedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != t.want {
		return nil, fmt.Errorf("request went to %q, want the overridden endpoint %q", req.URL.Host, t.want)
	}
	return t.base.RoundTrip(req)
}

// pinnedClient dials host and nothing else.
func pinnedClient(t *testing.T, rawURL string) *http.Client {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return &http.Client{Transport: &hostPinnedTransport{want: u.Host, base: http.DefaultTransport}}
}

// The three sibling Anthropic endpoints (authorize URL, redirect URI,
// scopes) are env-overridable; the token endpoint the refresh POSTs to
// must be too, or an OEM-repackaged CLI moves three of four and the
// refresh worker keeps hitting the vendor's host.
func TestRefreshAnthropic_HonoursTokenURLOverride(t *testing.T) {
	freshRetrySchedule(t)
	body := `{"access_token":"sk-ant-newaccess1234567890abcdef","refresh_token":"rf-new","expires_in":3600}`
	srv := newFakeOAuthServer(body, http.StatusOK)
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL", srv.URL+"/v1/oauth/token")

	if _, err := RefreshAnthropic(context.Background(), pinnedClient(t, srv.URL), "client-id", "rf-old"); err != nil {
		t.Fatalf("RefreshAnthropic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.hits); got != 1 {
		t.Fatalf("overridden endpoint hits = %d, want 1", got)
	}
}

// Same promise for the Codex token endpoint, which shares the comment.
func TestRefreshCodex_HonoursTokenURLOverride(t *testing.T) {
	freshRetrySchedule(t)
	body := `{"access_token":"codex-newaccess1234567890","refresh_token":"rf-new","expires_in":3600}`
	srv := newFakeOAuthServer(body, http.StatusOK)
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL", srv.URL+"/oauth/token")

	if _, err := RefreshCodex(context.Background(), pinnedClient(t, srv.URL), "client-id", "rf-old"); err != nil {
		t.Fatalf("RefreshCodex: %v", err)
	}
	if got := atomic.LoadInt32(&srv.hits); got != 1 {
		t.Fatalf("overridden endpoint hits = %d, want 1", got)
	}
}

// Unset env keeps the published endpoint: the override is opt-in.
func TestTokenURLs_DefaultToThePublishedEndpoints(t *testing.T) {
	t.Setenv("ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL", "")
	t.Setenv("ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL", "")
	if got := anthropicTokenURL(); got != defaultAnthropicTokenURL {
		t.Errorf("anthropicTokenURL() = %q, want %q", got, defaultAnthropicTokenURL)
	}
	if got := codexTokenURL(); got != defaultCodexTokenURL {
		t.Errorf("codexTokenURL() = %q, want %q", got, defaultCodexTokenURL)
	}
}
