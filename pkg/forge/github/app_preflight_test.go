package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// probeReached is a stand-in forge that records whether it was contacted —
// the oracle for "before any socket", measured instead of asserted.
//
// The flag is atomic because the handler that sets it runs on the server's
// own goroutine while the test reads it on its. A plain bool is a data race
// the moment a regression DOES open a socket — the one run where this
// oracle has something to say — and the read may then miss the write and
// let the test report success on the claim it exists to measure.
type probeReached struct{ hit atomic.Bool }

// server answers everything with status/body, recording that it was asked.
func (p *probeReached) server(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hit.Store(true)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

// A stored App key that is not parseable PEM fails while the JWT is being
// signed — before a socket is opened. The assertion that matters is not the
// error text but that the forge was never contacted AND that the error says
// so: every handler downstream defaults to 502, so an unmarked failure here
// reports GitHub as broken for a key only iterion can read.
func TestMintInstallationToken_UnparseableKeyNeverReachesTheForge(t *testing.T) {
	var probe probeReached
	srv := probe.server(http.StatusTeapot, "")
	defer srv.Close()

	cfg := AppConfig{AppID: 42, PrivateKeyPEM: "-----BEGIN RSA PRIVATE KEY-----\nnot base64\n-----END RSA PRIVATE KEY-----", AppSlug: "iterion"}
	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, cfg, 99, time.Unix(1700000000, 0), nil)
	if err == nil {
		t.Fatal("an unparseable private key minted a token")
	}
	if probe.hit.Load() {
		t.Fatal("the mint opened a socket with a key it could not read — the pre-flight claim this marker rests on is false")
	}
	if !errors.Is(err, forge.ErrLocalPreflight) {
		t.Fatalf("mint error %v carries no forge.ErrLocalPreflight — a 502-defaulting handler will blame the forge for a key iterion stored", err)
	}
}

// The other direction, which is the way this fix could itself become a lie:
// a genuine refusal from the forge must NOT be marked, or a GitHub outage
// would be reported as iterion's own fault.
func TestMintInstallationToken_ForgeRefusalIsNotMarkedLocal(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	var probe probeReached
	srv := probe.server(http.StatusBadGateway, `{"message":"upstream is down"}`)
	defer srv.Close()

	cfg := AppConfig{AppID: 42, PrivateKeyPEM: pemStr, AppSlug: "iterion"}
	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, cfg, 99, time.Unix(1700000000, 0), nil)
	if err == nil {
		t.Fatal("a 502 from the forge minted a token")
	}
	if !probe.hit.Load() {
		t.Fatal("the forge was never asked — this test's premise is a refusal that came back over the wire, and the probe is what tells the two apart")
	}
	if errors.Is(err, forge.ErrLocalPreflight) {
		t.Fatalf("a forge 5xx was marked as iterion's own (%v) — the same inversion, reversed", err)
	}
}

// The handlers this marker exists for never call MintInstallationToken: they
// hold an *AppClient and reach the mint through rest (ListRepos, ListHooks…)
// or scopedREST (GetPullRequest, the issue profiles), each of which returns
// the mint error as it received it. That unwrapped return is the whole load
// of the fix — one fmt.Errorf("mint: %v", err) at either site would flatten
// the sentinel and put the 502 inversion back with every other test in this
// suite still green. Pinned here, at the two functions where such a re-wrap
// would be written.
func TestAppClientMintChain_PreservesLocalPreflight(t *testing.T) {
	cases := []struct {
		name string
		call func(context.Context, *AppClient) error
	}{
		{"rest", func(ctx context.Context, a *AppClient) error {
			_, err := a.rest(ctx)
			return err
		}},
		{"scopedREST", func(ctx context.Context, a *AppClient) error {
			_, err := a.scopedREST(ctx, map[string]string{"pull_requests": "read"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var probe probeReached
			srv := probe.server(http.StatusTeapot, "")
			defer srv.Close()

			a := &AppClient{
				HTTP: srv.Client(), WebBaseURL: srv.URL,
				Cfg:            AppConfig{AppID: 42, PrivateKeyPEM: "-----BEGIN RSA PRIVATE KEY-----\nnot base64\n-----END RSA PRIVATE KEY-----", AppSlug: "iterion"},
				InstallationID: 99,
				Now:            func() time.Time { return time.Unix(1700000000, 0).UTC() },
			}
			err := tc.call(context.Background(), a)
			if err == nil {
				t.Fatalf("%s served a client for a key that cannot be read", tc.name)
			}
			if probe.hit.Load() {
				t.Fatalf("%s opened a socket with a key it could not read", tc.name)
			}
			if !errors.Is(err, forge.ErrLocalPreflight) {
				t.Fatalf("%s returned %v with no forge.ErrLocalPreflight — the mint marks it, and this is where the mark is dropped; the route above answers 502 for a key iterion stored", tc.name, err)
			}
		})
	}
}
