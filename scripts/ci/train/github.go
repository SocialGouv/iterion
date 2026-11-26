package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// GitHub is what the train reads from and does on GitHub.
type GitHub interface {
	PullRequest(ctx context.Context, n int) (PR, error)
	// CommitMessages returns the pull request's commit messages, oldest first.
	CommitMessages(ctx context.Context, n int) ([]string, error)
	// QueueEntries returns the pull requests with an entry in main's merge queue.
	QueueEntries(ctx context.Context) ([]int, error)
	// Merge squashes n onto main as an admin, only if its head is headSHA.
	Merge(ctx context.Context, n int, headSHA, subject, body string) error
	// CancelPullRequestRuns cancels the pull_request runs of sha still queued
	// or running, and returns how many it cancelled.
	CancelPullRequestRuns(ctx context.Context, sha string) (int, error)
	Comment(ctx context.Context, n int, body string) error
}

// ghCLI implements GitHub with the gh CLI the operator is logged in with.
// Reads go through REST; the merge queue has no REST surface and the admin
// merge is gh's own command, the one the landing lanes already use.
type ghCLI struct {
	Repo string // owner/name
}

func (g ghCLI) gh(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (g ghCLI) PullRequest(ctx context.Context, n int) (PR, error) {
	out, err := g.gh(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d", g.Repo, n))
	if err != nil {
		return PR{}, err
	}
	var raw struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Draft  bool   `json:"draft"`
		Merged bool   `json:"merged"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return PR{}, fmt.Errorf("pull request #%d: %w", n, err)
	}
	pr := PR{
		Number: raw.Number, Title: raw.Title, State: raw.State, Draft: raw.Draft,
		BaseRef: raw.Base.Ref, HeadRef: raw.Head.Ref, HeadSHA: raw.Head.SHA, Author: raw.User.Login,
		CrossRepo: raw.Head.Repo == nil || raw.Head.Repo.FullName != g.Repo,
	}
	if raw.Merged {
		pr.State = "merged"
	}
	for page := 1; ; page++ {
		out, err := g.gh(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100&page=%d", g.Repo, n, page))
		if err != nil {
			return PR{}, err
		}
		var files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		if err := json.Unmarshal(out, &files); err != nil {
			return PR{}, fmt.Errorf("files of #%d: %w", n, err)
		}
		for _, f := range files {
			pr.Files = append(pr.Files, f.Filename)
			if f.PreviousFilename != "" {
				pr.Files = append(pr.Files, f.PreviousFilename)
			}
		}
		if len(files) < 100 {
			break
		}
	}
	return pr, nil
}

func (g ghCLI) CommitMessages(ctx context.Context, n int) ([]string, error) {
	var msgs []string
	for page := 1; ; page++ {
		out, err := g.gh(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d/commits?per_page=100&page=%d", g.Repo, n, page))
		if err != nil {
			return nil, err
		}
		var commits []struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(out, &commits); err != nil {
			return nil, fmt.Errorf("commits of #%d: %w", n, err)
		}
		for _, c := range commits {
			msgs = append(msgs, c.Commit.Message)
		}
		if len(commits) < 100 {
			return msgs, nil
		}
	}
}

func (g ghCLI) QueueEntries(ctx context.Context) ([]int, error) {
	owner, name, _ := strings.Cut(g.Repo, "/")
	out, err := g.gh(ctx, "api", "graphql",
		"-f", "query=query($o:String!,$n:String!){repository(owner:$o,name:$n){mergeQueue(branch:\"main\"){entries(first:100){nodes{pullRequest{number}}}}}}",
		"-f", "o="+owner, "-f", "n="+name)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Data struct {
			Repository struct {
				MergeQueue *struct {
					Entries struct {
						Nodes []struct {
							PullRequest struct {
								Number int `json:"number"`
							} `json:"pullRequest"`
						} `json:"nodes"`
					} `json:"entries"`
				} `json:"mergeQueue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("merge queue: %w", err)
	}
	if raw.Data.Repository.MergeQueue == nil {
		return nil, errors.New("merge queue: main has no merge queue in the GraphQL answer — cannot prove it empty")
	}
	var prs []int
	for _, e := range raw.Data.Repository.MergeQueue.Entries.Nodes {
		prs = append(prs, e.PullRequest.Number)
	}
	return prs, nil
}

func (g ghCLI) Merge(ctx context.Context, n int, headSHA, subject, body string) error {
	_, err := g.gh(ctx, "pr", "merge", strconv.Itoa(n), "-R", g.Repo,
		"--squash", "--admin", "--match-head-commit", headSHA, "--subject", subject, "--body", body)
	return err
}

func (g ghCLI) CancelPullRequestRuns(ctx context.Context, sha string) (int, error) {
	out, err := g.gh(ctx, "api", fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&event=pull_request&per_page=100", g.Repo, sha))
	if err != nil {
		return 0, err
	}
	var raw struct {
		Runs []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return 0, fmt.Errorf("runs of %s: %w", sha, err)
	}
	n := 0
	var errs []error
	for _, r := range raw.Runs {
		if r.Status == "completed" {
			continue
		}
		if _, err := g.gh(ctx, "api", "-X", "POST", fmt.Sprintf("repos/%s/actions/runs/%d/cancel", g.Repo, r.ID)); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

func (g ghCLI) Comment(ctx context.Context, n int, body string) error {
	_, err := g.gh(ctx, "api", fmt.Sprintf("repos/%s/issues/%d/comments", g.Repo, n), "-f", "body="+body)
	return err
}
