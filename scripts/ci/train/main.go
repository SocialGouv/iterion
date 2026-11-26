// Command train lands a lot of ready pull requests on ONE local gate instead
// of one CI cycle each (#3736), and isolates the culprit when the lot is red.
//
//	train plan  N [N…]                 check the lot, print it, change nothing
//	train run   [flags] N [N…]         compose, gate, bisect; evidence + lot.json
//	train land  -id ID [-dry-run]      land a green lot, verify main's tree per merge
//	train clean -id ID                 remove a train's worktree, keep its evidence
//
// run: a fresh worktree on origin/main under <state>/<id>/tree — <state> is
// $XDG_STATE_HOME/iterion/ci-train (default ~/.local/state/iterion/ci-train),
// OUTSIDE the checkout so nothing of the operator's tree (node_modules, a
// .env) resolves from the lot's —
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
// run ejects, before composing, a pull request whose derived files are stale
// on its own head: the train re-pins what a combination leaves stale, never
// what a pull request forgot.
//
// land: refuses unless the lot is green, main is still its base, the merge
// queue is empty and every pull request is still the gated one (head, base,
// draft, the lot rule); auto-merge armed on one is disarmed. Then one
// `gh pr merge --squash --admin --match-head-commit` per pull request. Before
// each, main must still be the previous landing, or the landing stops before
// merging; after each, the pull request must read merged and its merge
// commit must be a child of the previous landing carrying the gated tree. A
// pull request whose squash would not reproduce the gated step (a derived
// file the train regenerated) first gets a merge commit carrying the train's
// resolution on its branch. Runs to cancel and comments wait for the end.
//
// Run it from the repository, through devbox (task, go, yq and pnpm come
// from there), outside any sandbox that keeps .git/worktrees read-only, and
// in a unit that outlives the shell: a full gate takes tens of minutes.
//
// Exit codes: 0 green (run) or landed (land); 1 red, empty, or a landing
// refused before touching main; 2 a precondition refused (the gate's, the
// lot's, the tool's) — no verdict; 3 a landing that stopped after main
// changed — lot.json's "landed" and "unlanded" say exactly what reached main.
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

// stateBase is where trains live: outside every checkout, because a
// worktree nested in the operator's checkout resolves Node packages (and any
// other upward lookup) from the operator's tree, not the lot's — a
// dependency the lot dropped would still resolve and the gate read green.
func stateBase() (string, error) {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "iterion", "ci-train"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no XDG_STATE_HOME and no home directory to put trains under: %w", err)
	}
	return filepath.Join(home, ".local", "state", "iterion", "ci-train"), nil
}

// trainRoot is <state>/<id>: the train's worktree, evidence and lot.json.
func trainRoot(id string) (string, error) {
	base, err := stateBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, id), nil
}

// mainCheckout is the repository's main working tree: the place a command
// about a train is run from, never the train's own tree (whose Taskfile is
// the lot's).
func mainCheckout(ctx context.Context, g *gitRunner) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	common, err := g.run(ctx, wd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Dir(strings.TrimSpace(common)), nil
}

// touchedPaths lists the files a fetched pull request head changes against
// its merge base with base — from git, not from GitHub's file list, which
// stops at 3000 files without saying so.
func touchedPaths(ctx context.Context, g *gitRunner, dir, base, head string) ([]string, error) {
	mb, err := g.run(ctx, dir, "merge-base", base, head)
	if err != nil {
		return nil, err
	}
	return g.lines(ctx, dir, "diff", "--name-only", "--no-renames", strings.TrimSpace(mb), head)
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
	allowFork := fs.Bool("allow-fork", false, "let a pull request from a fork ride the lot (its code runs as you)")
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
		if pr.Truncated {
			return exitError{2, fmt.Errorf("#%d: GitHub listed only part of its files (the list stops at 3000): plan cannot tell what it touches — `run` reads them from git", pr.Number)}
		}
	}
	for _, pr := range prs {
		var derived []string
		for _, f := range pr.Files {
			if isDerived(f) {
				derived = append(derived, f)
			}
		}
		origin := "branch " + pr.HeadRef
		if pr.CrossRepo {
			origin = "FORK"
		}
		fmt.Fprintf(stdout, "#%d %s — %s, by %s, %s, head %s, %d file(s)", pr.Number, pr.Title, pr.State, pr.Author, origin, short(pr.HeadSHA), len(pr.Files))
		if len(derived) > 0 {
			fmt.Fprintf(stdout, ", derived: %s", strings.Join(derived, ", "))
		}
		if s := judgePaths(pr); len(s) > 0 {
			fmt.Fprintf(stdout, ", judge: %s", strings.Join(s, ", "))
		}
		fmt.Fprintln(stdout)
	}
	if err := checkLot(prs, *allowFork); err != nil {
		return exitError{1, err}
	}
	fmt.Fprintf(stdout, "lot of %d: ready to run\n", len(prs))
	return nil
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	repo := fs.String("repo", "SocialGouv/iterion", "owner/name")
	remote := fs.String("remote", "origin", "the git remote of -repo")
	id := fs.String("id", "t"+time.Now().UTC().Format("20060102T150405Z"), "the train's id: its directory under $XDG_STATE_HOME/iterion/ci-train/")
	allowFork := fs.Bool("allow-fork", false, "let a pull request from a fork ride the lot (its code runs as you)")
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
	if err := checkLot(prs, *allowFork); err != nil {
		return exitError{2, err}
	}
	g := &gitRunner{}
	root, err := trainRoot(*id)
	if err != nil {
		return exitError{2, err}
	}
	checkout, err := mainCheckout(ctx, g)
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
		return fmt.Errorf("%w — its worktree stays for inspection; drop it from the main checkout: (cd %s && task ci:train -- clean -id %s)", err, checkout, *id)
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
	// What each pull request touches, from the commits themselves: GitHub's
	// file list stops at 3000 files without an error.
	for i := range prs {
		files, err := touchedPaths(ctx, g, dir, base, prs[i].HeadSHA)
		if err != nil {
			return exitError{2, withClean(err)}
		}
		prs[i].Files, prs[i].Truncated = files, false
	}
	if err := checkLot(prs, *allowFork); err != nil {
		return exitError{2, withClean(err)}
	}
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		return exitError{2, withClean(err)}
	}
	t := &Train{
		ID: *id, Dir: dir, Root: root, Git: g, Log: stderr, MaxEject: *maxEject, AllowFork: *allowFork,
		Gate:  scriptGate{Log: stderr, EmptyDir: empty},
		Regen: commandRegen{Commands: defaultGenerators, Log: stderr, EmptyDir: empty},
	}
	if *accept != "" {
		t.AcceptBaseRed = strings.Split(*accept, ",")
	}
	if *setup != "" {
		t.Setup = func(ctx context.Context, dir string) error {
			cmd := exec.CommandContext(ctx, "bash", "-c", *setup)
			cmd.Dir = dir
			cmd.Env = prCodeEnv(empty, "COREPACK_HOME="+filepath.Join(root, "corepack"))
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
	root, err := trainRoot(*id)
	if err != nil {
		return exitError{2, err}
	}
	lotPath := filepath.Join(root, "lot.json")
	rep, err := readReport(lotPath)
	if err != nil {
		return exitError{2, err}
	}
	lock, err := lockLanding(filepath.Join(root, "land.lock"))
	if err != nil {
		return exitError{1, err}
	}
	defer lock.Close()
	l := &Lander{
		Dir: filepath.Join(root, "tree"), Remote: *remote, Git: g, GH: ghCLI{Repo: *repo}, Log: stderr, DryRun: *dry,
		Save: func(r *Report) error { return writeReport(lotPath, r) },
	}
	if err := l.Land(ctx, rep); err != nil {
		if errors.Is(err, errPartial) {
			return exitError{3, err}
		}
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
	root, err := trainRoot(*id)
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
