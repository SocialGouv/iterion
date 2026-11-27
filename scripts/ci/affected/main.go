// Command affected selects the tests a diff can reach: the Go packages whose
// test binaries link a changed package, read a changed file, or own it, and
// the non-Go CI legs of .github/workflows/tests.yml whose inputs changed. It
// serves on-demand pull-request CI and the local train gate; the nightly
// keeps running everything.
//
//	affected                                  # origin/main...HEAD
//	affected -base origin/main -head HEAD     # the same, explicit
//	affected -scan                            # the static read scan, undeclared reads first
//	affected -replay-since 2026-11-19 -replay-until 2026-11-26 -weights scripts/ci/test-shards-unit-weights.tsv
//	                                          # local midnights: 11-19 00:00 to 11-26 00:00
//
// The JSON result goes to stdout, a human summary to stderr. Under-selection
// is a false green, the worst outcome here, so every doubt selects more:
//
//   - a change matching a global trigger of inputs.yaml (go.mod, vendor/,
//     .github/, scripts/ci/, the Taskfile, devbox...) selects everything;
//   - a changed file no package, declaration or leg claims selects
//     everything, and says so;
//   - the import graph is the union over the build variants CI compiles
//     (CGO on and off, the live tag, the desktop tags).
//
// Files reach tests four ways, all declared or derived:
//
//  1. imports: a production file of package P changes P; every test binary
//     that links P (`go list -deps -test`) is selected;
//  2. ownership: a test reads anything under its own directory, so every
//     package whose directory contains the file is selected;
//  3. declarations: inputs.yaml `packages:` names, per package, the repo
//     paths its tests read outside their directory. guard_test.go scans the
//     tests and fails on an undeclared read;
//  4. legs: inputs.yaml `legs:` maps each tests.yml job to its paths, the
//     Go packages it tests, and the binaries it runs.
//
// The graph is read from the working tree, so -head must be the checked-out
// commit and the tree clean (-allow-dirty accepts a dirty tree, loudly).
//
// Exit status: 0 with a selection printed; 1 when -scan finds an
// undeclared read; 2 on a usage, git or go error. `task -x ci:affected`
// builds and runs the binary so the status reaches its caller (`go run`
// folds every nonzero status into 1).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/git"
)

func main() {
	base := flag.String("base", "origin/main", "base revision; the diff is base...head (from their merge base)")
	head := flag.String("head", "HEAD", "head revision; must be the checked-out commit")
	inputsPath := flag.String("inputs", inputsFile, "declarations file, repo-relative")
	allowDirty := flag.Bool("allow-dirty", false, "accept a working tree with uncommitted changes (the graph then reflects them)")
	scan := flag.Bool("scan", false, "print the static scan of reads outside package directories, undeclared first, and exit 1 if any is undeclared")
	replaySince := flag.String("replay-since", "", "replay the first-parent commits of -replay-ref since this date (2006-01-02: local midnight; or RFC 3339) through the selector")
	replayUntil := flag.String("replay-until", "", "upper bound of -replay-since (2006-01-02: up to that day's local midnight; or RFC 3339)")
	replayRef := flag.String("replay-ref", "origin/main", "branch whose history -replay-since walks")
	weights := flag.String("weights", "", "with -replay-since: TSV import-path TAB seconds weighting the unit-test time share")
	flag.Parse()

	if err := run(context.Background(), options{
		base: *base, head: *head, inputs: *inputsPath, allowDirty: *allowDirty, scan: *scan,
		replaySince: *replaySince, replayUntil: *replayUntil, replayRef: *replayRef, weights: *weights,
	}, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "affected:", err)
		var e exitCode
		if errors.As(err, &e) {
			os.Exit(int(e))
		}
		os.Exit(2)
	}
}

// exitCode is a non-error verdict that must still exit nonzero.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

type options struct {
	base, head, inputs       string
	allowDirty, scan         bool
	replaySince, replayUntil string
	replayRef, weights       string
}

func run(ctx context.Context, o options, stdout, stderr io.Writer) error {
	root, err := gitOut(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root = strings.TrimSpace(root)
	in, err := loadInputs(filepath.Join(root, filepath.FromSlash(o.inputs)))
	if err != nil {
		return err
	}
	if o.scan {
		return runScan(root, in, stdout)
	}
	if err := checkTree(ctx, root, o.head, o.allowDirty, stderr); err != nil {
		return err
	}
	module, err := readModulePath(root)
	if err != nil {
		return err
	}
	g, err := loadGraph(ctx, root, module)
	if err != nil {
		return err
	}
	if g.nestedModules, err = nestedModules(ctx, root); err != nil {
		return err
	}
	if err := g.check(); err != nil {
		return err
	}
	if o.replaySince != "" {
		return runReplay(ctx, root, g, in, o, stdout, stderr)
	}
	changes, err := diff(ctx, root, o.base+"..."+o.head)
	if err != nil {
		return err
	}
	res := selectTests(g, in, changes)
	res.Base, res.Head = o.base, o.head
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return err
	}
	summarize(res, stderr)
	return nil
}

// checkTree refuses a head that is not the checked-out commit (the graph is
// read from the working tree) and, unless allowed, a dirty tree.
func checkTree(ctx context.Context, root, head string, allowDirty bool, stderr io.Writer) error {
	want, err := gitOut(ctx, root, "rev-parse", "--verify", head+"^{commit}")
	if err != nil {
		return err
	}
	have, err := gitOut(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(want) != strings.TrimSpace(have) {
		return fmt.Errorf("-head %s is %s but the working tree is at %s: the import graph is read from the working tree, check the head out first", head, strings.TrimSpace(want), strings.TrimSpace(have))
	}
	status, err := gitOut(ctx, root, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		if !allowDirty {
			return fmt.Errorf("the working tree has uncommitted changes, and the import graph would read them: commit or set them aside, or pass -allow-dirty\n%s", status)
		}
		fmt.Fprintf(stderr, "affected: WARNING: dirty working tree; the import graph includes uncommitted changes the diff does not:\n%s", status)
	}
	return nil
}

// diff lists the paths base...head touches, renames split into a deletion
// and an addition so both paths are judged.
func diff(ctx context.Context, root, rng string) ([]change, error) {
	out, err := gitOut(ctx, root, "diff", "--name-status", "-z", "--no-renames", rng, "--")
	if err != nil {
		return nil, err
	}
	return parseNameStatus(out)
}

func parseNameStatus(out string) ([]change, error) {
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return nil, nil
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("git diff --name-status: odd field count %d", len(fields))
	}
	var changes []change
	for i := 0; i < len(fields); i += 2 {
		st, p := fields[i], fields[i+1]
		if st == "" || p == "" {
			return nil, fmt.Errorf("git diff --name-status: empty status or path at field %d", i)
		}
		changes = append(changes, change{Path: p, Deleted: st[0] == 'D'})
	}
	return changes, nil
}

// nestedModules lists the tracked go.mod files other than the root's.
func nestedModules(ctx context.Context, root string) (map[string]bool, error) {
	out, err := gitOut(ctx, root, "ls-files", "-z", "--", "go.mod", "*/go.mod")
	if err != nil {
		return nil, err
	}
	mods := map[string]bool{}
	for _, f := range strings.Split(out, "\x00") {
		if f == "" || f == "go.mod" {
			continue
		}
		mods[path.Dir(f)] = true
	}
	return mods, nil
}

// gitOut runs git in dir with hooks off and the inherited repository
// environment scrubbed.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", git.NoRunHooks(args...)...)
	cmd.Dir = dir
	cmd.Env = git.SanitizeEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func summarize(res result, w io.Writer) {
	fmt.Fprintf(w, "affected: %s...%s: %d changed file(s)\n", res.Base, res.Head, len(res.Changed))
	if res.All {
		fmt.Fprintf(w, "affected: EVERYTHING selected (%d packages, %d legs):\n", len(res.Packages), len(res.Legs))
		for _, r := range res.Reasons {
			fmt.Fprintf(w, "  - %s\n", r)
		}
		return
	}
	fmt.Fprintf(w, "affected: %d of %d test packages selected\n", len(res.Packages), res.TestPackages)
	for _, p := range res.Packages {
		fmt.Fprintf(w, "  %s: %s\n", p.Dir, strings.Join(p.Why, "; "))
	}
	fmt.Fprintf(w, "affected: %d leg(s) selected\n", len(res.Legs))
	for _, l := range res.Legs {
		fmt.Fprintf(w, "  %s: %s\n", l.Name, strings.Join(l.Why, "; "))
	}
}
