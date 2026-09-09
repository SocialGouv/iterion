package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReviewPRTicketFetch guards the deterministic node that reads the ticket(s)
// a PR claims to deliver.
//
// It exists because the fetch used to be a prose recipe the REVIEWER executed
// with the run's forge token in its prompt — a write-capable credential
// (contents/issues/pull_requests, see RuntimeInstallationPermissions) handed to
// an agent whose input includes every linked issue body, i.e. text an outside
// contributor writes. Moving the fetch into this node is the boundary; these
// are the properties that boundary has to keep, each one a way the prose recipe
// was wrong:
//
//   - GitHub Enterprise is addressed on ITS OWN host (/api/v3 + /api/graphql),
//     never api.github.com — an enterprise token must not leave the enterprise.
//   - GitLab is authenticated with `Authorization: Bearer`, the only header
//     that serves BOTH a PAT and an OAuth access token (pkg/forge/gitlab/
//     client.go); PRIVATE-TOKEN silently fails every OAuth connection.
//   - A ticket keeps its OWN repository identity, so `other/repo#1` is fetched
//     from other/repo and never collapses onto this repo's #1.
//   - A number that resolves to a PULL REQUEST is reported unverifiable, not
//     judged as if it were the ticket.
//   - A merely `mentioned` reference is marked as such (only a `closes` link
//     may become a blocking requirements finding).
//   - The credential never appears in the node's output.
func TestReviewPRTicketFetch(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	script := toolScript(t, "review-pr/main.bot", "ticket_fetch")

	const token = "ghs_secret_runtime_token"

	type result struct {
		Mode    string `json:"mode"`
		Tickets string `json:"tickets"`
		Status  string `json:"status"`
		Count   int    `json:"count"`
		Note    string `json:"note"`
	}

	// run substitutes the node's refs and executes the real script body.
	run := func(t *testing.T, refs map[string]string) result {
		t.Helper()
		body := script
		full := map[string]string{
			"{{vars.ticket_context}}":      `"auto"`,
			"{{vars.pr_url}}":              `""`,
			"{{vars.tracker_api_base}}":    `""`,
			"{{vars.ticket_refs}}":         `""`,
			"{{vars.source_branch}}":       `""`,
			"{{vars.scope_notes}}":         `""`,
			"{{secrets.forge_token.path}}": `""`,
		}
		for k, v := range refs {
			if _, ok := full[k]; !ok {
				t.Fatalf("unknown ref %s", k)
			}
			full[k] = v
		}
		for ref, val := range full {
			body = strings.ReplaceAll(body, ref, val)
		}
		if strings.Contains(body, "{{") {
			t.Fatalf("unsubstituted ref left in the script: %s", firstRef(body))
		}
		path := filepath.Join(t.TempDir(), "ticket_fetch.py")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("python3", path).Output()
		if err != nil {
			t.Fatalf("ticket_fetch failed: %v (out %q)", err, out)
		}
		var res result
		if uerr := json.Unmarshal(out, &res); uerr != nil {
			t.Fatalf("ticket_fetch output is not ticket_fetch_output JSON: %v (%q)", uerr, out)
		}
		if strings.Contains(string(out), token) {
			t.Errorf("the forge credential leaked into the node's output: %s", out)
		}
		return res
	}

	tokenFile := func(t *testing.T) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "forge_token")
		if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("github enterprise: own host, linked issues, cross-repo identity", func(t *testing.T) {
		var graphqlPath string
		var authSeen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authSeen = append(authSeen, r.Header.Get("Authorization"))
			if r.Header.Get("PRIVATE-TOKEN") != "" {
				t.Errorf("PRIVATE-TOKEN sent to a GitHub host")
			}
			switch {
			case r.URL.Path == "/api/graphql":
				graphqlPath = r.URL.Path
				var req struct {
					Variables map[string]any `json:"variables"`
				}
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &req)
				// Page 1 of 2 — pagination is what keeps a PR with >50 linked
				// issues from silently reviewing only the first page.
				if req.Variables["c"] == nil {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
						"pullRequest": map[string]any{"closingIssuesReferences": map[string]any{
							"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "cur1"},
							"nodes": []any{map[string]any{
								"number": 11, "title": "Export as CSV", "body": "the demand",
								"state": "OPEN", "url": "https://ghe.example.org/acme/widgets/issues/11",
								"repository": map[string]any{"nameWithOwner": "acme/widgets"},
							}},
						}},
					}}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
					"pullRequest": map[string]any{"closingIssuesReferences": map[string]any{
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						"nodes": []any{map[string]any{
							"number": 11, "title": "OTHER REPO ticket", "body": "elsewhere",
							"state": "OPEN", "url": "https://ghe.example.org/other/repo/issues/11",
							"repository": map[string]any{"nameWithOwner": "other/repo"},
						}},
					}},
				}}})
			case r.URL.Path == "/api/v3/repos/acme/widgets/pulls/7":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"title": "Deliver the export", "body": "Fixes #11. See also #42. Closes #43.",
				})
			case r.URL.Path == "/api/v3/repos/acme/widgets/issues/42":
				_ = json.NewEncoder(w).Encode(map[string]any{"title": "Mentioned only", "body": "context", "state": "open"})
			case r.URL.Path == "/api/v3/repos/acme/widgets/issues/43":
				// GitHub serves pull requests from /issues/<n> too.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"title": "a pull request", "body": "", "state": "open",
					"pull_request": map[string]any{"url": "https://ghe.example.org/api/v3/repos/acme/widgets/pulls/43"},
				})
			default:
				w.WriteHeader(404)
			}
		}))
		defer srv.Close()

		res := run(t, map[string]string{
			"{{vars.pr_url}}":              `"` + srv.URL + `/acme/widgets/pull/7"`,
			"{{secrets.forge_token.path}}": `"` + tokenFile(t) + `"`,
		})

		if res.Mode != "forge" {
			t.Fatalf("mode = %q, want forge (note %q)", res.Mode, res.Note)
		}
		if graphqlPath != "/api/graphql" {
			t.Errorf("GHE GraphQL must be <host>/api/graphql, got %q — api.github.com or <base>/graphql sends an enterprise token off-forge", graphqlPath)
		}
		for _, a := range authSeen {
			if a != "Bearer "+token {
				t.Errorf("Authorization = %q, want a Bearer of the forge token", a)
			}
		}
		if !strings.Contains(res.Tickets, "acme/widgets#11") || !strings.Contains(res.Tickets, "other/repo#11") {
			t.Errorf("both linked issues must survive with their own repo identity:\n%s", res.Tickets)
		}
		if !strings.Contains(res.Tickets, "OTHER REPO ticket") {
			t.Errorf("the cross-repo ticket collapsed onto this repo's #11:\n%s", res.Tickets)
		}
		if !strings.Contains(res.Status, "acme/widgets#43: unverifiable") ||
			!strings.Contains(res.Status, "pull request, not an issue") {
			t.Errorf("a PR number must be reported unverifiable, not judged as a ticket:\n%s", res.Status)
		}
		if !strings.Contains(res.Status, "acme/widgets#42: fetched (mentioned") {
			t.Errorf("a merely mentioned ref must be marked `mentioned` (only a closes link may block):\n%s", res.Status)
		}
		if !strings.Contains(res.Status, "acme/widgets#11: fetched (closes") {
			t.Errorf("a forge-linked issue must be marked `closes`:\n%s", res.Status)
		}
		if res.Count != 3 {
			t.Errorf("count = %d, want 3 readable tickets (#11 here, #11 elsewhere, #42)", res.Count)
		}
	})

	t.Run("gitlab: Bearer (never PRIVATE-TOKEN), closes_issues", func(t *testing.T) {
		var closesHit bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("PRIVATE-TOKEN") != "" {
				t.Errorf("PRIVATE-TOKEN rejects an OAuth connection's token — GitLab must be addressed with Bearer")
			}
			if got := r.Header.Get("Authorization"); got != "Bearer "+token {
				t.Errorf("Authorization = %q, want Bearer of the forge token", got)
			}
			// EscapedPath: Go decodes %2F in r.URL.Path, which would hide a
			// project path that was NOT url-encoded by the node.
			switch r.URL.EscapedPath() {
			case "/api/v4/projects/group%2Fsub%2Fproj/merge_requests/3/closes_issues":
				closesHit = true
				_ = json.NewEncoder(w).Encode([]any{map[string]any{
					"iid": 5, "title": "Ship the importer", "description": "the demand", "state": "opened",
					"web_url":    "https://gitlab.example.org/group/sub/proj/-/issues/5",
					"references": map[string]any{"full": "group/sub/proj#5"},
				}})
			case "/api/v4/projects/group%2Fsub%2Fproj/merge_requests/3":
				_ = json.NewEncoder(w).Encode(map[string]any{"title": "MR", "description": "Closes #5"})
			default:
				w.WriteHeader(404)
			}
		}))
		defer srv.Close()

		res := run(t, map[string]string{
			"{{vars.pr_url}}":              `"` + srv.URL + `/group/sub/proj/-/merge_requests/3"`,
			"{{secrets.forge_token.path}}": `"` + tokenFile(t) + `"`,
		})
		if !closesHit {
			t.Error("the GitLab linked-issue endpoint was never called")
		}
		if res.Mode != "forge" || res.Count != 1 {
			t.Fatalf("mode=%q count=%d note=%q", res.Mode, res.Count, res.Note)
		}
		if !strings.Contains(res.Tickets, "group/sub/proj#5") || !strings.Contains(res.Tickets, "Ship the importer") {
			t.Errorf("the linked issue is missing from the ticket block:\n%s", res.Tickets)
		}
	})

	// The switch, the external-tracker hand-off and a bare CLI run all have to
	// leave the node inert — and, crucially, never fail: this node sits on the
	// trunk of a bot whose merge gate must always land a verdict.
	t.Run("inert modes never fail the run", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			refs map[string]string
			want string
		}{
			{"off", map[string]string{"{{vars.ticket_context}}": `"off"`}, "off"},
			{"external tracker", map[string]string{"{{vars.tracker_api_base}}": `"https://jira.example.org"`}, "external"},
			{"no pr url", map[string]string{}, "none"},
			{"unknown url shape", map[string]string{"{{vars.pr_url}}": `"https://example.org/some/page"`}, "none"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				res := run(t, tc.refs)
				if res.Mode != tc.want {
					t.Errorf("mode = %q, want %q (note %q)", res.Mode, tc.want, res.Note)
				}
				if res.Tickets != "" || res.Count != 0 {
					t.Errorf("inert mode produced tickets: %q", res.Tickets)
				}
				if res.Note == "" {
					t.Error("an inert mode must say why, or the reviewer cannot report it")
				}
			})
		}
	})

	// An unreachable forge degrades to `unverifiable`; it must never crash the
	// node, because a crashed node is a review that never posts its gate.
	t.Run("unreachable forge degrades, never crashes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		}))
		url := srv.URL
		srv.Close() // nothing listens: every request fails at the socket
		res := run(t, map[string]string{
			"{{vars.pr_url}}":      `"` + url + `/acme/widgets/pull/7"`,
			"{{vars.ticket_refs}}": `"#9"`,
			"{{vars.scope_notes}}": `"Fixes #9"`,
		})
		if res.Mode != "forge" {
			t.Fatalf("mode = %q, want forge", res.Mode)
		}
		if res.Count != 0 {
			t.Fatalf("count = %d, want 0 — nothing was readable", res.Count)
		}
		if !strings.Contains(res.Status, "acme/widgets#9: unverifiable") {
			t.Errorf("an unreachable forge must produce an explicit unverifiable line, got %q", res.Status)
		}
	})
}
