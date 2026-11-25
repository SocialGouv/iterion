package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// dryRun reads through the real client and prints every write instead of
// making it. An issue it "creates" gets a placeholder number from 900001 up,
// so the decisions that follow (the quarantine rows, their validation) run
// exactly as they would live.
type dryRun struct {
	GitHub
	out  io.Writer
	next int
}

func newDryRun(gh GitHub, out io.Writer) *dryRun {
	return &dryRun{GitHub: gh, out: out, next: 900001}
}

func (d *dryRun) would(format string, a ...any) {
	fmt.Fprintf(d.out, "DRY RUN — would "+format+"\n", a...)
}

func indent(s string) string {
	return "    | " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    | ")
}

func (d *dryRun) RerunFailedJobs(_ context.Context, id int64) error {
	d.would("rerun the failed jobs of run %d", id)
	return nil
}

func (d *dryRun) EnsureLabel(_ context.Context, name, _, _ string) error {
	d.would("ensure label %q exists", name)
	return nil
}

func (d *dryRun) CreateIssue(_ context.Context, title, body string, labels []string) (Issue, error) {
	n := d.next
	d.next++
	d.would("open issue (placeholder #%d) %q labelled %v:\n%s", n, title, labels, indent(body))
	return Issue{ID: int64(n), Number: n, Title: title, Body: body, HTMLURL: fmt.Sprintf("(placeholder #%d)", n), User: User{Login: selfLogin}}, nil
}

func (d *dryRun) AddSubIssue(_ context.Context, epic int, issueID int64) error {
	d.would("file issue id %d as a sub-issue of #%d", issueID, epic)
	return nil
}

func (d *dryRun) Comment(_ context.Context, number int, body string) error {
	d.would("comment on #%d:\n%s", number, indent(body))
	return nil
}

func (d *dryRun) CloseIssue(_ context.Context, number int) error {
	d.would("close #%d", number)
	return nil
}

func (d *dryRun) CreateBranch(_ context.Context, branch, sha string) error {
	d.would("create branch %s at %s", branch, sha)
	return nil
}

func (d *dryRun) PutFile(_ context.Context, branch, path, message string, content []byte, _ string) error {
	d.would("commit %s on %s (%q):\n%s", path, branch, message, indent(string(content)))
	return nil
}

func (d *dryRun) CreatePullRequest(_ context.Context, head, title, body string) (PullRequest, error) {
	d.would("open a pull request %s -> main %q:\n%s", head, title, indent(body))
	return PullRequest{Number: 0, Title: title, State: "open", HTMLURL: "(pull request from " + head + ")"}, nil
}

func (d *dryRun) EnableAutoMerge(_ context.Context, pr PullRequest) error {
	d.would("arm auto-merge on %s", pr.HTMLURL)
	return nil
}

func (d *dryRun) ClosePullRequest(_ context.Context, number int) error {
	d.would("close pull request #%d", number)
	return nil
}

// dryGit prints the revert instead of making it.
type dryGit struct{ out io.Writer }

func (g dryGit) Revert(_ context.Context, sha, branch string) error {
	fmt.Fprintf(g.out, "DRY RUN — would revert %s on a new branch %s from origin/main and push it\n", sha, branch)
	return nil
}
