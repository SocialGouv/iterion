package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

type labelRecorder func(*http.Request) (*http.Response, error)

func (f labelRecorder) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Every call the App client sends with one of the installation's tokens — the
// management token (rest), a scoped profile minted and then served from its
// cache (scopedREST, the board's GraphQL), the one-call administration token
// (CreateRepo) — leaves labelled with the installation, so the transport
// below can charge that installation's budget. The mints, signed with the
// App's JWT, spend no installation budget and leave unlabelled.
func TestAppClient_LabelsEveryInstallationTokenRequest(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/99/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_inst", "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []any{}})
	})
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("/api/v3/orgs/octo/repos", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "octo/new"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var (
		mu   sync.Mutex
		sent []string
	)
	app := &AppClient{
		HTTP: &http.Client{Transport: labelRecorder(func(r *http.Request) (*http.Response, error) {
			id, ok := forge.InstallationOf(r.Context())
			label := "unlabelled"
			if ok {
				label = fmt.Sprintf("installation %d", id)
			}
			mu.Lock()
			sent = append(sent, fmt.Sprintf("%s %s %s (%s)", r.Method, r.URL.Path, label, strings.SplitN(r.Header.Get("Authorization"), ".", 2)[0]))
			mu.Unlock()
			return http.DefaultTransport.RoundTrip(r)
		})},
		WebBaseURL: srv.URL,
		Cfg:        AppConfig{AppID: 42, PrivateKeyPEM: pemStr, AppSlug: "iterion"}, InstallationID: 99,
	}
	ctx := context.Background()
	if _, err := app.ListRepos(ctx, forge.RepoQuery{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		var out struct{}
		if err := app.GraphQL(ctx, "query{viewer{login}}", nil, &out); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.CreateRepo(ctx, forge.RepoCreateSpec{Owner: "octo", Name: "new"}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	var mints, calls int
	for _, s := range sent {
		if strings.Contains(s, "/access_tokens ") {
			mints++
			if !strings.Contains(s, " unlabelled ") {
				t.Errorf("%s: a mint is signed with the App's JWT and spends no installation budget", s)
			}
			continue
		}
		calls++
		if !strings.HasSuffix(s, " installation 99 (Bearer ghs_inst)") {
			t.Errorf("%s: sent with installation 99's token, it must be labelled with installation 99", s)
		}
	}
	if mints != 3 || calls != 4 {
		t.Errorf("saw %d mints and %d token calls, want 3 and 4 — every token client exercised:\n%s", mints, calls, strings.Join(sent, "\n"))
	}
}
