// Command train lands a lot of ready pull requests on ONE local gate instead
// of one CI cycle each (#3736), and isolates the culprit when the lot is red.
//
//	train plan  N [N…]                 check the lot, print it, change nothing
//	train run   [flags] N [N…]         compose, gate, bisect; evidence + lot.json
//	train land  -id ID [-dry-run]      land a green lot, verify main's tree per merge
//	train clean -id ID                 remove a train's worktree, keep its evidence
//
// run: a fresh worktree on origin/main under <repo>/.iterion/ci-train/<id>/,
// one `merge --squash` per pull request in the order given. A conflict on a
// derived file (the committed-reading golden, the repository maps) is
// resolved by its generator; any other conflict ejects the pull request.
// Then scripts/ci/gate.sh on the lot's head. Exit 1 → the red members rerun
// once (green again = a flake, recorded), are probed on the base (red there =
// main's, not the lot's), and the lot's prefixes are bisected on the rest:
// the first pull request that turns them red is ejected and the remaining lot
// is gated in full again. Exit 2 (a refusal) and 3 (incomplete) are not
// verdicts and stop the train.
//
// land: refuses unless the lot is green, main is still its base, the merge
// queue is empty and every head is the gated one. Then one
// `gh pr merge --squash --admin --match-head-commit` per pull request, and
// after each: main's new tip must be a child of the previous tip and carry
// the gated tree, or the landing stops. A pull request whose squash would not
// reproduce the gated step (a derived file the train regenerated) first gets
// a merge commit carrying the train's resolution on its branch.
//
// Run it from the repository, through devbox (task, go, yq and pnpm come
// from there), outside any sandbox that keeps .git/worktrees read-only, and
// in a unit that outlives the shell: a full gate takes tens of minutes.
//
// Exit codes: 0 green (run) or landed (land); 1 red, empty or refused to
// land; 2 a precondition refused (the gate's, the lot's, the tool's) — no
// verdict.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

func main() {
	err := dispatch(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "ci:train:", err)
	var ee exitError
	if errors.As(err, &ee) {
		os.Exit(ee.code)
	}
	os.Exit(2)
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return exitError{2, errors.New("usage: train plan|run|land|clean … (see the package doc)")}
	}
	switch args[0] {
	case "plan":
		return cmdPlan(ctx, args[1:], stdout)
	case "run":
		return cmdRun(ctx, args[1:], stdout, stderr)
	case "land":
		return cmdLand(ctx, args[1:], stderr)
	case "clean":
		return cmdClean(ctx, args[1:], stderr)
	default:
		return exitError{2, fmt.Errorf("unknown verb %q: plan, run, land or clean", args[0])}
	}
}

func parsePRs(args []string) ([]int, error) {
	var ns []int
	for _, a := range args {
		n, err := strconv.Atoi(strings.TrimPrefix(a, "#"))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%q is not a pull request number", a)
		}
		ns = append(ns, n)
	}
	if len(ns) == 0 {
		return nil, errors.New("no pull request given")
	}
	return ns, nil
}

func loadPRs(ctx context.Context, gh GitHub, ns []int) ([]PR, error) {
	prs := make([]PR, 0, len(ns))
	for _, n := range ns {
		pr, err := gh.PullRequest(ctx, n)
		if err != nil {
			return nil, err
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

// trainRoot is <main checkout>/.iterion/ci-train/<id>: the evidence must
// outlive the worktree, and the main checkout outlives any worktree the
// command was run from.
func trainRoot(ctx context.Context, g *gitRunner, id string) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	common, err := g.run(ctx, wd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(strings.TrimSpace(common)), ".iterion", "ci-train", id), nil
}

func validID(id string) error {
	if id == "" || strings.ContainsAny(id, "/\\ ") || strings.HasPrefix(id, ".") {
		return fmt.Errorf("train id %q: one path segment, no leading dot", id)
	}
	return nil
}

func cmdPlan(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	repo := fs.String("repo", "SocialGouv/iterion", "owner/name")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	ns, err := parsePRs(fs.Args())
	if err != nil {
		return exitError{2, err}
	}
	prs, err := loadPRs(ctx, ghCLI{Repo: *repo}, ns)
	if err != nil {
		return exitError{2, err}
	}
	for _, pr := range prs {
		var derived []string
		for _, f := range pr.Files {
			if isDerived(f) {
				derived = append(derived, f)
			}
		}
		fmt.Fprintf(stdout, "#%d %s — %s, head %s, %d file(s)", pr.Number, pr.Title, pr.State, short(pr.HeadSHA), len(pr.Files))
		if len(derived) > 0 {
			fmt.Fprintf(stdout, ", derived: %s", strings.Join(derived, ", "))
		}
		if s := soloPaths(pr); len(s) > 0 {
			fmt.Fprintf(stdout, ", CI: %s", strings.Join(s, ", "))
		}
		fmt.Fprintln(stdout)
	}
	if err := checkLot(prs); err != nil {
		return exitError{1, err}
	}
	fmt.Fprintf(stdout, "lot of %d: ready to run\n", len(prs))
	return nil
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	repo := fs.String("repo", "SocialGouv/iterion", "owner/name")
	remote := fs.String("remote", "origin", "the git remote of -repo")
	id := fs.String("id", "t"+time.Now().UTC().Format("20060102T150405Z"), "the train's id: its directory under .iterion/ci-train/")
	maxEject := fs.Int("max-eject", 3, "stop after this many pull requests ejected by bisection (0 = no bound)")
	accept := fs.String("accept-base-red", "", "comma-separated gate members red on main itself that may stay red on the lot")
	setup := fs.String("setup", "pnpm install --frozen-lockfile", "shell command run once in the fresh worktree before the first gate (empty = none)")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if err := validID(*id); err != nil {
		return exitError{2, err}
	}
	ns, err := parsePRs(fs.Args())
	if err != nil {
		return exitError{2, err}
	}
	gh := ghCLI{Repo: *repo}
	prs, err := loadPRs(ctx, gh, ns)
	if err != nil {
		return exitError{2, err}
	}
	if err := checkLot(prs); err != nil {
		return exitError{2, err}
	}
	g := &gitRunner{}
	root, err := trainRoot(ctx, g, *id)
	if err != nil {
		return exitError{2, err}
	}
	if _, err := os.Stat(root); err == nil {
		return exitError{2, fmt.Errorf("%s exists: a train id is used once", root)}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return exitError{2, err}
	}
	wd, _ := os.Getwd()
	if _, err := g.run(ctx, wd, "fetch", "--quiet", *remote, "main"); err != nil {
		return exitError{2, err}
	}
	base, err := g.revParse(ctx, wd, "FETCH_HEAD")
	if err != nil {
		return exitError{2, err}
	}
	dir := filepath.Join(root, "tree")
	if _, err := g.run(ctx, wd, "worktree", "add", "--quiet", "--detach", dir, base); err != nil {
		return exitError{2, err}
	}
	// From here on the train owns a registered worktree: every failure says
	// how to drop it, since each registered worktree costs a sandboxed
	// session a deny path.
	withClean := func(err error) error {
		return fmt.Errorf("%w — its worktree stays for inspection; drop it with `task ci:train -- clean -id %s`", err, *id)
	}
	for _, pr := range prs {
		if _, err := g.run(ctx, dir, "fetch", "--quiet", *remote, fmt.Sprintf("pull/%d/head", pr.Number)); err != nil {
			return exitError{2, withClean(err)}
		}
		got, err := g.revParse(ctx, dir, "FETCH_HEAD")
		if err != nil {
			return exitError{2, withClean(err)}
		}
		if got != pr.HeadSHA {
			return exitError{2, withClean(fmt.Errorf("#%d: fetched head %s, GitHub said %s — it moved; run again", pr.Number, short(got), short(pr.HeadSHA)))}
		}
	}
	t := &Train{
		ID: *id, Dir: dir, Root: root, Git: g, Log: stderr, MaxEject: *maxEject,
		Gate:  scriptGate{Log: stderr},
		Regen: commandRegen{Commands: defaultGenerators, Log: stderr},
	}
	if *accept != "" {
		t.AcceptBaseRed = strings.Split(*accept, ",")
	}
	if *setup != "" {
		t.Setup = func(ctx context.Context, dir string) error {
			cmd := exec.CommandContext(ctx, "bash", "-c", *setup)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "COREPACK_HOME="+filepath.Join(root, "corepack"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w: %s", *setup, err, tail(string(out), 20))
			}
			return nil
		}
	}
	rep, runErr := t.Run(ctx, base, prs)
	if rep != nil {
		if err := writeReport(filepath.Join(root, "lot.json"), rep); err != nil {
			return exitError{2, withClean(errors.Join(runErr, err))}
		}
		printSummary(stdout, rep)
	}
	if runErr != nil {
		code := 1
		if rep == nil || rep.Verdict == verdictRefused || rep.Verdict == verdictIncomplete || rep.Verdict == "" {
			code = 2
		}
		return exitError{code, withClean(runErr)}
	}
	if rep.Verdict != verdictGreen {
		return exitError{1, withClean(fmt.Errorf("verdict %s: nothing to land", rep.Verdict))}
	}
	return nil
}

func printSummary(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "train %s on %s: %s\n", rep.ID, short(rep.Base), rep.Verdict)
	for _, s := range rep.Steps {
		fmt.Fprintf(w, "  land  #%d %s%s\n", s.PR.Number, s.PR.Title, regenNote(s.Regenerated))
	}
	for _, e := range rep.Ejected {
		fmt.Fprintf(w, "  eject #%d: %s\n", e.PR, e.Reason)
	}
	if len(rep.Flakes) > 0 {
		fmt.Fprintf(w, "  flakes: %s\n", strings.Join(rep.Flakes, ", "))
	}
	if len(rep.BaseRed) > 0 {
		fmt.Fprintf(w, "  red on main too, accepted: %s\n", strings.Join(rep.BaseRed, ", "))
	}
	fmt.Fprintf(w, "  evidence: %s\n", rep.Evidence)
	if rep.Verdict == verdictGreen {
		fmt.Fprintf(w, "  next: train land -id %s (on an empty merge queue)\n", rep.ID)
	}
}

func cmdLand(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("land", flag.ContinueOnError)
	repo := fs.String("repo", "SocialGouv/iterion", "owner/name")
	remote := fs.String("remote", "origin", "the git remote of -repo")
	id := fs.String("id", "", "the train to land")
	dry := fs.Bool("dry-run", false, "check everything and simulate the landing locally; push nothing, merge nothing")
	keep := fs.Bool("keep-worktree", false, "keep the train's worktree after a landing")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if err := validID(*id); err != nil {
		return exitError{2, err}
	}
	g := &gitRunner{}
	root, err := trainRoot(ctx, g, *id)
	if err != nil {
		return exitError{2, err}
	}
	lotPath := filepath.Join(root, "lot.json")
	rep, err := readReport(lotPath)
	if err != nil {
		return exitError{2, err}
	}
	l := &Lander{
		Dir: filepath.Join(root, "tree"), Remote: *remote, Git: g, GH: ghCLI{Repo: *repo}, Log: stderr, DryRun: *dry,
		Save: func(r *Report) error { return writeReport(lotPath, r) },
	}
	if err := l.Land(ctx, rep); err != nil {
		return exitError{1, err}
	}
	if *dry || *keep {
		return nil
	}
	return removeWorktree(ctx, g, root)
}

func cmdClean(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	id := fs.String("id", "", "the train whose worktree to remove")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if err := validID(*id); err != nil {
		return exitError{2, err}
	}
	g := &gitRunner{}
	root, err := trainRoot(ctx, g, *id)
	if err != nil {
		return exitError{2, err}
	}
	if err := removeWorktree(ctx, g, root); err != nil {
		return exitError{2, err}
	}
	fmt.Fprintf(stderr, "ci:train: worktree of %s removed; evidence kept in %s\n", *id, root)
	return nil
}

// removeWorktree deregisters and removes <root>/tree — only that path, and
// only when git lists it as a worktree: a registered worktree costs every
// sandboxed session a deny path, so a finished train does not leave one.
func removeWorktree(ctx context.Context, g *gitRunner, root string) error {
	dir := filepath.Join(root, "tree")
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	list, err := g.run(ctx, wd, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	if !strings.Contains(list, "worktree "+dir+"\n") {
		return fmt.Errorf("%s is not a registered worktree", dir)
	}
	_, err = g.run(ctx, wd, "worktree", "remove", "--force", dir)
	return err
}
