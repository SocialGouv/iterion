package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GitHub always models a pull request's head repository, and sends
// `"repo": null` once a fork is DELETED or blocked. Read as "no head
// repository" that is indistinguishable from an answer that never carried
// one, and one step from "therefore the base repo" — so the ref says the
// head WAS declared, which is what makes the refusal name the right cause.
func TestGitHubPullHeadRepoDeclared(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantDeclared bool
		wantWithheld bool
		wantHead     string
	}{
		{
			name:         "fork head names its repo",
			body:         `{"number":7,"state":"open","head":{"ref":"f","sha":"s","repo":{"full_name":"mallory/widgets","clone_url":"https://github.com/mallory/widgets.git"}},"base":{"ref":"main"}}`,
			wantDeclared: true,
			wantHead:     "mallory/widgets",
		},
		{
			name:         "deleted fork sends repo: null",
			body:         `{"number":7,"state":"open","head":{"ref":"f","sha":"s","repo":null},"base":{"ref":"main"}}`,
			wantDeclared: true,
			wantWithheld: true,
		},
		{
			name: "an answer that never carried the key declares nothing",
			body: `{"number":7,"state":"open","head":{"ref":"f","sha":"s"},"base":{"ref":"main"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
			pr, err := c.GetPullRequest(context.Background(), "acme/widgets", 7)
			if err != nil {
				t.Fatal(err)
			}
			if pr.HeadRepoFullName != tc.wantHead {
				t.Errorf("head = %q, want %q", pr.HeadRepoFullName, tc.wantHead)
			}
			if pr.HeadRepoDeclared != tc.wantDeclared {
				t.Errorf("declared = %v, want %v", pr.HeadRepoDeclared, tc.wantDeclared)
			}
			if pr.HeadRepoWithheld() != tc.wantWithheld {
				t.Errorf("withheld = %v, want %v", pr.HeadRepoWithheld(), tc.wantWithheld)
			}
			if pr.SameRepoAs("acme/widgets") {
				t.Error("none of these heads is the base repo — same-repo must never be assumed from an empty field")
			}
		})
	}
}
