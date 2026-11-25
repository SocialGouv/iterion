package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// watchedPath is the workflow whose main runs main-watch judges.
	watchedPath = ".github/workflows/tests.yml"
	// label marks every issue main-watch opens; the issues are found again
	// through their markers, the label is for people.
	label = "main-watch"
	// flakeEpic is the CI-health epic every flake ticket is filed under.
	flakeEpic = 1410
	// revertDelay is how long main stays red before its most recent suspect
	// is reverted.
	revertDelay = 4 * time.Hour
	// quarantineFile is scripts/ci/testquarantine's list.
	quarantineFile = "scripts/ci/test-quarantine.tsv"
	// branchPrefix names every branch main-watch pushes; a pull request from
	// one of them is main-watch's own change and is never reverted.
	branchPrefix = "main-watch/"
	// selfLogin is the author of every issue and comment main-watch writes
	// (the job's GITHUB_TOKEN). Anyone can open an issue or comment on this
	// public repository, so a marker is read only in text by this author.
	// Another workflow of this repository posting a stranger's text with its
	// own GITHUB_TOKEN would still relay a marker.
	selfLogin = "github-actions[bot]"
)

// ownIssue says whether main-watch wrote the issue, so its markers count.
func ownIssue(is Issue) bool { return is.User.Login == selfLogin }

// ownComments returns the bodies of the comments main-watch wrote.
func ownComments(cs []Comment) []string {
	var out []string
	for _, c := range cs {
		if c.User.Login == selfLogin {
			out = append(out, c.Body)
		}
	}
	return out
}

// maintainer says whether a person with write access to the repository
// filed the issue: only their `flaky:` titles name a flake ticket.
func maintainer(is Issue) bool {
	switch is.AuthorAssociation {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return false
}

// Git reverts one commit of main onto a new branch and pushes it.
type Git interface {
	Revert(ctx context.Context, sha, branch string) error
}

// errRevertDoesNotApply is a revert git refused (a conflict with what landed
// since): a person resolves it, main-watch does not.
var errRevertDoesNotApply = errors.New("the revert does not apply cleanly on main")

// Watcher holds main-watch's decisions. Every GitHub and git effect goes
// through GH and Git, so a dry run swaps the writes and nothing else.
type Watcher struct {
	GH   GitHub
	Git  Git
	Repo string
	Out  io.Writer
	Now  func() time.Time
	// SelfURL is this main-watch run, linked from everything it writes.
	SelfURL string
	// Workflow is the watched workflow's source as main holds it.
	Workflow []byte
	// ValidateQuarantine runs scripts/ci/testquarantine's own guards on a
	// candidate list.
	ValidateQuarantine func(list []byte) error

	labelled bool
}

// ensureLabel makes sure the label exists before the first issue this
// Watcher opens.
func (w *Watcher) ensureLabel(ctx context.Context) error {
	if w.labelled {
		return nil
	}
	if err := w.GH.EnsureLabel(ctx, label, "b60205", "Opened by the main-watch workflow"); err != nil {
		return err
	}
	w.labelled = true
	return nil
}

func (w *Watcher) say(format string, a ...any) {
	fmt.Fprintf(w.Out, format+"\n", a...)
}

// eligible is the trust boundary every run crosses before main-watch reads
// it as main's verdict: the watched workflow, on main, from this repository,
// started by a push or the schedule. A pull request from a fork whose branch
// is also called `main` passes a `branch=main` filter and fails here.
func (w *Watcher) eligible(r Run) error {
	switch {
	case r.Path != watchedPath:
		return fmt.Errorf("run %d is of %s, not %s", r.ID, r.Path, watchedPath)
	case r.HeadBranch != "main":
		return fmt.Errorf("run %d is on %q, not main", r.ID, r.HeadBranch)
	case r.HeadRepository.FullName != w.Repo:
		return fmt.Errorf("run %d comes from %q, not %s", r.ID, r.HeadRepository.FullName, w.Repo)
	case r.Event != "push" && r.Event != "schedule":
		return fmt.Errorf("run %d was started by %q: main-watch judges push and schedule runs only", r.ID, r.Event)
	}
	return nil
}

// HandleRun judges one run of the watched workflow by its id.
func (w *Watcher) HandleRun(ctx context.Context, id int64) error {
	run, err := w.GH.Run(ctx, id)
	if err != nil {
		return err
	}
	if err := w.eligible(run); err != nil {
		return fmt.Errorf("refused: %w", err)
	}
	return w.handle(ctx, run)
}

func (w *Watcher) handle(ctx context.Context, run Run) error {
	if run.Status != "completed" {
		w.say("run %d attempt %d is %s: nothing to judge until it completes", run.ID, run.Attempt, run.Status)
		return nil
	}
	switch run.Conclusion {
	case "success":
		if run.Attempt > 1 {
			first, err := w.GH.Attempt(ctx, run.ID, 1)
			if err != nil {
				return err
			}
			if red(first.Conclusion) {
				if err := w.flake(ctx, run); err != nil {
					return err
				}
			}
		}
		return w.green(ctx, run)
	case "failure", "timed_out":
		if run.Attempt == 1 {
			w.say("run %d (%s) failed on its first attempt: rerunning its failed jobs once", run.ID, short(run.HeadSHA))
			return w.GH.RerunFailedJobs(ctx, run.ID)
		}
		return w.regression(ctx, run)
	default:
		if run.Conclusion == "cancelled" && run.Attempt > 1 {
			first, err := w.GH.Attempt(ctx, run.ID, 1)
			if err != nil {
				return err
			}
			if red(first.Conclusion) {
				w.say("run %d (%s): its rerun was cancelled (a newer push on main cancels it through the watched workflow's concurrency group, or a person did); main-watch does not rerun it again, the newer run judges main, and this run's flake or regression signal is lost", run.ID, short(run.HeadSHA))
				return nil
			}
		}
		w.say("run %d concluded %q: no verdict on main, nothing to do", run.ID, run.Conclusion)
		return nil
	}
}

func red(conclusion string) bool { return conclusion == "failure" || conclusion == "timed_out" }

// jobFailure is one failing test and the job that saw it. A job that hung
// with no failing test in its log is one jobFailure with no package, and Hang
// says what the log shows.
type jobFailure struct {
	Failure
	Job  Job
	Hang string
}

var orphanTest = regexp.MustCompile(`Terminate orphan process: pid \(\d+\) \((\S+\.test)\)`)

// hangOf describes a job cancelled past its timeout, naming the test binary
// the runner killed as an orphan when the log shows one.
func hangOf(log string) string {
	if m := orphanTest.FindStringSubmatch(log); m != nil {
		return fmt.Sprintf("the job exceeded its timeout (a hang); `%s` was still running", m[1])
	}
	return "the job exceeded its timeout (a hang)"
}

// failures reads the failures of one red attempt from its jobs' logs: the
// failed and timed-out jobs, and the cancelled ones. GitHub concludes a job
// past its timeout-minutes as cancelled, and the watched workflow's matrices
// do not fail fast, so a cancelled job of a red attempt hung.
func (w *Watcher) failures(ctx context.Context, run Run, attempt int) ([]jobFailure, error) {
	jobs, err := w.GH.Jobs(ctx, run.ID, attempt)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(jobs, func(a, b Job) int { return strings.Compare(a.Name, b.Name) })
	var out []jobFailure
	for _, j := range jobs {
		if !red(j.Conclusion) && j.Conclusion != "cancelled" {
			continue
		}
		log, err := w.GH.JobLog(ctx, j.ID)
		if err != nil {
			return nil, err
		}
		fs := parseFailures(log)
		for _, f := range fs {
			out = append(out, jobFailure{Failure: f, Job: j})
		}
		if len(fs) == 0 && j.Conclusion == "cancelled" {
			out = append(out, jobFailure{Job: j, Hang: hangOf(log)})
		}
	}
	slices.SortStableFunc(out, func(a, b jobFailure) int {
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		return strings.Compare(a.Test, b.Test)
	})
	return out, nil
}

// flake files what a red-then-green run leaves behind: a ticket per failing
// test and one pull request adding the quarantine rows.
//
// Every step is resumable, so a pass that failed half-way is finished by the
// next one: tickets are found again by their marker, the branch's list is
// read before the rows are committed, the pull request is found by its head
// branch and armed if it is not, and a ticket comment is skipped once its run
// marker is there.
func (w *Watcher) flake(ctx context.Context, run Run) error {
	branch := fmt.Sprintf("%squarantine-run-%d", branchPrefix, run.ID)
	fails, err := w.failures(ctx, run, 1)
	if err != nil {
		return err
	}
	if len(fails) == 0 {
		w.say("run %d: attempt 1 failed with no go test FAIL line and attempt %d is green: an infrastructure failure, nothing to quarantine", run.ID, run.Attempt)
		return nil
	}
	gating, err := quarantinedJobs(w.Workflow)
	if err != nil {
		return err
	}
	mainSHA, err := w.GH.MainSHA(ctx)
	if err != nil {
		return err
	}
	list, blob, err := w.GH.File(ctx, quarantineFile, mainSHA)
	if err != nil {
		return err
	}
	listed := quarantinedRows(list)
	issues, err := w.GH.OpenIssues(ctx)
	if err != nil {
		return err
	}
	type filed struct {
		jobFailure
		ticket Issue
		row    string // the quarantine row, empty when none is added
		why    string // why no row is added
	}
	var all []filed
	var rows []string
	for _, f := range fails {
		if f.Hang != "" {
			w.say("run %d: job %s hung (%s): no test to quarantine", run.ID, f.Job.Name, f.Hang)
			continue
		}
		if f.Test == "" {
			w.say("run %d: %s failed outside any test in job %s: no test to quarantine", run.ID, f.Package, f.Job.Name)
			continue
		}
		ticket, err := w.flakeTicket(ctx, issues, run, f)
		if err != nil {
			return err
		}
		issues = append(issues, ticket)
		fl := filed{jobFailure: f, ticket: ticket}
		path := quarantinePath(f.Test)
		switch {
		case !jobIn(f.Job.Name, gating):
			fl.why = fmt.Sprintf("job `%s` does not skip the quarantine list, so a row would not take the test out of it", f.Job.Name)
		case listed[f.Package+"\t"+path]:
			fl.why = "the test is already quarantined"
		default:
			fl.row = fmt.Sprintf("%s\t%s\t#%d", f.Package, path, ticket.Number)
			listed[f.Package+"\t"+path] = true
			rows = append(rows, fl.row)
		}
		all = append(all, fl)
	}
	var pr *PullRequest
	prNote := ""
	if len(rows) > 0 {
		msg := fmt.Sprintf("test(quarantine): quarantine %d flaky test(s) seen on main run %d", len(rows), run.ID)
		exists, err := w.GH.BranchExists(ctx, branch)
		if err != nil {
			return err
		}
		base, baseBlob := list, blob
		if exists {
			// An earlier pass created the branch; its commit may be missing.
			base, baseBlob, err = w.GH.File(ctx, quarantineFile, branch)
			if err != nil {
				return err
			}
		}
		onBranch := quarantinedRows(base)
		var missing []string
		for _, r := range rows {
			f := strings.SplitN(r, "\t", 3)
			if !onBranch[f[0]+"\t"+f[1]] {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			next := appendRows(base, missing)
			if err := w.ValidateQuarantine(next); err != nil {
				return fmt.Errorf("the quarantine list with %d new row(s) is refused: %w", len(missing), err)
			}
			if !exists {
				if err := w.GH.CreateBranch(ctx, branch, mainSHA); err != nil {
					return err
				}
			}
			if err := w.GH.PutFile(ctx, branch, quarantineFile, msg, next, baseBlob); err != nil {
				return err
			}
		}
		opened, found, err := w.GH.PullRequestFrom(ctx, branch)
		if err != nil {
			return err
		}
		if !found {
			var b strings.Builder
			fmt.Fprintf(&b, "Run %s failed on main, then passed when its failed jobs were rerun: these tests flake. Each row takes one out of the gating jobs into the nightly quarantine witness, under its ticket.\n\n", run.HTMLURL)
			for _, fl := range all {
				if fl.row != "" {
					fmt.Fprintf(&b, "- `%s` `%s` — #%d (job [%s](%s))\n", fl.Package, quarantinePath(fl.Test), fl.ticket.Number, fl.Job.Name, fl.Job.HTMLURL)
				}
			}
			fmt.Fprintf(&b, "\nOpened by main-watch: %s\n", w.SelfURL)
			if opened, err = w.GH.CreatePullRequest(ctx, branch, msg, b.String()); err != nil {
				return err
			}
		}
		switch {
		case opened.MergedAt != nil:
			prNote = "merged"
			pr = &opened
			w.say("run %d: %s is merged, its flakes are filed", run.ID, opened.HTMLURL)
		case opened.State == "closed":
			w.say("run %d: %s was closed unmerged: main-watch does not reopen it", run.ID, opened.HTMLURL)
			for i := range all {
				if all[i].row != "" {
					all[i].why = fmt.Sprintf("its quarantine pull request %s was closed unmerged", opened.HTMLURL)
					all[i].row = ""
				}
			}
		case opened.AutoMerge == nil:
			if err := w.GH.EnableAutoMerge(ctx, opened); err != nil {
				return err
			}
			prNote = "auto-merge armed"
			pr = &opened
		default:
			prNote = "auto-merge armed"
			pr = &opened
			w.say("run %d: %s exists with auto-merge armed, its flakes are already filed", run.ID, opened.HTMLURL)
		}
	}
	// One comment per ticket, listing every failure of this run it owns: two
	// failing subtests of one test share their ticket.
	var order []int
	byTicket := map[int][]filed{}
	for _, fl := range all {
		if _, seen := byTicket[fl.ticket.Number]; !seen {
			order = append(order, fl.ticket.Number)
		}
		byTicket[fl.ticket.Number] = append(byTicket[fl.ticket.Number], fl)
	}
	for _, n := range order {
		owned := byTicket[n]
		comments, err := w.GH.Comments(ctx, n)
		if err != nil {
			return err
		}
		if handled(owned[0].ticket, comments, run) {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Failed on main in run %s (commit %s), then passed on attempt %d:\n\n", run.HTMLURL, short(run.HeadSHA), run.Attempt)
		for _, fl := range owned {
			fmt.Fprintf(&b, "- `%s` in job [%s](%s): ", fl.Test, fl.Job.Name, fl.Job.HTMLURL)
			if fl.row != "" && pr != nil {
				fmt.Fprintf(&b, "quarantined by %s (%s), row `%s`\n", pr.HTMLURL, prNote, strings.ReplaceAll(fl.row, "\t", " "))
			} else {
				fmt.Fprintf(&b, "not quarantined: %s\n", fl.why)
			}
		}
		fmt.Fprintf(&b, "\nmain-watch: %s\n%s\n", w.SelfURL, runMarker(run))
		if err := w.GH.Comment(ctx, n, b.String()); err != nil {
			return err
		}
	}
	return nil
}

var flakeMarker = regexp.MustCompile(`<!-- main-watch:flake (\S+) (\S+) -->`)

// flakeTicket finds the open ticket of a flaking test — one main-watch filed
// (its marker names the package and the test) or one a maintainer filed (a
// `flaky:` title naming the top-level test) — or files it under the CI-health
// epic. Several matches resolve to the lowest number: the oldest ticket.
func (w *Watcher) flakeTicket(ctx context.Context, issues []Issue, run Run, f jobFailure) (Issue, error) {
	top := strings.SplitN(f.Test, "/", 2)[0]
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(top) + `\b`)
	var byMarker, byTitle []Issue
	for _, is := range issues {
		if m := flakeMarker.FindStringSubmatch(is.Body); m != nil && ownIssue(is) && m[1] == f.Package && m[2] == f.Test {
			byMarker = append(byMarker, is)
		} else if strings.HasPrefix(is.Title, "flaky:") && maintainer(is) && word.MatchString(is.Title) {
			byTitle = append(byTitle, is)
		}
	}
	lowest := func(is []Issue) Issue {
		return slices.MinFunc(is, func(a, b Issue) int { return a.Number - b.Number })
	}
	if len(byMarker) > 0 {
		return lowest(byMarker), nil
	}
	if len(byTitle) > 0 {
		return lowest(byTitle), nil
	}
	if err := w.ensureLabel(ctx); err != nil {
		return Issue{}, err
	}
	title := fmt.Sprintf("flaky: %s %s", strings.TrimPrefix(f.Package, "github.com/SocialGouv/iterion/"), f.Test)
	body := fmt.Sprintf("main-watch saw `%s` (`%s`) fail on main in job [%s](%s) of run %s (commit %s), then pass when the failed jobs were rerun.\n\n"+
		"Fix the test, remove its row from `%s` in the same change, and close this issue: the nightly quarantine witness reddens on a row whose ticket is closed.\n\n"+
		"Opened by main-watch: %s\n\n<!-- main-watch:flake %s %s -->\n",
		f.Test, f.Package, f.Job.Name, f.Job.HTMLURL, run.HTMLURL, short(run.HeadSHA), quarantineFile, w.SelfURL, f.Package, f.Test)
	is, err := w.GH.CreateIssue(ctx, title, body, []string{label})
	if err != nil {
		return Issue{}, err
	}
	if err := w.GH.AddSubIssue(ctx, flakeEpic, is.ID); err != nil {
		return Issue{}, err
	}
	w.say("run %d: filed #%d for %s %s", run.ID, is.Number, f.Package, f.Test)
	return is, nil
}

// redState is what a main-red ticket records for the revert check, in its
// body's marker: the facts it needs travel with the ticket.
type redState struct {
	Run        int64     `json:"run"`
	SHA        string    `json:"sha"`
	Since      time.Time `json:"since"`
	GreenSHA   string    `json:"green_sha,omitempty"`
	Suspects   []int     `json:"suspects"`
	Revertable bool      `json:"revertable"`
}

var redMarker = regexp.MustCompile(`<!-- main-watch:red (\{.*?\}) -->`)

// redTicket returns the open main-red ticket main-watch filed and its state,
// or ok=false. Two open ones (a lost race) resolve to the lowest number.
func redTicket(issues []Issue) (Issue, redState, bool, error) {
	var found []Issue
	for _, is := range issues {
		if ownIssue(is) && redMarker.MatchString(is.Body) {
			found = append(found, is)
		}
	}
	if len(found) == 0 {
		return Issue{}, redState{}, false, nil
	}
	is := slices.MinFunc(found, func(a, b Issue) int { return a.Number - b.Number })
	var st redState
	if err := json.Unmarshal([]byte(redMarker.FindStringSubmatch(is.Body)[1]), &st); err != nil {
		return Issue{}, redState{}, false, fmt.Errorf("main-red ticket #%d: unreadable marker: %w", is.Number, err)
	}
	return is, st, true, nil
}

func runMarker(run Run) string {
	return fmt.Sprintf("<!-- main-watch:run %d/%d -->", run.ID, run.Attempt)
}

// handled says whether an issue already records this run attempt, in its
// body or a comment main-watch wrote.
func handled(is Issue, comments []Comment, run Run) bool {
	m := runMarker(run)
	if ownIssue(is) && strings.Contains(is.Body, m) {
		return true
	}
	for _, c := range ownComments(comments) {
		if strings.Contains(c, m) {
			return true
		}
	}
	return false
}

// regression files a run that stayed red on its rerun: a main-red ticket
// naming the failing tests and the suspects, or a comment on the one open.
func (w *Watcher) regression(ctx context.Context, run Run) error {
	issues, err := w.GH.OpenIssues(ctx)
	if err != nil {
		return err
	}
	ticket, _, open, err := redTicket(issues)
	if err != nil {
		return err
	}
	if open {
		comments, err := w.GH.Comments(ctx, ticket.Number)
		if err != nil {
			return err
		}
		if handled(ticket, comments, run) {
			w.say("run %d: already recorded on #%d", run.ID, ticket.Number)
			return nil
		}
		fails, err := w.failures(ctx, run, run.Attempt)
		if err != nil {
			return err
		}
		body := fmt.Sprintf("Still red on run %s (commit %s), after its rerun.\n\n%s\nmain-watch: %s\n%s\n", run.HTMLURL, short(run.HeadSHA), failureList(fails), w.SelfURL, runMarker(run))
		w.say("run %d: main still red, recorded on #%d", run.ID, ticket.Number)
		return w.GH.Comment(ctx, ticket.Number, body)
	}
	fails, err := w.failures(ctx, run, run.Attempt)
	if err != nil {
		return err
	}
	st := redState{Run: run.ID, SHA: run.HeadSHA, Since: run.CreatedAt, Suspects: []int{}}
	var suspects string
	green, found, err := w.lastGreen(ctx, run)
	if err != nil {
		return err
	}
	if found {
		commits, err := w.GH.Compare(ctx, green.HeadSHA, run.HeadSHA)
		if err != nil {
			return err
		}
		st.GreenSHA = green.HeadSHA
		st.Suspects = suspectPRs(commits)
		suspects = fmt.Sprintf("Merged since the last green run %s (commit %s):", green.HTMLURL, short(green.HeadSHA))
		for _, n := range st.Suspects {
			suspects += fmt.Sprintf(" #%d", n)
		}
		if len(st.Suspects) == 0 {
			suspects += " no pull request"
		}
	} else {
		suspects = "No green run of main among the last completed ones: the suspects are unknown."
	}
	testsNamed := slices.ContainsFunc(fails, func(f jobFailure) bool { return f.Package != "" })
	hung := slices.ContainsFunc(fails, func(f jobFailure) bool { return f.Hang != "" })
	st.Revertable = found && len(st.Suspects) > 0 && testsNamed
	revert := fmt.Sprintf("If main is still red %s after this run started, main-watch opens a revert of the most recently merged suspect, with auto-merge armed.", revertDelay)
	if !st.Revertable {
		revert = "main-watch will NOT revert anything: "
		switch {
		case !testsNamed && hung:
			revert += "a job hung and no failing test or package is named in the logs, so main-watch cannot tell which change to blame."
		case !testsNamed:
			revert += "no failing test or package is named in the logs (an infrastructure failure, twice), so no change is to blame."
		default:
			revert += "no suspect pull request is known."
		}
	}
	state, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := w.ensureLabel(ctx); err != nil {
		return err
	}
	title := fmt.Sprintf("main red: 🧪 Tests failing on main since %s", short(run.HeadSHA))
	body := fmt.Sprintf("Run %s failed on main and again on its rerun: a regression, not a flake.\n\n%s\n%s\n\n%s\n\nThis ticket closes itself on the next green run of main.\n\nOpened by main-watch: %s\n\n<!-- main-watch:red %s -->\n%s\n",
		run.HTMLURL, failureList(fails), suspects, revert, w.SelfURL, state, runMarker(run))
	is, err := w.GH.CreateIssue(ctx, title, body, []string{label})
	if err != nil {
		return err
	}
	w.say("run %d: main red, filed #%d (suspects %v)", run.ID, is.Number, st.Suspects)
	return nil
}

func failureList(fails []jobFailure) string {
	if len(fails) == 0 {
		return "No go test FAIL line in the failed jobs' logs: an infrastructure or workflow failure.\n"
	}
	var b strings.Builder
	b.WriteString("Failing:\n")
	for _, f := range fails {
		if f.Hang != "" {
			fmt.Fprintf(&b, "- job [%s](%s) hung: %s\n", f.Job.Name, f.Job.HTMLURL, f.Hang)
			continue
		}
		test := f.Test
		if test == "" {
			test = "(the package itself: build, setup or a panic outside a test)"
		}
		fmt.Fprintf(&b, "- `%s` `%s` — job [%s](%s)\n", f.Package, test, f.Job.Name, f.Job.HTMLURL)
	}
	return b.String()
}

var prRef = regexp.MustCompile(`\(#([1-9][0-9]*)\)\s*$`)

// suspectPRs lists the pull requests a range of main's commits landed, each
// once, from the `(#N)` a squash merge appends to its subject line. Commits
// without one (a release, a direct push) name no pull request.
func suspectPRs(commits []Commit) []int {
	var out []int
	for _, c := range commits {
		subject := strings.SplitN(c.Message, "\n", 2)[0]
		if m := prRef.FindStringSubmatch(subject); m != nil {
			n, err := strconv.Atoi(m[1])
			if err == nil && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	slices.Sort(out)
	return out
}

// mainRuns lists the eligible runs of main, whatever their status, newest
// first by run number — the workflow's own monotonic counter, never the
// listing's order.
func (w *Watcher) mainRuns(ctx context.Context) ([]Run, error) {
	all, err := w.GH.MainRuns(ctx)
	if err != nil {
		return nil, err
	}
	var out []Run
	for _, r := range all {
		if w.eligible(r) == nil {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Run) int { return b.RunNumber - a.RunNumber })
	return out, nil
}

// verdict says whether a run's latest attempt says something about main:
// completed green or red, not cancelled, skipped or still running.
func verdict(r Run) bool {
	return r.Status == "completed" && (r.Conclusion == "success" || red(r.Conclusion))
}

// lastGreen is the newest green run of main started before run.
func (w *Watcher) lastGreen(ctx context.Context, run Run) (Run, bool, error) {
	runs, err := w.mainRuns(ctx)
	if err != nil {
		return Run{}, false, err
	}
	for _, r := range runs {
		if verdict(r) && r.Conclusion == "success" && r.RunNumber < run.RunNumber {
			return r, true, nil
		}
	}
	return Run{}, false, nil
}

// green closes the open main-red ticket when a run started no earlier than
// the red one it records comes back green, and first closes the revert pull
// request the episode opened if it is still open: main is green without it.
func (w *Watcher) green(ctx context.Context, run Run) error {
	issues, err := w.GH.OpenIssues(ctx)
	if err != nil {
		return err
	}
	ticket, st, open, err := redTicket(issues)
	if err != nil || !open {
		return err
	}
	if run.CreatedAt.Before(st.Since) {
		w.say("run %d is green but started before the red run %d of #%d: main is not known green", run.ID, st.Run, ticket.Number)
		return nil
	}
	comments, err := w.GH.Comments(ctx, ticket.Number)
	if err != nil {
		return err
	}
	reverted := ""
	for _, c := range ownComments(comments) {
		m := revertMarker.FindStringSubmatch(c)
		if m == nil || m[1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return fmt.Errorf("#%d: revert marker %q: %w", ticket.Number, m[0], err)
		}
		pr, err := w.GH.PullRequest(ctx, n)
		if err != nil {
			return err
		}
		switch {
		case pr.MergedAt != nil:
			reverted += fmt.Sprintf(" The revert %s is already merged.", pr.HTMLURL)
		case pr.State != "open":
			reverted += fmt.Sprintf(" The revert %s is already closed.", pr.HTMLURL)
		default:
			if err := w.GH.ClosePullRequest(ctx, n); err != nil {
				return err
			}
			if err := w.GH.Comment(ctx, n, fmt.Sprintf("main is green again without this revert: run %s (commit %s). main-watch closes it; the reverted change stays.\n\nmain-watch: %s\n", run.HTMLURL, short(run.HeadSHA), w.SelfURL)); err != nil {
				return err
			}
			w.say("run %d: main green again, closed the revert %s", run.ID, pr.HTMLURL)
			reverted += fmt.Sprintf(" Closed the revert %s, no longer needed.", pr.HTMLURL)
		}
	}
	body := fmt.Sprintf("main is green again: run %s (commit %s).%s Closing.\n\nmain-watch: %s\n%s\n", run.HTMLURL, short(run.HeadSHA), reverted, w.SelfURL, runMarker(run))
	if err := w.GH.Comment(ctx, ticket.Number, body); err != nil {
		return err
	}
	w.say("run %d: main green again, closing #%d", run.ID, ticket.Number)
	return w.GH.CloseIssue(ctx, ticket.Number)
}

// revertMarker records the revert step of a red episode: opened (with the
// revert pull request's number) or refused.
var revertMarker = regexp.MustCompile(`<!-- main-watch:revert-(?:opened #([1-9][0-9]*)|refused) -->`)

func revertOpenedMarker(n int) string {
	return fmt.Sprintf("<!-- main-watch:revert-opened #%d -->", n)
}

// Sweep is the scheduled pass. It judges main's newest run (a workflow_run
// event main-watch missed is caught here, and judging a run twice changes
// nothing), then reads main's newest verdict — the newest run that completed
// green or red: green closes the red episode, red past revertDelay reverts.
func (w *Watcher) Sweep(ctx context.Context) error {
	runs, err := w.mainRuns(ctx)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		w.say("no run of %s on main", watchedPath)
		return nil
	}
	newest := runs[0]
	if err := w.handle(ctx, newest); err != nil {
		return err
	}
	at := slices.IndexFunc(runs, verdict)
	if at < 0 {
		w.say("no run of %s on main completed green or red", watchedPath)
		return nil
	}
	latest := runs[at]
	if latest.Conclusion == "success" {
		if latest.ID == newest.ID {
			return nil // handle closed the episode
		}
		return w.green(ctx, latest)
	}
	issues, err := w.GH.OpenIssues(ctx)
	if err != nil {
		return err
	}
	ticket, st, open, err := redTicket(issues)
	if err != nil || !open {
		return err
	}
	comments, err := w.GH.Comments(ctx, ticket.Number)
	if err != nil {
		return err
	}
	for _, c := range ownComments(comments) {
		if revertMarker.MatchString(c) {
			w.say("#%d: the revert step already ran for this red episode", ticket.Number)
			return nil
		}
	}
	if age := w.Now().Sub(st.Since); age < revertDelay {
		w.say("#%d: main red for %s, the revert waits until %s", ticket.Number, age.Round(time.Minute), revertDelay)
		return nil
	}
	return w.revert(ctx, ticket, st)
}

func (w *Watcher) refuseRevert(ctx context.Context, ticket Issue, reason string) error {
	w.say("#%d: no revert: %s", ticket.Number, reason)
	body := fmt.Sprintf("main has been red for more than %s and main-watch does NOT revert: %s A person takes it from here.\n\nmain-watch: %s\n<!-- main-watch:revert-refused -->\n", revertDelay, reason, w.SelfURL)
	return w.GH.Comment(ctx, ticket.Number, body)
}

// mainWatchChange says whether a pull request is a revert or one of
// main-watch's own: neither is ever reverted, so a red revert cannot start a
// revert loop.
func mainWatchChange(pr PullRequest) bool {
	t := strings.ToLower(pr.Title)
	return strings.HasPrefix(pr.Head.Ref, branchPrefix) || strings.HasPrefix(t, "revert") || strings.HasPrefix(pr.Head.Ref, "revert-")
}

func (w *Watcher) revert(ctx context.Context, ticket Issue, st redState) error {
	if !st.Revertable {
		return w.refuseRevert(ctx, ticket, "the red run named no failing test, or no suspect pull request is known.")
	}
	var merged []PullRequest
	var notPRs []int
	for _, n := range st.Suspects {
		pr, err := w.GH.PullRequest(ctx, n)
		if he := (*httpError)(nil); errors.As(err, &he) && he.Status == http.StatusNotFound {
			notPRs = append(notPRs, n)
			continue
		}
		if err != nil {
			return err
		}
		if pr.MergedAt != nil && pr.MergeCommitSHA != "" {
			merged = append(merged, pr)
		}
	}
	// A `(#N)` naming an issue is a commit that landed outside a pull
	// request: its landing time is unknown, so the most recent suspect is too.
	if len(notPRs) > 0 {
		verb := "is not a pull request"
		if len(notPRs) > 1 {
			verb = "are not pull requests"
		}
		return w.refuseRevert(ctx, ticket, fmt.Sprintf("%s %s: a commit landed outside one, and main-watch cannot rank it against the merged suspects.", suspectList(notPRs), verb))
	}
	if len(merged) == 0 {
		return w.refuseRevert(ctx, ticket, "none of the suspects is a merged pull request.")
	}
	// The most recent suspect by merge time, the number breaking a tie.
	suspect := slices.MaxFunc(merged, func(a, b PullRequest) int {
		if c := a.MergedAt.Compare(*b.MergedAt); c != 0 {
			return c
		}
		return a.Number - b.Number
	})
	if mainWatchChange(suspect) {
		return w.refuseRevert(ctx, ticket, fmt.Sprintf("the most recent suspect #%d is itself a revert or a main-watch change.", suspect.Number))
	}
	branch := fmt.Sprintf("%srevert-%d", branchPrefix, suspect.Number)
	exists, err := w.GH.BranchExists(ctx, branch)
	if err != nil {
		return err
	}
	if exists {
		return w.refuseRevert(ctx, ticket, fmt.Sprintf("branch %s already exists: an earlier revert attempt of #%d did not finish.", branch, suspect.Number))
	}
	if err := w.Git.Revert(ctx, suspect.MergeCommitSHA, branch); err != nil {
		if errors.Is(err, errRevertDoesNotApply) {
			return w.refuseRevert(ctx, ticket, fmt.Sprintf("reverting #%d (%s): %v.", suspect.Number, short(suspect.MergeCommitSHA), err))
		}
		return err
	}
	title := "revert: " + suspect.Title
	body := fmt.Sprintf("Reverts #%d (%s).\n\nmain has been red for more than %s (#%d, first red run %d on %s), and #%d is the most recently merged of the pull requests that landed since the last green run (%s). Merge armed: the queue lands it once its checks pass. To keep #%d, fix main: main-watch closes this pull request on main's next green run.\n\nOpened by main-watch: %s\n",
		suspect.Number, short(suspect.MergeCommitSHA), revertDelay, ticket.Number, st.Run, short(st.SHA), suspect.Number, suspectList(st.Suspects), suspect.Number, w.SelfURL)
	pr, err := w.GH.CreatePullRequest(ctx, branch, title, body)
	if err != nil {
		return err
	}
	// The ticket records the revert before it is armed: green closes the
	// pull requests these markers name, so a write failing after this one
	// still leaves an armed revert that main's next green run closes.
	if err := w.GH.Comment(ctx, ticket.Number, fmt.Sprintf("Opened %s, reverting #%d, the most recently merged suspect.\n\nmain-watch: %s\n%s\n", pr.HTMLURL, suspect.Number, w.SelfURL, revertOpenedMarker(pr.Number))); err != nil {
		return err
	}
	if err := w.GH.EnableAutoMerge(ctx, pr); err != nil {
		return err
	}
	if err := w.GH.Comment(ctx, suspect.Number, fmt.Sprintf("main has been red for more than %s since this pull request landed (#%d): main-watch opened its revert, %s, with auto-merge armed. If a fix turns main green first, main-watch closes the revert.\n\nmain-watch: %s\n", revertDelay, ticket.Number, pr.HTMLURL, w.SelfURL)); err != nil {
		return err
	}
	w.say("#%d: opened %s reverting #%d", ticket.Number, pr.HTMLURL, suspect.Number)
	return nil
}

func suspectList(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// quarantinedRows indexes the list's rows by `package<TAB>test`.
func quarantinedRows(list []byte) map[string]bool {
	rows := map[string]bool{}
	for _, line := range strings.Split(string(list), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) >= 2 {
			rows[f[0]+"\t"+f[1]] = true
		}
	}
	return rows
}

func appendRows(list []byte, rows []string) []byte {
	out := string(list)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return []byte(out + strings.Join(rows, "\n") + "\n")
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}
