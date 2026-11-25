// Command mainwatch keeps main from staying red (#3692). It judges the runs
// of .github/workflows/tests.yml on main — pushes and the nightly — and acts:
//
//   - a first attempt that failed: rerun its failed jobs, once;
//   - red then green on the rerun, a FLAKE: a `flaky:` ticket per failing test
//     (under the CI-health epic, or appended to the open one) and one pull
//     request adding their rows to scripts/ci/test-quarantine.tsv, auto-merge
//     armed — only for tests of a job that skips that list; a job that failed
//     with no go test FAIL line is infrastructure and gets nothing;
//   - red again on the rerun, a REGRESSION: one "main red" ticket naming the
//     failing tests and the pull requests merged since the last green run;
//   - main still red 4 h after that run: a revert pull request of the most
//     recently merged suspect, auto-merge armed, and a comment on the
//     original — never of a revert or of a main-watch change, once per red
//     episode;
//   - a green run: the open main-red ticket closes.
//
// Usage, from the repository root:
//
//	mainwatch -run-id ID [-dry-run]   # judge one run (the workflow_run event)
//	mainwatch -sweep [-dry-run]       # judge main's newest run, then the revert clock
//
// Environment: GITHUB_API_URL, GITHUB_GRAPHQL_URL, GITHUB_REPOSITORY,
// GITHUB_SERVER_URL and GITHUB_RUN_ID (all set by Actions), GH_TOKEN (the
// job's GITHUB_TOKEN: reads, reruns, issues, comments on issues and pull
// requests) and, unless -dry-run, BOT_TOKEN (the App token that pushes
// branches and opens pull requests, so their checks run). A dry run reads everything and prints every write.
//
// Every decision is idempotent — markers in the issues it writes, branch
// names keyed by run and by pull request — so judging a run twice, or a
// sweep catching an event already handled, changes nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	runID := flag.Int64("run-id", 0, "judge this run of "+watchedPath)
	sweep := flag.Bool("sweep", false, "judge main's newest run, then the revert clock")
	dry := flag.Bool("dry-run", false, "read everything, print every write instead of making it")
	flag.Parse()
	if err := run(*runID, *sweep, *dry); err != nil {
		fmt.Fprintln(os.Stderr, "mainwatch:", err)
		os.Exit(1)
	}
}

func requireEnv(names ...string) (map[string]string, error) {
	out := map[string]string{}
	var missing []string
	for _, n := range names {
		v := os.Getenv(n)
		if v == "" {
			missing = append(missing, n)
		}
		out[n] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing environment: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func run(runID int64, sweep, dry bool) error {
	if (runID != 0) == sweep {
		return errors.New("usage: exactly one of -run-id ID or -sweep")
	}
	names := []string{"GITHUB_API_URL", "GITHUB_GRAPHQL_URL", "GITHUB_REPOSITORY", "GITHUB_SERVER_URL", "GITHUB_RUN_ID", "GH_TOKEN"}
	if !dry {
		names = append(names, "BOT_TOKEN")
	}
	env, err := requireEnv(names...)
	if err != nil {
		return err
	}
	workflow, err := os.ReadFile(watchedPath)
	if err != nil {
		return fmt.Errorf("read the watched workflow (run from the repository root): %w", err)
	}
	client := &restClient{
		api:      strings.TrimRight(env["GITHUB_API_URL"], "/"),
		graphql:  env["GITHUB_GRAPHQL_URL"],
		repo:     env["GITHUB_REPOSITORY"],
		workflow: filepath.Base(watchedPath),
		token:    env["GH_TOKEN"],
		botToken: env["BOT_TOKEN"],
		http:     &http.Client{Timeout: 2 * time.Minute},
	}
	w := &Watcher{
		GH:                 client,
		Git:                execGit{dir: "."},
		Repo:               env["GITHUB_REPOSITORY"],
		Out:                os.Stdout,
		Now:                time.Now,
		SelfURL:            fmt.Sprintf("%s/%s/actions/runs/%s", strings.TrimRight(env["GITHUB_SERVER_URL"], "/"), env["GITHUB_REPOSITORY"], env["GITHUB_RUN_ID"]),
		Workflow:           workflow,
		ValidateQuarantine: validateWithTestquarantine("."),
	}
	if dry {
		w.GH = newDryRun(client, os.Stdout)
		w.Git = dryGit{out: os.Stdout}
	}
	ctx := context.Background()
	if sweep {
		return w.Sweep(ctx)
	}
	return w.HandleRun(ctx, runID)
}

// validateWithTestquarantine runs scripts/ci/testquarantine — the list's own
// parser and guards, the ones every gating job runs — on a candidate list.
func validateWithTestquarantine(repo string) func([]byte) error {
	return func(list []byte) error {
		f, err := os.CreateTemp("", "test-quarantine-*.tsv")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(list); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		cmd := exec.Command("go", "run", "-mod=vendor", "./scripts/ci/testquarantine", "-list", f.Name(), "-skip")
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("scripts/ci/testquarantine: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// execGit reverts in the job's checkout of main, whose origin remote carries
// the bot token the checkout step persisted.
type execGit struct{ dir string }

func (g execGit) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (g execGit) Revert(ctx context.Context, sha, branch string) error {
	if _, err := g.git(ctx, "fetch", "origin", "main"); err != nil {
		return err
	}
	if _, err := g.git(ctx, "checkout", "-B", branch, "origin/main"); err != nil {
		return err
	}
	parents, err := g.git(ctx, "rev-list", "--parents", "-n", "1", sha)
	if err != nil {
		return err
	}
	args := []string{"revert", "--no-edit"}
	if len(strings.Fields(parents)) > 2 {
		args = append(args, "-m", "1")
	}
	if _, err := g.git(ctx, append(args, sha)...); err != nil {
		if _, abortErr := g.git(ctx, "revert", "--abort"); abortErr != nil {
			return fmt.Errorf("%w: %v; then %v", errRevertDoesNotApply, err, abortErr)
		}
		return fmt.Errorf("%w: %v", errRevertDoesNotApply, err)
	}
	_, err = g.git(ctx, "push", "origin", "HEAD:refs/heads/"+branch)
	return err
}
