package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	redMongoRun = 45578072102 // mongo-conformance timeout + test-e2e (0) infra; suspect #3644
	redRaceRun  = 45604352729 // race-shard (2) + test-e2e (3) tests; suspects #3644 #3655 #3688
	red3675Run  = 45532787663 // race-shard (1): the pkg/server test #3675 tracks
)

func newWatcher(t *testing.T, gh GitHub, git Git) (*Watcher, *bytes.Buffer) {
	t.Helper()
	workflow, err := os.ReadFile("../../../" + watchedPath)
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	return &Watcher{
		GH: gh, Git: git, Repo: "SocialGouv/iterion", Out: out,
		Now:      func() time.Time { return time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC) },
		SelfURL:  "https://github.com/SocialGouv/iterion/actions/runs/1",
		Workflow: workflow,
		ValidateQuarantine: func(list []byte) error {
			t.Fatalf("the quarantine list was validated in a scenario that adds no row:\n%s", list)
			return nil
		},
	}, out
}

// recorded loads a recorded run and restates its latest attempt.
func recorded(t *testing.T, f *fixtureGitHub, id int64, attempt int, conclusion string) Run {
	t.Helper()
	r, err := f.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	r.Attempt, r.Conclusion = attempt, conclusion
	f.runs[id] = r
	return r
}

// failedJobsAgain makes attempt n of a run fail the same jobs as attempt 1.
func failedJobsAgain(t *testing.T, f *fixtureGitHub, id int64, n int) {
	t.Helper()
	js, err := f.Jobs(context.Background(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.jobsOf[fmt.Sprintf("%d/%d", id, n)] = js
}

func TestAFirstRedAttemptIsRerunOnce(t *testing.T) {
	f := newFixture(t)
	f.mainRuns = []Run{recorded(t, f, redMongoRun, 1, "failure")}
	w, _ := newWatcher(t, f, &recordingGit{})
	if err := w.HandleRun(context.Background(), redMongoRun); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"rerun"}) {
		t.Fatalf("writes = %v, want one rerun", got)
	}
	f.write(fmt.Sprintf("rerun %d", redMongoRun))
}

func TestARedRerunIsARegressionTicketNamingTestsAndSuspects(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redMongoRun, 2, "failure")
	failedJobsAgain(t, f, redMongoRun, 2)
	w, _ := newWatcher(t, f, &recordingGit{})
	if err := w.HandleRun(context.Background(), redMongoRun); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"label", "issue"}) {
		t.Fatalf("writes = %v, want the label then one issue", got)
	}
	is := f.write("issue")
	for _, want := range []string{
		"main red: 🧪 Tests failing on main since d49ced5d4",
		"`github.com/SocialGouv/iterion/pkg/store/mongo` `TestRunMetadataPublicWireGuard/acquisition/malformed` — job [mongo-conformance]",
		"Merged since the last green run https://github.com/SocialGouv/iterion/actions/runs/45550686629 (commit 8f2068563): #3644",
		"opens a revert of the most recently merged suspect",
		`"suspects":[3644],"revertable":true`,
		runMarker(Run{ID: redMongoRun, Attempt: 2}),
	} {
		if !strings.Contains(is, want) {
			t.Errorf("the main-red ticket lacks %q:\n%s", want, is)
		}
	}
	if strings.Contains(is, "test-e2e (0)") {
		t.Errorf("the ticket names test-e2e (0), which died with no FAIL line:\n%s", is)
	}
}

func TestAnInfrastructureOnlyRegressionIsNeverReverted(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redMongoRun, 2, "failure")
	all, _ := f.Jobs(context.Background(), redMongoRun, 1)
	var infra []Job
	for _, j := range all {
		if j.Name != "mongo-conformance" {
			infra = append(infra, j)
		}
	}
	f.jobsOf[fmt.Sprintf("%d/2", redMongoRun)] = infra
	w, _ := newWatcher(t, f, &recordingGit{})
	if err := w.HandleRun(context.Background(), redMongoRun); err != nil {
		t.Fatal(err)
	}
	is := f.write("issue")
	if !strings.Contains(is, `"revertable":false`) || !strings.Contains(is, "will NOT revert") {
		t.Fatalf("an infrastructure-only red ticket must refuse the revert:\n%s", is)
	}
}

func TestARedRunOfAnOpenEpisodeIsACommentOnce(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "failure")
	failedJobsAgain(t, f, redRaceRun, 2)
	f.issues = []Issue{{ID: 1, Number: 7000, Title: "main red", User: own, Body: `<!-- main-watch:red {"run":45578072102,"sha":"d49ced5d4a1e344c5fff0220f250577f53c41efb","since":"2026-11-25T08:38:32Z","suspects":[3644],"revertable":true} -->`}}
	w, _ := newWatcher(t, f, &recordingGit{})
	for range 2 {
		if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.kinds(); !slices.Equal(got, []string{"comment"}) {
		t.Fatalf("writes = %v, want one comment on the open ticket over two passes", got)
	}
	if c := f.write("comment #7000"); !strings.Contains(c, "TestSandboxRelay_ABulkyStepStillDeliversItsTokenCounts") {
		t.Errorf("the comment does not name the failing test:\n%s", c)
	}
}

func TestAGreenRerunFilesAFlakeTicketAndAQuarantinePullRequest(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	w, _ := newWatcher(t, f, &recordingGit{})
	var validated []byte
	w.ValidateQuarantine = func(list []byte) error {
		validated = list
		return validateWithTestquarantine("../../..")(list)
	}
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	want := []string{"label", "issue", "issue", "sub-issue", "sub-issue", "branch", "commit", "pr", "auto-merge", "comment", "comment"}
	if got := f.kinds(); !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	rows := []string{
		"github.com/SocialGouv/iterion/e2e\tTestProdWatch_SentryAnOversizedPageFloodsNoChannel\t#7001",
		"github.com/SocialGouv/iterion/pkg/backend/model\tTestSandboxRelay_ABulkyStepStillDeliversItsTokenCounts\t#7002",
	}
	commit := f.write("commit " + quarantineFile + " on main-watch/quarantine-run-45604352729")
	for _, r := range rows {
		if !strings.Contains(commit, r+"\n") {
			t.Errorf("the quarantine commit lacks row %q:\n%s", r, commit)
		}
	}
	if !bytes.HasPrefix(validated, []byte(f.list)) || !strings.HasSuffix(string(validated), rows[0]+"\n"+rows[1]+"\n") {
		t.Errorf("the validated list is not main's list plus the two rows:\n%s", validated)
	}
	f.write("sub-issue 70010 of #1410")
	f.write("sub-issue 70020 of #1410")
	if c := f.write("comment #7001"); !strings.Contains(c, "quarantined by https://github.com/SocialGouv/iterion/pull/8001") {
		t.Errorf("the flake ticket does not link its quarantine pull request:\n%s", c)
	}
}

func TestAFlakeAppendsToTheOpenTicketAPersonFiled(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, red3675Run, 2, "success")
	var human Issue
	if err := f.load("issue-3675.json", &human); err != nil {
		t.Fatal(err)
	}
	f.issues = []Issue{human}
	w, _ := newWatcher(t, f, &recordingGit{})
	w.ValidateQuarantine = func([]byte) error { return nil }
	if err := w.HandleRun(context.Background(), red3675Run); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.kinds(), "issue") {
		t.Fatalf("a new ticket was filed beside #3675:\n%s", strings.Join(f.writes, "\n---\n"))
	}
	if c := f.write("commit"); !strings.Contains(c, "github.com/SocialGouv/iterion/pkg/server\tTestAssistantWatchSweep_K3HandoverThroughRetryDeliveries/every_resume_unreachable\t#3675\n") {
		t.Errorf("the row does not name the subtest under #3675:\n%s", c)
	}
	f.write("comment #3675")
}

func TestAFlakeOutsideTheQuarantinedJobsIsTicketedNotQuarantined(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redMongoRun, 2, "success")
	w, _ := newWatcher(t, f, &recordingGit{})
	if err := w.HandleRun(context.Background(), redMongoRun); err != nil {
		t.Fatal(err)
	}
	want := []string{"label", "issue", "sub-issue", "comment"}
	if got := f.kinds(); !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v (no branch, no pull request)", got, want)
	}
	if c := f.write("comment"); !strings.Contains(c, "not quarantined: job `mongo-conformance` does not skip the quarantine list") {
		t.Errorf("the ticket comment does not say why there is no row:\n%s", c)
	}
}

func TestAnInfrastructureFlakeIsRerunOnly(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redMongoRun, 2, "success")
	all, _ := f.Jobs(context.Background(), redMongoRun, 1)
	var infra []Job
	for _, j := range all {
		if j.Name != "mongo-conformance" {
			infra = append(infra, j)
		}
	}
	f.jobsOf[fmt.Sprintf("%d/1", redMongoRun)] = infra
	w, out := newWatcher(t, f, &recordingGit{})
	if err := w.HandleRun(context.Background(), redMongoRun); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("an infrastructure flake wrote %v", f.kinds())
	}
	if !strings.Contains(out.String(), "infrastructure failure, nothing to quarantine") {
		t.Errorf("the decision is not reported:\n%s", out)
	}
}

func TestAFlakeRunAlreadyFiledWritesNothing(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	w, out := newWatcher(t, f, &recordingGit{})
	w.ValidateQuarantine = func([]byte) error { return nil }
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	f.writes = nil
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("a run already filed wrote %v", f.kinds())
	}
	if !strings.Contains(out.String(), "exists with auto-merge armed, its flakes are already filed") {
		t.Errorf("the second pass does not say the run is filed:\n%s", out)
	}
}

func TestARunOutsideMainsTrustBoundaryIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*Run){
		"fork":           func(r *Run) { r.HeadRepository.FullName = "attacker/iterion" },
		"pull request":   func(r *Run) { r.Event = "pull_request" },
		"branch":         func(r *Run) { r.HeadBranch = "feature" },
		"other workflow": func(r *Run) { r.Path = ".github/workflows/docs.yml" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			r := recorded(t, f, redMongoRun, 1, "failure")
			mutate(&r)
			f.runs[redMongoRun] = r
			w, _ := newWatcher(t, f, &recordingGit{})
			err := w.HandleRun(context.Background(), redMongoRun)
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if len(f.writes) != 0 {
				t.Fatalf("a refused run wrote %v", f.kinds())
			}
		})
	}
}

// redEpisode opens the main-red ticket of redMongoRun, as regression files it.
func redEpisode(t *testing.T, f *fixtureGitHub, suspects []int) Issue {
	t.Helper()
	st, err := json.Marshal(redState{Run: redMongoRun, SHA: "d49ced5d4a1e344c5fff0220f250577f53c41efb", Since: time.Date(2026, 11, 25, 8, 38, 32, 0, time.UTC), GreenSHA: "8f2068563d1024dd13c2cecbcd97f0aeb6c98576", Suspects: suspects, Revertable: true})
	if err != nil {
		t.Fatal(err)
	}
	is := Issue{ID: 70000, Number: 7000, Title: "main red", User: own, Body: "<!-- main-watch:red " + string(st) + " -->"}
	f.issues = append(f.issues, is)
	return is
}

// redMain makes main's newest run a red rerun of redRaceRun.
func redMain(t *testing.T, f *fixtureGitHub) {
	t.Helper()
	r := recorded(t, f, redRaceRun, 2, "failure")
	f.mainRuns = []Run{r}
	f.comments[7000] = []Comment{ownComment(runMarker(r))}
}

func TestMainRedForFourHoursRevertsTheMostRecentSuspect(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644, 3655, 3688})
	redMain(t, f)
	git := &recordingGit{}
	w, _ := newWatcher(t, f, git)
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(git.reverts, []string{"294110a7b4d4aa276f412b47a15a250e2ae25ec5 main-watch/revert-3688"}) {
		t.Fatalf("reverts = %v, want #3688, the last merged suspect", git.reverts)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"pr", "auto-merge", "comment", "comment"}) {
		t.Fatalf("writes = %v, want the revert armed before the ticket records it", got)
	}
	if pr := f.write("pr main-watch/revert-3688 revert: ci: run race/e2e legs"); !strings.Contains(pr, "Reverts #3688") {
		t.Errorf("revert body:\n%s", pr)
	}
	f.write("comment #3688")
	if c := f.write("comment #7000"); !revertMarker.MatchString(c) {
		t.Errorf("the ticket does not record the revert:\n%s", c)
	}
	// The episode's revert is spent: the next sweep does nothing.
	f.writes = nil
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || len(git.reverts) != 1 {
		t.Fatalf("a second sweep wrote %v and reverted %v", f.kinds(), git.reverts)
	}
}

func TestTheRevertWaitsFourHours(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644})
	redMain(t, f)
	git := &recordingGit{}
	w, _ := newWatcher(t, f, git)
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 38, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || len(git.reverts) != 0 {
		t.Fatalf("3 h 59 into the episode: writes %v, reverts %v", f.kinds(), git.reverts)
	}
}

func TestARevertIsNeverReverted(t *testing.T) {
	for name, pr := range map[string]PullRequest{
		"main-watch branch": {Title: "revert: feat: x", Head: struct {
			Ref string `json:"ref"`
		}{Ref: "main-watch/revert-3644"}},
		"revert title": {Title: `Revert "feat: x"`, Head: struct {
			Ref string `json:"ref"`
		}{Ref: "someone/undo"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			merged := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
			pr.Number, pr.MergedAt, pr.MergeCommitSHA = 9999, &merged, "abc"
			f.pulls[9999] = pr
			redEpisode(t, f, []int{3644, 9999})
			redMain(t, f)
			git := &recordingGit{}
			w, _ := newWatcher(t, f, git)
			w.Now = func() time.Time { return time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC) }
			if err := w.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(git.reverts) != 0 {
				t.Fatalf("reverted %v", git.reverts)
			}
			if c := f.write("comment #7000"); !strings.Contains(c, "#9999 is itself a revert or a main-watch change") || !strings.Contains(c, "<!-- main-watch:revert-refused -->") {
				t.Errorf("refusal:\n%s", c)
			}
		})
	}
}

func TestARevertThatDoesNotApplyIsHandedToAPerson(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644})
	redMain(t, f)
	git := &recordingGit{err: fmt.Errorf("%w: CONFLICT", errRevertDoesNotApply)}
	w, _ := newWatcher(t, f, git)
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"comment"}) {
		t.Fatalf("writes = %v, want the refusal only", got)
	}
	if c := f.write("comment #7000"); !strings.Contains(c, "does not apply cleanly") {
		t.Errorf("refusal:\n%s", c)
	}
}

func TestAGreenRunClosesTheEpisodeOnlyIfNotOlder(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644})
	older := recorded(t, f, red3675Run, 1, "success") // started before the red run
	w, _ := newWatcher(t, f, &recordingGit{})
	if err := w.HandleRun(context.Background(), older.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("an older green run wrote %v", f.kinds())
	}
	recorded(t, f, redRaceRun, 1, "success")
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"comment", "close"}) {
		t.Fatalf("writes = %v, want a comment and the close", got)
	}
	f.write("close #7000")
}

func TestTheSweepCatchesAMissedEventOnMainsNewestRun(t *testing.T) {
	f := newFixture(t)
	r := recorded(t, f, redRaceRun, 1, "failure")
	old := recorded(t, f, redMongoRun, 1, "success")
	f.mainRuns = []Run{old, r}
	w, _ := newWatcher(t, f, &recordingGit{})
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"rerun"}) {
		t.Fatalf("writes = %v, want the newest run rerun", got)
	}
	f.write(fmt.Sprintf("rerun %d", redRaceRun))
}

// The dry run is the recorded scenario through the dry-run client: the same
// decisions, printed, and not one write reaching the client underneath.
func TestADryRunPrintsTheDecisionAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	out := &bytes.Buffer{}
	w, _ := newWatcher(t, newDryRun(f, out), dryGit{out: out})
	w.Out = out
	w.ValidateQuarantine = func([]byte) error { return nil }
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("a dry run reached the client: %v", f.kinds())
	}
	for _, want := range []string{
		"DRY RUN — would open issue (placeholder #900001) \"flaky: e2e TestProdWatch_SentryAnOversizedPageFloodsNoChannel\"",
		"DRY RUN — would create branch main-watch/quarantine-run-45604352729",
		"| github.com/SocialGouv/iterion/pkg/backend/model\tTestSandboxRelay_ABulkyStepStillDeliversItsTokenCounts\t#900002",
		"DRY RUN — would arm auto-merge",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
}

// A rerun in progress hides nothing: the sweep judges the newest run (here
// still running, so nothing), never an older red one behind it, and reads
// main's state from the newest run with a verdict.
func TestTheSweepNeverRerunsAnOlderRunBehindOneInProgress(t *testing.T) {
	f := newFixture(t)
	running := recorded(t, f, redRaceRun, 2, "")
	running.Status = "in_progress"
	older := recorded(t, f, redMongoRun, 1, "failure")
	f.mainRuns = []Run{older, running}
	w, out := newWatcher(t, f, &recordingGit{})
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("the sweep wrote %v behind a run in progress", f.kinds())
	}
	if !strings.Contains(out.String(), "is in_progress: nothing to judge") {
		t.Errorf("the newest run was not the one judged:\n%s", out)
	}
}

func TestTheSweepClosesTheEpisodeOnAGreenVerdictBehindARunInProgress(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644})
	running := recorded(t, f, redRaceRun, 1, "")
	running.Status = "in_progress"
	green := recorded(t, f, redRaceRun, 1, "success")
	green.ID, green.RunNumber = 1, running.RunNumber-1
	f.mainRuns = []Run{green, running}
	w, _ := newWatcher(t, f, &recordingGit{})
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"comment", "close"}) {
		t.Fatalf("writes = %v, want the episode closed", got)
	}
}

// A stranger's issue carrying a main-red marker is no episode: whatever
// state it claims (a past start that would trigger a revert, a future one
// that would swallow every regression), main-watch files its own ticket.
func TestAStrangersRedMarkerIsIgnored(t *testing.T) {
	for name, since := range map[string]string{"past": "2020-01-01T00:00:00Z", "future": "2099-01-01T00:00:00Z"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.issues = []Issue{{ID: 95000, Number: 9500, Title: "question about the docs", User: stranger, AuthorAssociation: "NONE",
				Body: `<!-- main-watch:red {"run":1,"sha":"x","since":"` + since + `","suspects":[3655],"revertable":true} -->`}}
			r := recorded(t, f, redRaceRun, 2, "failure")
			failedJobsAgain(t, f, redRaceRun, 2)
			f.mainRuns = []Run{r}
			git := &recordingGit{}
			w, _ := newWatcher(t, f, git)
			w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
			if err := w.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(git.reverts) != 0 {
				t.Fatalf("a stranger's marker drove a revert: %v", git.reverts)
			}
			if got := f.kinds(); !slices.Equal(got, []string{"label", "issue"}) {
				t.Fatalf("writes = %v, want main-watch's own main-red ticket", got)
			}
			f.write("issue #7001 main red")
		})
	}
}

// A stranger's revert-refused comment on the real ticket does not spend the
// episode's revert.
func TestAStrangersRevertMarkerIsIgnored(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644, 3655, 3688})
	redMain(t, f)
	f.comments[7000] = append(f.comments[7000], Comment{Body: "lol <!-- main-watch:revert-refused -->", User: stranger})
	git := &recordingGit{}
	w, _ := newWatcher(t, f, git)
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(git.reverts, []string{"294110a7b4d4aa276f412b47a15a250e2ae25ec5 main-watch/revert-3688"}) {
		t.Fatalf("reverts = %v: a stranger's comment cancelled the revert", git.reverts)
	}
}

// A stranger's comment carrying a run marker does not hide the run from the
// ticket.
func TestAStrangersRunMarkerIsIgnored(t *testing.T) {
	f := newFixture(t)
	r := recorded(t, f, redRaceRun, 2, "failure")
	failedJobsAgain(t, f, redRaceRun, 2)
	redEpisode(t, f, []int{3644})
	f.comments[7000] = []Comment{{Body: runMarker(r), User: stranger}}
	w, _ := newWatcher(t, f, &recordingGit{})
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"comment"}) {
		t.Fatalf("writes = %v, want the run recorded on #7000", got)
	}
}

// A stranger's issue carrying a flake marker, or a `flaky:` title, is not the
// flake's ticket: its author could close it and redden the nightly witness.
func TestAStrangersFlakeTicketIsIgnored(t *testing.T) {
	for name, is := range map[string]Issue{
		"marker": {Title: "hello", Body: "<!-- main-watch:flake github.com/SocialGouv/iterion/e2e TestProdWatch_SentryAnOversizedPageFloodsNoChannel -->", User: stranger, AuthorAssociation: "NONE"},
		"title":  {Title: "flaky: TestProdWatch_SentryAnOversizedPageFloodsNoChannel", User: stranger, AuthorAssociation: "CONTRIBUTOR"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			is.ID, is.Number = 95000, 9500
			f.issues = []Issue{is}
			recorded(t, f, redRaceRun, 2, "success")
			w, _ := newWatcher(t, f, &recordingGit{})
			w.ValidateQuarantine = func([]byte) error { return nil }
			if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
				t.Fatal(err)
			}
			if c := f.write("commit"); strings.Contains(c, "#9500") {
				t.Fatalf("a quarantine row names the stranger's #9500:\n%s", c)
			}
			f.write("issue #7001 flaky: e2e TestProdWatch_SentryAnOversizedPageFloodsNoChannel")
		})
	}
}

// A rerun cancelled by a newer push leaves no verdict, and main-watch says
// so rather than staying silent or rerunning again.
func TestACancelledRerunOfARedRunSaysItsSignalIsLost(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "cancelled")
	w, out := newWatcher(t, f, &recordingGit{})
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("a cancelled rerun wrote %v", f.kinds())
	}
	if !strings.Contains(out.String(), "its rerun was cancelled") || !strings.Contains(out.String(), "signal is lost") {
		t.Errorf("the lost verdict is not reported:\n%s", out)
	}
}

// A `(#N)` suspect that is an issue hands the revert to a person: main-watch
// cannot tell when that commit landed, so it reverts none of the others.
func TestAnIssueAmongTheSuspectsRefusesTheRevert(t *testing.T) {
	f := newFixture(t)
	f.notPulls[3255] = true
	redEpisode(t, f, []int{3255, 3644, 3655, 3688})
	redMain(t, f)
	git := &recordingGit{}
	w, _ := newWatcher(t, f, git)
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(git.reverts) != 0 {
		t.Fatalf("reverted %v beside a suspect that is not a pull request", git.reverts)
	}
	if c := f.write("comment #7000"); !strings.Contains(c, "#3255 is not a pull request") || !strings.Contains(c, "<!-- main-watch:revert-refused -->") {
		t.Errorf("refusal:\n%s", c)
	}
	f.writes = nil
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("a second sweep wrote %v", f.kinds())
	}
}

// openedRevert runs the revert step of a 4 h red episode: #3688's revert is
// pull request 8001.
func openedRevert(t *testing.T) (*fixtureGitHub, *Watcher) {
	t.Helper()
	f := newFixture(t)
	redEpisode(t, f, []int{3644, 3655, 3688})
	redMain(t, f)
	w, _ := newWatcher(t, f, &recordingGit{})
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.write("pr main-watch/revert-3688")
	f.writes = nil
	return f, w
}

func TestAGreenMainClosesTheEpisodesOpenRevert(t *testing.T) {
	f, w := openedRevert(t)
	recorded(t, f, redRaceRun, 1, "success")
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"close-pr", "comment", "comment", "close"}) {
		t.Fatalf("writes = %v, want the revert closed, then the episode", got)
	}
	f.write("close-pr #8001")
	f.write("comment #8001")
	if c := f.write("comment #7000"); !strings.Contains(c, "Closed the revert https://github.com/SocialGouv/iterion/pull/8001") {
		t.Errorf("the ticket does not say the revert was closed:\n%s", c)
	}
}

// A revert step that fails after arming the revert (here the comment on the
// suspect pull request) leaves the revert recorded on the ticket: main's next
// green run closes it instead of letting it merge.
func TestAGreenMainClosesARevertWhoseStepFailedAfterArming(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644, 3655, 3688})
	redMain(t, f)
	f.failOnce["comment #3688"] = errors.New("POST /repos/SocialGouv/iterion/issues/3688/comments: HTTP 403")
	w, _ := newWatcher(t, f, &recordingGit{})
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err == nil {
		t.Fatal("the sweep hides the failed comment on the suspect")
	}
	if pr := f.pulls[8001]; pr.State != "open" || pr.AutoMerge == nil {
		t.Fatalf("revert #8001 = state %q, armed %v: want it open and armed before the failure", pr.State, pr.AutoMerge != nil)
	}
	f.writes = nil
	recorded(t, f, redRaceRun, 1, "success")
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if pr := f.pulls[8001]; pr.State != "closed" {
		t.Fatalf("revert #8001 after main's green run: state %q, armed %v; writes %v", pr.State, pr.AutoMerge != nil, f.kinds())
	}
}

func TestAGreenMainAfterTheRevertMergedClosesOnlyTheTicket(t *testing.T) {
	f, w := openedRevert(t)
	pr := f.pulls[8001]
	merged := time.Date(2026, 11, 25, 13, 30, 0, 0, time.UTC)
	pr.State, pr.MergedAt = "closed", &merged
	f.pulls[8001] = pr
	recorded(t, f, redRaceRun, 1, "success")
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"comment", "close"}) {
		t.Fatalf("writes = %v, want the ticket commented and closed only", got)
	}
	if c := f.write("comment #7000"); !strings.Contains(c, "is already merged") {
		t.Errorf("the ticket does not say the revert merged:\n%s", c)
	}
}

// A flake pass that fails after any of its writes is finished by the next
// one: rows committed, pull request opened and armed, tickets commented once.
func TestAFlakePassThatFailedHalfWayIsFinishedByTheNext(t *testing.T) {
	for _, kind := range []string{"branch", "commit", "pr", "auto-merge"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			recorded(t, f, redRaceRun, 2, "success")
			f.failOnce[kind] = fmt.Errorf("injected %s failure", kind)
			w, _ := newWatcher(t, f, &recordingGit{})
			w.ValidateQuarantine = func([]byte) error { return nil }
			if err := w.HandleRun(context.Background(), redRaceRun); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("pass 1: err = %v, want the injected failure", err)
			}
			if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
				t.Fatalf("pass 2: %v", err)
			}
			branch := "main-watch/quarantine-run-45604352729"
			for _, row := range []string{
				"github.com/SocialGouv/iterion/e2e\tTestProdWatch_SentryAnOversizedPageFloodsNoChannel\t#7001\n",
				"github.com/SocialGouv/iterion/pkg/backend/model\tTestSandboxRelay_ABulkyStepStillDeliversItsTokenCounts\t#7002\n",
			} {
				if strings.Count(f.onBranch[branch], row) != 1 {
					t.Errorf("the branch's list holds row %q %d times:\n%s", row, strings.Count(f.onBranch[branch], row), f.onBranch[branch])
				}
			}
			n, ok := f.byHead[branch]
			if !ok || f.pulls[n].AutoMerge == nil {
				t.Fatalf("pull request from %s: found=%v, auto-merge armed=%v", branch, ok, ok && f.pulls[n].AutoMerge != nil)
			}
			for _, ticket := range []int{7001, 7002} {
				if got := len(f.comments[ticket]); got != 1 {
					t.Errorf("#%d has %d comments, want 1", ticket, got)
				}
			}
			tickets := 0
			for _, k := range f.kinds() {
				if k == "issue" {
					tickets++
				}
			}
			if tickets != 2 {
				t.Errorf("writes = %v, want two tickets in all", f.kinds())
			}
		})
	}
}

// A job past its timeout-minutes concludes cancelled: the ticket names it as
// a hang, never as infrastructure, and refuses the revert.
func TestAHungJobIsNamedInTheRegressionTicket(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "failure")
	all, _ := f.Jobs(context.Background(), redRaceRun, 1)
	var hung []Job
	for _, j := range all {
		if j.Name == "test-unit (1)" {
			hung = append(hung, j)
		}
	}
	if len(hung) != 1 || hung[0].Conclusion != "cancelled" {
		t.Fatalf("the recorded test-unit (1) job: %+v", hung)
	}
	f.jobsOf[fmt.Sprintf("%d/2", redRaceRun)] = hung
	w, _ := newWatcher(t, f, &recordingGit{})
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	is := f.write("issue")
	for _, want := range []string{"- job [test-unit (1)]", "hung: the job exceeded its timeout (a hang); `server.test` was still running", `"revertable":false`, "a job hung"} {
		if !strings.Contains(is, want) {
			t.Errorf("the ticket lacks %q:\n%s", want, is)
		}
	}
	if strings.Contains(is, "infrastructure") {
		t.Errorf("the ticket calls a hang infrastructure:\n%s", is)
	}
}

// A quarantine pull request a person closed stays closed.
func TestAClosedQuarantinePullRequestIsNotReopened(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	w, out := newWatcher(t, f, &recordingGit{})
	w.ValidateQuarantine = func([]byte) error { return nil }
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	pr := f.pulls[8001]
	pr.State = "closed"
	f.pulls[8001] = pr
	f.writes = nil
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes = %v after a person closed the quarantine pull request", f.kinds())
	}
	if !strings.Contains(out.String(), "pull/8001 was closed unmerged: main-watch does not reopen it") {
		t.Errorf("the decision is not reported:\n%s", out)
	}
}

// A rerun joins the watched workflow's concurrency group, which may cancel
// the runs in it: rerunning an older red run would cost main's newer run
// (push or nightly, running or done) its verdict. Only main's newest eligible
// run is rerun, and the refusal names the run that judges main instead.
func TestARedRunBehindANewerRunOfMainIsNotRerun(t *testing.T) {
	for _, event := range []string{"push", "schedule"} {
		for _, status := range []string{"in_progress", "completed"} {
			t.Run(event+"/"+status, func(t *testing.T) {
				f := newFixture(t)
				older := recorded(t, f, redMongoRun, 1, "failure")
				newer := older
				newer.ID, newer.RunNumber, newer.Event, newer.Status, newer.Conclusion = 45600000001, older.RunNumber+1, event, status, ""
				if status == "completed" {
					newer.Conclusion = "success"
				}
				f.mainRuns = []Run{older, newer}
				w, out := newWatcher(t, f, &recordingGit{})
				if err := w.HandleRun(context.Background(), redMongoRun); err != nil {
					t.Fatal(err)
				}
				if len(f.writes) != 0 {
					t.Fatalf("an older red run was rerun behind main's newer run: writes = %v", f.kinds())
				}
				if want := fmt.Sprintf("main's newer run %d (#%d", newer.ID, newer.RunNumber); !strings.Contains(out.String(), want) {
					t.Errorf("the refusal does not name the newer run (%q):\n%s", want, out)
				}
			})
		}
	}
}

// The dry run prints the same refusal.
func TestADryRunReportsTheNewerRunThatJudgesMain(t *testing.T) {
	f := newFixture(t)
	older := recorded(t, f, redMongoRun, 1, "failure")
	newer := older
	newer.ID, newer.RunNumber, newer.Status, newer.Conclusion = 45600000001, older.RunNumber+1, "in_progress", ""
	f.mainRuns = []Run{newer, older}
	out := &bytes.Buffer{}
	w, _ := newWatcher(t, newDryRun(f, out), dryGit{out: out})
	w.Out = out
	if err := w.HandleRun(context.Background(), redMongoRun); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "would rerun") || !strings.Contains(out.String(), "main's newer run 45600000001") {
		t.Fatalf("the dry run does not report the refusal:\n%s", out)
	}
}

// A maintainer's `flaky:` ticket names a test by its top-level name, which
// many packages share: it is the flake's ticket only when its title also
// names the flake's package.
func TestAMaintainersFlakyTicketOfAnotherPackageIsNotReused(t *testing.T) {
	for name, title := range map[string]string{
		"other package": "flaky: pkg/runtime TestAssistantWatchSweep_K3HandoverThroughRetryDeliveries hands over one pass early",
		"no package":    "flaky: TestAssistantWatchSweep_K3HandoverThroughRetryDeliveries hands over one pass early",
		"longer path":   "flaky: pkg/server/sub TestAssistantWatchSweep_K3HandoverThroughRetryDeliveries hands over one pass early",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			recorded(t, f, red3675Run, 2, "success")
			var human Issue
			if err := f.load("issue-3675.json", &human); err != nil {
				t.Fatal(err)
			}
			human.Title = title
			f.issues = []Issue{human}
			w, _ := newWatcher(t, f, &recordingGit{})
			w.ValidateQuarantine = func([]byte) error { return nil }
			if err := w.HandleRun(context.Background(), red3675Run); err != nil {
				t.Fatal(err)
			}
			if slices.ContainsFunc(f.writes, func(s string) bool { return strings.HasPrefix(s, "comment #3675") }) {
				t.Fatalf("the flake was recorded on #3675, whose title names another package:\n%s", strings.Join(f.writes, "\n---\n"))
			}
			f.write("issue #7001 flaky: pkg/server TestAssistantWatchSweep_K3HandoverThroughRetryDeliveries")
		})
	}
}

// The package matches as a whole token, backquoted or by its import path.
func TestAMaintainersFlakyTicketNamingThePackageIsReused(t *testing.T) {
	for name, title := range map[string]string{
		"backquoted":  "flaky: `pkg/server` `TestAssistantWatchSweep_K3HandoverThroughRetryDeliveries` hangs",
		"import path": "flaky: github.com/SocialGouv/iterion/pkg/server TestAssistantWatchSweep_K3HandoverThroughRetryDeliveries",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			recorded(t, f, red3675Run, 2, "success")
			var human Issue
			if err := f.load("issue-3675.json", &human); err != nil {
				t.Fatal(err)
			}
			human.Title = title
			f.issues = []Issue{human}
			w, _ := newWatcher(t, f, &recordingGit{})
			w.ValidateQuarantine = func([]byte) error { return nil }
			if err := w.HandleRun(context.Background(), red3675Run); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(f.kinds(), "issue") {
				t.Fatalf("a new ticket was filed beside #3675:\n%s", strings.Join(f.writes, "\n---\n"))
			}
			f.write("comment #3675")
		})
	}
}

// A person who disables the quarantine pull request's auto-merge has the last
// word: a later pass of the same run neither re-arms it nor writes anything
// else, and says why.
func TestAQuarantineAutoMergeAPersonDisabledIsNotReArmed(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	w, out := newWatcher(t, f, &recordingGit{})
	w.ValidateQuarantine = func([]byte) error { return nil }
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	pr := f.pulls[8001]
	pr.AutoMerge = nil
	f.pulls[8001] = pr
	f.history[8001] = append(f.history[8001], AutoMergeEvent{Kind: AutoMergeDisabled, Actor: "someone"})
	f.writes = nil
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes = %v after a person disabled the auto-merge", f.kinds())
	}
	if !strings.Contains(out.String(), "pull/8001 was disabled by `someone`: main-watch does not re-arm it") {
		t.Errorf("the decision is not reported:\n%s", out)
	}
}

// A flake seen first after its auto-merge was disabled tells the ticket so.
func TestAFlakeTicketSaysTheQuarantineAutoMergeWasDisabled(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	branch := "main-watch/quarantine-run-45604352729"
	pr := PullRequest{Number: 8001, NodeID: "PR_node", Title: "test(quarantine)", State: "open", HTMLURL: "https://github.com/SocialGouv/iterion/pull/8001"}
	pr.Head.Ref = branch
	f.pulls[8001], f.byHead[branch] = pr, 8001
	f.history[8001] = []AutoMergeEvent{{Kind: AutoMergeEnabled, Actor: selfLogin}, {Kind: AutoMergeDisabled, Actor: "someone", Reason: "disabled by a user"}}
	w, _ := newWatcher(t, f, &recordingGit{})
	w.ValidateQuarantine = func([]byte) error { return nil }
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.kinds(), "auto-merge") || slices.Contains(f.kinds(), "pr") {
		t.Fatalf("writes = %v: the existing pull request is reused and left unarmed", f.kinds())
	}
	if c := f.write("comment #7001"); !strings.Contains(c, "(auto-merge disabled by `someone` (disabled by a user), main-watch leaves it to a person)") {
		t.Errorf("the ticket does not say the auto-merge is disabled:\n%s", c)
	}
}

// A quarantine pull request that lost its arming without a disable (here a
// person removed it from the merge queue, the shape of #3680's timeline) is
// not re-armed: once armed, any loss of arming stands.
func TestAQuarantinePullRequestAPersonDequeuedIsNotReArmed(t *testing.T) {
	for name, tc := range map[string]struct {
		after []AutoMergeEvent
		says  string
		note  string
	}{
		"dequeued by a person": {
			after: []AutoMergeEvent{{Kind: AddedToQueue, Actor: "devthejo"}, {Kind: RemovedFromQueue, Actor: "devthejo", Reason: "manual"}},
			says:  "pull/8001 was removed from the merge queue by `devthejo` (manual): main-watch does not re-arm it",
			note:  "(removed from the merge queue by `devthejo` (manual), main-watch leaves it to a person)",
		},
		"in the queue": {
			after: []AutoMergeEvent{{Kind: AddedToQueue, Actor: "devthejo"}},
			says:  "pull/8001 was added to the merge queue by `devthejo`: main-watch does not re-arm it",
			note:  "(in the merge queue, added by `devthejo`)",
		},
		"armed once, no longer": {
			says: "pull/8001 was armed once and is no longer, the timeline names no disable: main-watch does not re-arm it",
			note: "(auto-merge armed once and no longer armed, main-watch leaves it to a person)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			recorded(t, f, redRaceRun, 2, "success")
			w, out := newWatcher(t, f, &recordingGit{})
			w.ValidateQuarantine = func([]byte) error { return nil }
			if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
				t.Fatal(err)
			}
			pr := f.pulls[8001]
			pr.AutoMerge = nil
			f.pulls[8001] = pr
			f.history[8001] = append(f.history[8001], tc.after...)
			f.writes = nil
			if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
				t.Fatal(err)
			}
			if len(f.writes) != 0 {
				t.Fatalf("writes = %v after the quarantine pull request lost its arming", f.kinds())
			}
			if !strings.Contains(out.String(), tc.says) {
				t.Errorf("the decision is not reported as %q:\n%s", tc.says, out)
			}
			// A ticket seen first after the loss says so.
			g := newFixture(t)
			recorded(t, g, redRaceRun, 2, "success")
			g.pulls[8001], g.byHead[pr.Head.Ref] = pr, 8001
			g.history[8001] = slices.Clone(f.history[8001])
			v, _ := newWatcher(t, g, &recordingGit{})
			v.ValidateQuarantine = func([]byte) error { return nil }
			if err := v.HandleRun(context.Background(), redRaceRun); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(g.kinds(), "auto-merge") {
				t.Fatalf("writes = %v: the pull request is re-armed", g.kinds())
			}
			if c := g.write("comment #7001"); !strings.Contains(c, tc.note) {
				t.Errorf("the ticket does not say %q:\n%s", tc.note, c)
			}
		})
	}
}

// A quarantine pull request a person closed, branch deleted, gets nothing
// pushed: the pull request is read before any branch or commit.
func TestAClosedQuarantinePullRequestGetsNoPushEvenWithItsBranchGone(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	w, _ := newWatcher(t, f, &recordingGit{})
	w.ValidateQuarantine = func([]byte) error { return nil }
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	branch := "main-watch/quarantine-run-45604352729"
	pr := f.pulls[8001]
	pr.State = "closed"
	f.pulls[8001] = pr
	delete(f.branches, branch)
	delete(f.onBranch, branch)
	f.writes = nil
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes = %v for a closed quarantine pull request whose branch is gone", f.kinds())
	}
}

// A revert whose auto-merge cannot be armed is recorded on the ticket for a
// person, marker included: the sweep fails loudly, the revert step does not
// run again, and main's next green run closes the unarmed revert.
func TestARevertThatCannotBeArmedIsHandedToAPerson(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644, 3655, 3688})
	redMain(t, f)
	f.failOnce["auto-merge"] = errors.New("enable auto-merge on #8001: Pull request is in clean status")
	git := &recordingGit{}
	w, _ := newWatcher(t, f, git)
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err == nil || !strings.Contains(err.Error(), "clean status") {
		t.Fatalf("sweep err = %v, want the arming failure", err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"pr", "comment"}) {
		t.Fatalf("writes = %v, want the revert opened and the ticket told, nothing on the suspect", got)
	}
	c := f.write("comment #7000")
	if !strings.Contains(c, "could NOT arm its auto-merge") || !strings.Contains(c, revertOpenedMarker(8001)) {
		t.Errorf("the ticket does not hand the unarmed revert to a person:\n%s", c)
	}
	f.writes = nil
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || len(git.reverts) != 1 {
		t.Fatalf("a second sweep wrote %v and reverted %v", f.kinds(), git.reverts)
	}
	recorded(t, f, redRaceRun, 1, "success")
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if pr := f.pulls[8001]; pr.State != "closed" {
		t.Fatalf("the unarmed revert after main's green run: state %q; writes %v", pr.State, f.kinds())
	}
}

// A revert the ticket never recorded (its comment failed) is still found by
// main's next green run, through its branch, closed and named on the ticket.
func TestAGreenMainClosesARevertTheTicketNeverRecorded(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644, 3655, 3688})
	redMain(t, f)
	f.failOnce["comment #7000"] = errors.New("POST /repos/SocialGouv/iterion/issues/7000/comments: HTTP 502")
	w, _ := newWatcher(t, f, &recordingGit{})
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err == nil {
		t.Fatal("the sweep hides the failed record")
	}
	for _, c := range f.comments[7000] {
		if revertMarker.MatchString(c.Body) {
			t.Fatalf("the ticket records the revert despite the injected failure:\n%s", c.Body)
		}
	}
	f.writes = nil
	recorded(t, f, redRaceRun, 1, "success")
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	if pr := f.pulls[8001]; pr.State != "closed" {
		t.Fatalf("the unrecorded revert after main's green run: state %q; writes %v", pr.State, f.kinds())
	}
	if c := f.write("comment #7000"); !strings.Contains(c, "Closed the revert https://github.com/SocialGouv/iterion/pull/8001") {
		t.Errorf("the closing comment does not name the revert:\n%s", c)
	}
}

// A revert step whose ticket record failed after the revert was armed is
// finished by the next sweep: the ticket records the revert that is open and
// armed, the suspect is told, and nothing is re-armed nor refused.
func TestARevertWhoseRecordFailedIsRecordedByTheNextSweep(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644, 3655, 3688})
	redMain(t, f)
	f.failOnce["comment #7000"] = errors.New("POST /repos/SocialGouv/iterion/issues/7000/comments: HTTP 502")
	git := &recordingGit{}
	w, _ := newWatcher(t, f, git)
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err == nil {
		t.Fatal("the sweep hides the failed record")
	}
	f.writes = nil
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"comment", "comment"}) {
		t.Fatalf("writes = %v, want the ticket and the suspect told, nothing re-armed", got)
	}
	c := f.write("comment #7000")
	if strings.Contains(c, "revert-refused") || !strings.Contains(c, revertOpenedMarker(8001)) || !strings.Contains(c, "Opened https://github.com/SocialGouv/iterion/pull/8001, reverting #3688") {
		t.Errorf("the ticket does not record the armed revert:\n%s", c)
	}
	if s := f.write("comment #3688"); !strings.Contains(s, "with auto-merge armed") {
		t.Errorf("the suspect is not told of its armed revert:\n%s", s)
	}
	if len(git.reverts) != 1 {
		t.Fatalf("reverts = %v, want the first one only", git.reverts)
	}
	f.writes = nil
	if err := w.Sweep(context.Background()); err != nil || len(f.writes) != 0 {
		t.Fatalf("a third sweep: err %v, writes %v", err, f.kinds())
	}
}

// A revert whose arming a person stopped before the next sweep recorded it is
// recorded unarmed, for a person: never re-armed, never refused.
func TestARevertDisarmedBeforeItsRecordIsHandedToAPerson(t *testing.T) {
	f := newFixture(t)
	redEpisode(t, f, []int{3644, 3655, 3688})
	redMain(t, f)
	f.failOnce["comment #7000"] = errors.New("POST /repos/SocialGouv/iterion/issues/7000/comments: HTTP 502")
	w, _ := newWatcher(t, f, &recordingGit{})
	w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
	if err := w.Sweep(context.Background()); err == nil {
		t.Fatal("the sweep hides the failed record")
	}
	pr := f.pulls[8001]
	pr.AutoMerge = nil
	f.pulls[8001] = pr
	f.writes = nil
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"comment"}) {
		t.Fatalf("writes = %v, want the ticket told only", got)
	}
	c := f.write("comment #7000")
	if strings.Contains(c, "revert-refused") || !strings.Contains(c, revertOpenedMarker(8001)) || !strings.Contains(c, "its auto-merge is not armed") {
		t.Errorf("the ticket does not hand the unarmed revert to a person:\n%s", c)
	}
}

// A revert branch whose pull request is closed or merged, or that has none,
// still refuses the revert: the earlier attempt is a person's to judge.
func TestARevertBranchWithoutAnOpenPullRequestRefuses(t *testing.T) {
	for name, state := range map[string]string{"no pull request": "", "closed": "closed"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			redEpisode(t, f, []int{3644, 3655, 3688})
			redMain(t, f)
			f.branches["main-watch/revert-3688"] = true
			if state != "" {
				pr := PullRequest{Number: 8001, State: state, HTMLURL: "https://github.com/SocialGouv/iterion/pull/8001"}
				pr.Head.Ref = "main-watch/revert-3688"
				f.pulls[8001], f.byHead[pr.Head.Ref] = pr, 8001
			}
			w, _ := newWatcher(t, f, &recordingGit{})
			w.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 39, 0, 0, time.UTC) }
			if err := w.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if c := f.write("comment #7000"); !strings.Contains(c, "revert-refused") || !strings.Contains(c, "already exists") {
				t.Errorf("the ticket does not refuse:\n%s", c)
			}
		})
	}
}

// A sub-issue filing that failed is made by the next pass: every ticket
// main-watch filed ends up under the CI-health epic, once.
func TestAFailedSubIssueFilingIsRetriedByTheNextPass(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	f.failOnce["sub-issue"] = errors.New("POST /repos/SocialGouv/iterion/issues/1410/sub_issues: HTTP 502")
	w, _ := newWatcher(t, f, &recordingGit{})
	w.ValidateQuarantine = func([]byte) error { return nil }
	if err := w.HandleRun(context.Background(), redRaceRun); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("pass 1: err = %v, want the failed filing", err)
	}
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	got := slices.Sorted(slices.Values(f.subs[flakeEpic]))
	if !slices.Equal(got, []int64{70010, 70020}) {
		t.Fatalf("sub-issues of #%d = %v, want both tickets once", flakeEpic, got)
	}
}

// A quarantine list the validator refuses fails every pass over the run the
// same way: the tickets the pass created are still filed under the epic.
func TestARefusedQuarantineListStillFilesTheTickets(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	w, _ := newWatcher(t, f, &recordingGit{})
	w.ValidateQuarantine = func([]byte) error { return errors.New("row refused") }
	if err := w.HandleRun(context.Background(), redRaceRun); err == nil || !strings.Contains(err.Error(), "row refused") {
		t.Fatalf("err = %v, want the refused quarantine list", err)
	}
	got := slices.Sorted(slices.Values(f.subs[flakeEpic]))
	if !slices.Equal(got, []int64{70010, 70020}) {
		t.Fatalf("sub-issues of #%d = %v, want both tickets: the refusal is the PR step's, not the filing's", flakeEpic, got)
	}
}

// A ticket a person filed under another issue is left there: GitHub refuses
// a second parent, so filing it would fail every later pass.
func TestAFlakeTicketUnderAnotherEpicIsLeftThere(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	f.parents[7001] = 3000
	w, _ := newWatcher(t, f, &recordingGit{})
	w.ValidateQuarantine = func([]byte) error { return nil }
	for pass := 1; pass <= 2; pass++ {
		if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if got := f.subs[flakeEpic]; !slices.Equal(got, []int64{70020}) {
		t.Fatalf("sub-issues of #%d = %v, want #7002 only: #7001 is under #3000", flakeEpic, got)
	}
}

// A flake pass that keeps failing (here every sub-issue filing: the epic is
// full) still lets a green
// rerun close the red episode and its armed revert.
func TestAFlakePassThatKeepsFailingStillClosesTheRedEpisode(t *testing.T) {
	f, w := openedRevert(t)
	quarantine := PullRequest{Number: 8002, State: "closed", HTMLURL: "https://github.com/SocialGouv/iterion/pull/8002"}
	merged := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	quarantine.MergedAt = &merged
	quarantine.Head.Ref = "main-watch/quarantine-run-45604352729"
	f.pulls[8002], f.byHead[quarantine.Head.Ref] = quarantine, 8002
	f.failAll["sub-issue"] = errors.New("POST /repos/SocialGouv/iterion/issues/1410/sub_issues: HTTP 422: Sub issue may only have one parent")
	recorded(t, f, redRaceRun, 2, "success")
	if err := w.HandleRun(context.Background(), redRaceRun); err == nil || !strings.Contains(err.Error(), "HTTP 422") {
		t.Fatalf("pass 1: err = %v, want the failed filing reported", err)
	}
	if pr := f.pulls[8001]; pr.State != "closed" {
		t.Fatalf("pass 1: armed revert #8001 is %q on a green main; writes %v", pr.State, f.kinds())
	}
	f.write("close #7000")
	// The red episode is closed: a second pass still reports the failed
	// filing and leaves the closed ticket alone.
	f.writes = nil
	if err := w.HandleRun(context.Background(), redRaceRun); err == nil || !strings.Contains(err.Error(), "HTTP 422") {
		t.Fatalf("pass 2: err = %v, want the failed filing reported", err)
	}
	if slices.ContainsFunc(f.writes, func(w string) bool {
		return strings.HasPrefix(w, "close #7000") || strings.HasPrefix(w, "comment #7000")
	}) {
		t.Fatalf("pass 2 writes on the closed red ticket #7000: %v", f.writes)
	}
}

// A flake in a gating job off the scale set (test-native-exec) is ticketed
// without a row: the nightly witness runs on the scale set, without the
// environment that job exists for.
func TestAFlakeOffTheScaleSetIsTicketedNotQuarantined(t *testing.T) {
	f := newFixture(t)
	recorded(t, f, redRaceRun, 2, "success")
	js, err := f.Jobs(context.Background(), redRaceRun, 1)
	if err != nil {
		t.Fatal(err)
	}
	js = slices.Clone(js)
	for i := range js {
		if js[i].Name == "race-shard (2)" {
			js[i].Name = "test-native-exec"
		}
	}
	f.jobsOf[fmt.Sprintf("%d/1", redRaceRun)] = js
	w, _ := newWatcher(t, f, &recordingGit{})
	w.ValidateQuarantine = func([]byte) error { return nil }
	if err := w.HandleRun(context.Background(), redRaceRun); err != nil {
		t.Fatal(err)
	}
	commit := f.write("commit")
	if strings.Contains(commit, "TestSandboxRelay_ABulkyStepStillDeliversItsTokenCounts") {
		t.Errorf("a test-native-exec flake got a quarantine row:\n%s", commit)
	}
	if !strings.Contains(commit, "TestProdWatch_SentryAnOversizedPageFloodsNoChannel\t#7001\n") {
		t.Errorf("the scale-set flake lost its row:\n%s", commit)
	}
	if c := f.write("comment #7002"); !strings.Contains(c, "not quarantined: job `test-native-exec` runs off the scale set") {
		t.Errorf("the ticket does not say why there is no row:\n%s", c)
	}
}

// Every write of the GitHub interface is replaced by the dry run: called
// through it, none reaches the client underneath, and each is printed. A
// method added to the interface is a write here until listed as a read, so a
// write without its dry-run twin fails.
func TestTheDryRunReplacesEveryWrite(t *testing.T) {
	reads := map[string]bool{
		"Run": true, "Attempt": true, "Jobs": true, "JobLog": true, "MainRuns": true, "Compare": true,
		"PullRequest": true, "OpenIssues": true, "Comments": true, "MainSHA": true, "File": true,
		"BranchExists": true, "PullRequestFrom": true, "AutoMergeHistory": true, "SubIssues": true,
		"HasParent": true,
	}
	f := newFixture(t)
	out := &bytes.Buffer{}
	d := reflect.ValueOf(newDryRun(f, out))
	iface := reflect.TypeFor[GitHub]()
	writes := 0
	for i := range iface.NumMethod() {
		m := iface.Method(i)
		if reads[m.Name] {
			continue
		}
		writes++
		fn := d.MethodByName(m.Name)
		args := make([]reflect.Value, fn.Type().NumIn())
		for j := range args {
			args[j] = reflect.Zero(fn.Type().In(j))
		}
		args[0] = reflect.ValueOf(context.Background())
		before := out.Len()
		res := fn.Call(args)
		if errv := res[len(res)-1]; !errv.IsNil() {
			t.Errorf("dry-run %s: %v", m.Name, errv.Interface())
		}
		if !strings.Contains(out.String()[before:], "DRY RUN — would") {
			t.Errorf("dry-run %s printed nothing", m.Name)
		}
	}
	if len(f.writes) != 0 {
		t.Fatalf("the dry run reached the client: %v", f.kinds())
	}
	if writes == 0 {
		t.Fatal("no write method found on the GitHub interface")
	}
}
