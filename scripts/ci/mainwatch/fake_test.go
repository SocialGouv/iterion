package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The fixtures under testdata/ were recorded from the GitHub API with
// `gh api` on 2026-11-25, reduced to the fields main-watch reads: runs
// 45578072102 (red: mongo-conformance timed out, test-e2e (0) died with no
// FAIL line), 45604352729 (red: a race-shard and a test-e2e test) and
// 45532787663 (red: the pkg/server test #3675 tracks), the main runs listing,
// the comparisons from each red run's last green run, and the suspects' pull
// requests. Job logs are excerpts of the real logs — every go test verdict
// line with two lines of context, or a job's last 25 lines when it has none —
// byte for byte, ANSI escapes and timestamps included. None of these runs was
// ever rerun: a scenario that needs a second attempt states it with
// fixtureGitHub.attempts and .jobsOf, on top of the recorded first one.

// fixtureGitHub serves the recorded reads and records every write.
type fixtureGitHub struct {
	t *testing.T

	runs     map[int64]Run       // overrides of testdata/runs
	attempts map[string]Run      // "<id>/<n>"
	jobsOf   map[string][]Job    // "<id>/<n>" overrides of testdata/jobs
	issues   []Issue             // the open issues
	comments map[int][]Comment   // per issue
	branches map[string]bool     // existing branches
	onBranch map[string]string   // the quarantine list on each branch
	pulls    map[int]PullRequest // overrides of testdata/pulls
	notPulls map[int]bool        // numbers that are issues: 404 on pulls/N
	byHead   map[string]int      // pull request number by head branch
	mainRuns []Run               // override of testdata/main-runs.json
	list     string              // the quarantine list on main
	failOnce map[string]error    // a write kind ("comment #N" for a comment) that fails once
	mu       sync.Mutex
	writes   []string
	next     int
}

// own is an issue or comment author main-watch writes as; stranger is not.
var (
	own      = User{Login: selfLogin}
	stranger = User{Login: "someone"}
)

// ownComment is a comment main-watch wrote.
func ownComment(body string) Comment { return Comment{Body: body, User: own} }

// fail returns the injected failure of a write kind, once.
func (f *fixtureGitHub) fail(kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.failOnce[kind]
	delete(f.failOnce, kind)
	return err
}

func newFixture(t *testing.T) *fixtureGitHub {
	list, err := os.ReadFile("../../../" + quarantineFile)
	if err != nil {
		t.Fatal(err)
	}
	return &fixtureGitHub{
		t: t, runs: map[int64]Run{}, attempts: map[string]Run{}, jobsOf: map[string][]Job{},
		comments: map[int][]Comment{}, branches: map[string]bool{}, onBranch: map[string]string{},
		pulls: map[int]PullRequest{}, notPulls: map[int]bool{}, byHead: map[string]int{}, failOnce: map[string]error{},
		list: string(list), next: 7001,
	}
}

func (f *fixtureGitHub) load(path string, v any) error {
	raw, err := os.ReadFile(filepath.Join("testdata", path))
	if err != nil {
		return fmt.Errorf("no fixture: %w", err)
	}
	return json.Unmarshal(raw, v)
}

func (f *fixtureGitHub) record(format string, a ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, fmt.Sprintf(format, a...))
}

func (f *fixtureGitHub) Run(_ context.Context, id int64) (Run, error) {
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	var r Run
	return r, f.load(fmt.Sprintf("runs/%d.json", id), &r)
}

func (f *fixtureGitHub) Attempt(ctx context.Context, id int64, n int) (Run, error) {
	if r, ok := f.attempts[fmt.Sprintf("%d/%d", id, n)]; ok {
		return r, nil
	}
	if n == 1 {
		var r Run
		return r, f.load(fmt.Sprintf("runs/%d.json", id), &r)
	}
	return Run{}, fmt.Errorf("no attempt %d of run %d in the fixture", n, id)
}

func (f *fixtureGitHub) Jobs(_ context.Context, id int64, n int) ([]Job, error) {
	if js, ok := f.jobsOf[fmt.Sprintf("%d/%d", id, n)]; ok {
		return js, nil
	}
	var v struct {
		Jobs []Job `json:"jobs"`
	}
	return v.Jobs, f.load(fmt.Sprintf("jobs/%d-%d.json", id, n), &v)
}

func (f *fixtureGitHub) JobLog(_ context.Context, id int64) (string, error) {
	raw, err := os.ReadFile(filepath.Join("testdata", "logs", fmt.Sprintf("%d.log", id)))
	if err != nil {
		return "", fmt.Errorf("no log fixture: %w", err)
	}
	return string(raw), nil
}

func (f *fixtureGitHub) MainRuns(context.Context) ([]Run, error) {
	if f.mainRuns != nil {
		return f.mainRuns, nil
	}
	var v struct {
		Runs []Run `json:"workflow_runs"`
	}
	return v.Runs, f.load("main-runs.json", &v)
}

func (f *fixtureGitHub) Compare(_ context.Context, base, head string) ([]Commit, error) {
	var v struct {
		Commits []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
	}
	if err := f.load(fmt.Sprintf("compare/%s...%s.json", base, head), &v); err != nil {
		return nil, err
	}
	out := make([]Commit, len(v.Commits))
	for i, c := range v.Commits {
		out[i] = Commit{SHA: c.SHA, Message: c.Commit.Message}
	}
	return out, nil
}

func (f *fixtureGitHub) PullRequest(_ context.Context, n int) (PullRequest, error) {
	if f.notPulls[n] {
		return PullRequest{}, &httpError{Method: "GET", Path: fmt.Sprintf("/repos/SocialGouv/iterion/pulls/%d", n), Status: 404, Body: `{"message":"Not Found"}`}
	}
	if pr, ok := f.pulls[n]; ok {
		return pr, nil
	}
	var pr PullRequest
	return pr, f.load(fmt.Sprintf("pulls/%d.json", n), &pr)
}

func (f *fixtureGitHub) OpenIssues(context.Context) ([]Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Issue(nil), f.issues...), nil
}

func (f *fixtureGitHub) Comments(_ context.Context, n int) ([]Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Comment(nil), f.comments[n]...), nil
}

const fixtureMainSHA = "8568ab11dfeedfacefeedfacefeedfacefeedfac"

func (f *fixtureGitHub) MainSHA(context.Context) (string, error) { return fixtureMainSHA, nil }

func (f *fixtureGitHub) File(_ context.Context, path, ref string) ([]byte, string, error) {
	if path == quarantineFile && ref == fixtureMainSHA {
		return []byte(f.list), "blob-sha", nil
	}
	if list, ok := f.onBranch[ref]; ok && path == quarantineFile {
		return []byte(list), "branch-blob-sha", nil
	}
	return nil, "", fmt.Errorf("no fixture for %s@%s", path, ref)
}

func (f *fixtureGitHub) BranchExists(_ context.Context, b string) (bool, error) {
	return f.branches[b], nil
}

func (f *fixtureGitHub) PullRequestFrom(_ context.Context, b string) (PullRequest, bool, error) {
	n, ok := f.byHead[b]
	if !ok {
		return PullRequest{}, false, nil
	}
	return f.pulls[n], true, nil
}

func (f *fixtureGitHub) RerunFailedJobs(_ context.Context, id int64) error {
	f.record("rerun %d", id)
	return nil
}

func (f *fixtureGitHub) EnsureLabel(_ context.Context, name, _, _ string) error {
	f.record("label %s", name)
	return nil
}

func (f *fixtureGitHub) CreateIssue(_ context.Context, title, body string, labels []string) (Issue, error) {
	f.mu.Lock()
	n := f.next
	f.next++
	is := Issue{ID: int64(n) * 10, Number: n, Title: title, Body: body, HTMLURL: fmt.Sprintf("https://github.com/SocialGouv/iterion/issues/%d", n), User: own}
	f.issues = append(f.issues, is)
	f.mu.Unlock()
	f.record("issue #%d %s [%s]\n%s", n, title, strings.Join(labels, ","), body)
	return is, nil
}

func (f *fixtureGitHub) AddSubIssue(_ context.Context, epic int, id int64) error {
	f.record("sub-issue %d of #%d", id, epic)
	return nil
}

func (f *fixtureGitHub) Comment(_ context.Context, n int, body string) error {
	if err := f.fail(fmt.Sprintf("comment #%d", n)); err != nil {
		return err
	}
	f.mu.Lock()
	f.comments[n] = append(f.comments[n], ownComment(body))
	f.mu.Unlock()
	f.record("comment #%d\n%s", n, body)
	return nil
}

func (f *fixtureGitHub) CloseIssue(_ context.Context, n int) error {
	f.record("close #%d", n)
	return nil
}

func (f *fixtureGitHub) CreateBranch(_ context.Context, b, sha string) error {
	if err := f.fail("branch"); err != nil {
		return err
	}
	if sha != fixtureMainSHA {
		return fmt.Errorf("no fixture for a branch at %s", sha)
	}
	f.branches[b] = true
	f.onBranch[b] = f.list
	f.record("branch %s at %s", b, sha)
	return nil
}

func (f *fixtureGitHub) PutFile(_ context.Context, branch, path, msg string, content []byte, blob string) error {
	if err := f.fail("commit"); err != nil {
		return err
	}
	f.onBranch[branch] = string(content)
	f.record("commit %s on %s (%s, over %s)\n%s", path, branch, msg, blob, content)
	return nil
}

func (f *fixtureGitHub) CreatePullRequest(_ context.Context, head, title, body string) (PullRequest, error) {
	if err := f.fail("pr"); err != nil {
		return PullRequest{}, err
	}
	pr := PullRequest{Number: 8001, NodeID: "PR_node", Title: title, State: "open", HTMLURL: "https://github.com/SocialGouv/iterion/pull/8001"}
	pr.Head.Ref = head
	f.pulls[pr.Number] = pr
	f.byHead[head] = pr.Number
	f.record("pr %s %s\n%s", head, title, body)
	return pr, nil
}

func (f *fixtureGitHub) EnableAutoMerge(_ context.Context, pr PullRequest) error {
	if err := f.fail("auto-merge"); err != nil {
		return err
	}
	stored := f.pulls[pr.Number]
	stored.AutoMerge = &struct{}{}
	f.pulls[pr.Number] = stored
	f.record("auto-merge #%d", pr.Number)
	return nil
}

func (f *fixtureGitHub) ClosePullRequest(_ context.Context, n int) error {
	pr, ok := f.pulls[n]
	if !ok {
		return fmt.Errorf("close #%d: no such pull request in the fixture", n)
	}
	pr.State = "closed"
	f.pulls[n] = pr
	f.record("close-pr #%d", n)
	return nil
}

// kinds is the first word of every write, in order.
func (f *fixtureGitHub) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.writes))
	for i, w := range f.writes {
		out[i] = strings.Fields(w)[0]
	}
	return out
}

// write returns the one write starting with prefix, failing unless exactly one.
func (f *fixtureGitHub) write(prefix string) string {
	f.t.Helper()
	var found []string
	for _, w := range f.writes {
		if strings.HasPrefix(w, prefix) {
			found = append(found, w)
		}
	}
	if len(found) != 1 {
		f.t.Fatalf("want exactly one write starting %q, got %d among:\n%s", prefix, len(found), strings.Join(f.writes, "\n---\n"))
	}
	return found[0]
}

// recordingGit records the reverts it is asked for.
type recordingGit struct {
	reverts []string
	err     error
}

func (g *recordingGit) Revert(_ context.Context, sha, branch string) error {
	g.reverts = append(g.reverts, sha+" "+branch)
	return g.err
}
