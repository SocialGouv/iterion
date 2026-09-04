package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// mrServer serves one MR payload on both the get-one and the list endpoints of
// grp/proj, so the two read paths can be asserted against the same shape.
func mrServer(t *testing.T, payload map[string]any) *AdminClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/grp%2Fproj/merge_requests/9", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	})
	mux.HandleFunc("GET /api/v4/projects/grp%2Fproj/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{payload})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
}

// GitLab names the MR's two sides by numeric project id only. Equal ids prove
// the source branch lives in the very project we queried — which is what the
// fail-closed fork guards (command lane, gate auto-fix, gate relaunch) demand
// before launching a bot on `<base>.CloneURL + pr.SourceBranch`. Without this
// the field stayed empty on every GitLab MR and SameRepoAs refused all of them.
func TestGitLabMRHeadRepo(t *testing.T) {
	base := map[string]any{
		"iid": 9, "title": "t", "state": "opened", "sha": "h1",
		"source_branch": "feat", "target_branch": "main",
		"web_url": "https://gitlab.example/grp/proj/-/merge_requests/9",
	}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	cases := []struct {
		name     string
		payload  map[string]any
		wantHead string
		wantSame bool
	}{
		{
			name:     "same project",
			payload:  with(map[string]any{"source_project_id": 42, "target_project_id": 42}),
			wantHead: "grp/proj",
			wantSame: true,
		},
		{
			name:     "fork",
			payload:  with(map[string]any{"source_project_id": 77, "target_project_id": 42}),
			wantHead: "",
			wantSame: false,
		},
		{
			// An older GitLab or a partial payload omits the ids: unknown is
			// never proven safe, so the guards must still refuse.
			name:     "ids omitted",
			payload:  base,
			wantHead: "",
			wantSame: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mrServer(t, tc.payload)
			pr, err := c.GetPullRequest(context.Background(), "grp/proj", 9)
			if err != nil {
				t.Fatal(err)
			}
			if pr.HeadRepoFullName != tc.wantHead {
				t.Fatalf("GetPullRequest HeadRepoFullName = %q, want %q", pr.HeadRepoFullName, tc.wantHead)
			}
			if got := pr.SameRepoAs("grp/proj"); got != tc.wantSame {
				t.Fatalf("SameRepoAs = %v, want %v", got, tc.wantSame)
			}
			list, err := c.ListPullRequests(context.Background(), "grp/proj", forge.PullListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 {
				t.Fatalf("ListPullRequests returned %d refs", len(list))
			}
			if list[0].HeadRepoFullName != tc.wantHead {
				t.Fatalf("ListPullRequests HeadRepoFullName = %q, want %q", list[0].HeadRepoFullName, tc.wantHead)
			}
		})
	}
}
