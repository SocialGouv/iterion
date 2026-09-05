package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The hold-label veto is the operator's automation brake: pullRequestHoldLabel
// reads the labels of a PULL REQUEST through forge.IssueClient.GetIssue, and
// both the gate-autofix lane (forge_gate_autofix.go:282) and the gate-relaunch
// lane (forge_gate_relaunch.go:178) refuse to launch when the read fails —
// a veto that cannot be evaluated has not been cleared.
//
// On a github_app connection that read rides a MINTED token, so its permission
// set decides whether the brake works at all. GitHub gates a PR read on
// `pull_requests` even though the path is the issues one, so a token scoped to
// `issues` alone answers 404 and both lanes stop launching for the whole repo.
// This probe wires the real App path end to end and lets the fake forge apply
// exactly that rule, so what is asserted is the LANE working, not a map.

// prReadForge is a fake GitHub whose issue read obeys GitHub's own rule: the
// number is a pull request, so the call is refused unless the bearer's minted
// token carried the pull_requests grant.
type prReadForge struct {
	mu sync.Mutex
	// perms maps an issued token to the permission set it was minted with.
	perms map[string]map[string]string
	// refused counts reads rejected for want of the grant.
	refused int
	srv     *httptest.Server
}

func newPRReadForge(t *testing.T) *prReadForge {
	t.Helper()
	f := &prReadForge{perms: map[string]map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			var body struct {
				Permissions map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			tok := "ghs_" + strings.Join(sortedKeys(body.Permissions), "_")
			f.perms[tok] = body.Permissions
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": "2099-01-01T00:00:00Z"})
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		granted := f.perms[tok]
		_, canReadPulls := granted["pull_requests"]
		if !canReadPulls {
			f.refused++
		}
		f.mu.Unlock()
		if !canReadPulls {
			// GitHub hides what the token cannot see rather than saying why.
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 12, "title": "a pull request", "state": "open",
			"labels":       []map[string]any{{"name": "do-not-merge"}},
			"pull_request": map[string]any{"url": "https://gh/pulls/12"},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Deterministic token names; the order itself is irrelevant.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (f *prReadForge) refusedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refused
}

// A hold label on a PR is READ through a github_app connection — the lane's
// brake works. Before GetIssue carried the pull_requests grant this returned
// the forge's 404 and both lanes declined to launch, for every PR of every
// repo with hold_labels configured.
func TestPullRequestHoldLabelReadsThroughAGitHubAppConnection(t *testing.T) {
	s := newWebhookTestServer(t)
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	f := newPRReadForge(t)
	conn := forge.Connection{
		ID: "c-app", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime,
		InstallationID: 42, AppSlug: "iterion-forge-x", CreatedAt: time.Now().UTC(),
	}

	held, err := s.pullRequestHoldLabel(context.Background(), conn, "o/r", 12, []string{"do-not-merge"})
	if err != nil {
		t.Fatalf("the hold-label veto could not be evaluated on an App connection (%d read(s) refused for "+
			"want of pull_requests): %v — the autofix and gate-relaunch lanes both decline to launch on this", f.refusedCount(), err)
	}
	if held != "do-not-merge" {
		t.Errorf("held = %q, want the hold label the PR carries", held)
	}
}

// The other half of the veto: a PR without the label reads cleanly as "not
// held", so a working read is never mistaken for a brake.
func TestPullRequestHoldLabelAbsentOnAGitHubAppConnection(t *testing.T) {
	s := newWebhookTestServer(t)
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 42, PrivateKey: testAppKeyPEM(t), AppSlug: "iterion-forge-x"}
	f := newPRReadForge(t)
	conn := forge.Connection{
		ID: "c-app", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime,
		InstallationID: 42, AppSlug: "iterion-forge-x", CreatedAt: time.Now().UTC(),
	}

	held, err := s.pullRequestHoldLabel(context.Background(), conn, "o/r", 12, []string{"wip", "blocked"})
	if err != nil {
		t.Fatalf("pullRequestHoldLabel: %v", err)
	}
	if held != "" {
		t.Errorf("held = %q, want empty — the PR carries none of the configured hold labels", held)
	}
}
