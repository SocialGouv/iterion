package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// The listing asks for push and schedule runs, one listing each: a pull
// request from a fork's `main` also has head branch main, and a page of those
// would hide main's runs. The nightly is one of main's runs.
func TestMainRunsListsPushAndScheduleRuns(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Encode())
		switch r.URL.Query().Get("event") {
		case "push":
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":2,"run_number":20,"event":"push"}]}`))
		case "schedule":
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":3,"run_number":21,"event":"schedule"}]}`))
		default:
			http.Error(w, "unexpected listing "+r.URL.RawQuery, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := &restClient{api: srv.URL, repo: "SocialGouv/iterion", workflow: "tests.yml", http: srv.Client()}
	runs, err := c.MainRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"branch=main&event=push&per_page=100", "branch=main&event=schedule&per_page=100"}
	if !slices.Equal(queries, want) {
		t.Fatalf("queries = %q, want %q", queries, want)
	}
	var ids []int64
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{2, 3}) {
		t.Fatalf("runs = %v, want the push run 2 and the nightly 3", ids)
	}
}

// A full page of push runs is a window: a nightly older than its oldest run
// would let the last-green lookup reach past push runs the page cut off.
func TestMainRunsClipsTheNightliesToAFullPushWindow(t *testing.T) {
	var push []string
	for i := 0; i < 100; i++ {
		push = append(push, fmt.Sprintf(`{"id":%d,"run_number":%d,"event":"push"}`, 1000+i, 500+i))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("event") {
		case "push":
			_, _ = w.Write([]byte(`{"workflow_runs":[` + strings.Join(push, ",") + `]}`))
		case "schedule":
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":7,"run_number":550,"event":"schedule"},{"id":8,"run_number":499,"event":"schedule"}]}`))
		}
	}))
	defer srv.Close()
	c := &restClient{api: srv.URL, repo: "SocialGouv/iterion", workflow: "tests.yml", http: srv.Client()}
	runs, err := c.MainRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	has := func(id int64) bool { return slices.ContainsFunc(runs, func(r Run) bool { return r.ID == id }) }
	if len(runs) != 101 || !has(7) || has(8) {
		t.Fatalf("got %d runs (nightly 7 kept: %v, nightly 8 kept: %v), want the 100 push runs and nightly 7 only", len(runs), has(7), has(8))
	}
}

// The auto-merge history is read from the pull request's timeline under the
// job's token, an actor GitHub does not name kept as empty, and a timeline
// longer than the page, an item of another type or a GraphQL error refused
// rather than read as "never armed". totalCount counts the whole timeline,
// whatever the itemTypes filter: only filteredCount counts the items asked.
func TestAutoMergeHistoryReadsTheTimeline(t *testing.T) {
	for name, tc := range map[string]struct {
		answer string
		want   []AutoMergeEvent
		err    string
	}{
		"disabled twice": {
			answer: `{"data":{"repository":{"pullRequest":{"timelineItems":{"totalCount":9,"filteredCount":2,"nodes":[{"__typename":"AutoMergeDisabledEvent","actor":{"login":"someone"},"reason":""},{"__typename":"AutoMergeDisabledEvent","actor":null,"reason":"merge conflict"}]}}}}}`,
			want:   []AutoMergeEvent{{Kind: AutoMergeDisabled, Actor: "someone"}, {Kind: AutoMergeDisabled, Reason: "merge conflict"}},
		},
		// The answer GitHub gave for #3702, never armed.
		"never armed": {
			answer: `{"data":{"repository":{"pullRequest":{"timelineItems":{"totalCount":7,"filteredCount":0,"nodes":[]}}}}}`,
		},
		// The answer GitHub gave for #3680: armed, queued, dequeued by a
		// person, armed again, then merged by the queue.
		"queued and dequeued": {
			answer: `{"data":{"repository":{"pullRequest":{"timelineItems":{"totalCount":17,"filteredCount":6,"nodes":[{"__typename":"AutoMergeEnabledEvent","actor":{"login":"devthejo"}},{"__typename":"AddedToMergeQueueEvent","actor":{"login":"devthejo"}},{"__typename":"RemovedFromMergeQueueEvent","actor":{"login":"devthejo"},"reason":"manual"},{"__typename":"AutoMergeEnabledEvent","actor":{"login":"devthejo"}},{"__typename":"AddedToMergeQueueEvent","actor":{"login":"devthejo"}},{"__typename":"RemovedFromMergeQueueEvent","actor":{"login":"github-merge-queue"},"reason":"merged"}]}}}}}`,
			want: []AutoMergeEvent{
				{Kind: AutoMergeEnabled, Actor: "devthejo"}, {Kind: AddedToQueue, Actor: "devthejo"}, {Kind: RemovedFromQueue, Actor: "devthejo", Reason: "manual"},
				{Kind: AutoMergeEnabled, Actor: "devthejo"}, {Kind: AddedToQueue, Actor: "devthejo"}, {Kind: RemovedFromQueue, Actor: "github-merge-queue", Reason: "merged"},
			},
		},
		"partial": {
			answer: `{"data":{"repository":{"pullRequest":{"timelineItems":{"totalCount":140,"filteredCount":101,"nodes":[{"__typename":"AutoMergeDisabledEvent","actor":{"login":"someone"}}]}}}}}`,
			err:    "refusing a partial timeline",
		},
		"another type": {
			answer: `{"data":{"repository":{"pullRequest":{"timelineItems":{"totalCount":3,"filteredCount":1,"nodes":[{"__typename":"HeadRefForcePushedEvent"}]}}}}}`,
			err:    `item 0 is a "HeadRefForcePushedEvent"`,
		},
		"graphql error": {
			answer: `{"data":null,"errors":[{"message":"Resource not accessible by integration"}]}`,
			err:    "Resource not accessible by integration",
		},
		"no pull request": {
			answer: `{"data":{"repository":{"pullRequest":null}}}`,
			err:    "no such pull request",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var auth, body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				raw, _ := io.ReadAll(r.Body)
				body = string(raw)
				_, _ = w.Write([]byte(tc.answer))
			}))
			defer srv.Close()
			c := &restClient{api: srv.URL, graphql: srv.URL + "/graphql", repo: "SocialGouv/iterion", token: "job-token", botToken: "bot-token", http: srv.Client()}
			got, err := c.AutoMergeHistory(context.Background(), 8001)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("history = %+v, want %+v", got, tc.want)
			}
			if auth != "Bearer job-token" {
				t.Errorf("Authorization = %q, want the job's token", auth)
			}
			for _, want := range []string{"filteredCount", "AUTO_MERGE_ENABLED_EVENT", "ADDED_TO_MERGE_QUEUE_EVENT", "REMOVED_FROM_MERGE_QUEUE_EVENT", "AUTO_MERGE_DISABLED_EVENT", `"owner":"SocialGouv"`, `"name":"iterion"`, `"number":8001`} {
				if !strings.Contains(body, want) {
					t.Errorf("the query lacks %s:\n%s", want, body)
				}
			}
		})
	}
}

// The open pull requests are read with their head's branch and repository,
// the two facts that make one main-watch's own; a deleted head repository is
// nil, never this repository.
func TestOpenPullRequestsReadsTheHeadRepository(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Path+"?"+r.URL.Query().Encode())
		_, _ = w.Write([]byte(`[{"number":8001,"state":"open","head":{"ref":"main-watch/quarantine-run-1","repo":{"full_name":"SocialGouv/iterion"}}},{"number":8002,"state":"open","head":{"ref":"main-watch/quarantine-run-2","repo":null}}]`))
	}))
	defer srv.Close()
	c := &restClient{api: srv.URL, repo: "SocialGouv/iterion", http: srv.Client()}
	prs, err := c.OpenPullRequests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/repos/SocialGouv/iterion/pulls?page=1&per_page=100&state=open"}; !slices.Equal(queries, want) {
		t.Fatalf("queries = %q, want %q", queries, want)
	}
	if len(prs) != 2 || prs[0].Head.Ref != "main-watch/quarantine-run-1" || prs[0].Head.Repo == nil || prs[0].Head.Repo.FullName != "SocialGouv/iterion" || prs[1].Head.Repo != nil {
		t.Fatalf("pull requests = %+v", prs)
	}
}
