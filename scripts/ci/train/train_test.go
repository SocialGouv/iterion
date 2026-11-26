package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// isolateGit points every git the test runs at a config of its own: an
// identity, no signing, no system or user config — and a HOME it owns.
func isolateGit(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gitconfig")
	body := "[user]\n\tname = train-test\n\temail = train-test@example.com\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", dir)
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := (&gitRunner{}).run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if c == "<delete>" {
			if err := os.Remove(full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const golden = "internal/mdcode/testdata/committed-reading.txt"

// goldenRegen stands in for cmd/corpus-reading: one line per tracked .md,
// its path and the hash of its bytes, sorted — the real golden's shape, and
// the property that makes two pull requests adding neighbouring pages
// conflict on adjacent lines.
type goldenRegen struct {
	calls int
}

func (r *goldenRegen) Regenerate(ctx context.Context, dir string) error {
	r.calls++
	// A generator broken by the tree it runs on: break-gen.txt alone, or
	// gen-a.txt and gen-b.txt together (two pull requests fine apart).
	exists := func(p string) bool { _, err := os.Stat(filepath.Join(dir, p)); return err == nil }
	if exists("break-gen.txt") || (exists("gen-a.txt") && exists("gen-b.txt")) {
		return fmt.Errorf("go run ./cmd/iterion map gen: exit status 1: cmd/iterion: undefined: Foo")
	}
	files, err := (&gitRunner{}).lines(ctx, dir, "ls-files", "--cached", "--", "*.md")
	if err != nil {
		return err
	}
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		c, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		fmt.Fprintf(&b, "%s %x\n", f, sha256.Sum256(c))
	}
	full := filepath.Join(dir, golden)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(b.String()), 0o644)
}

// fixture is an origin repository with main and one branch per pull
// request, and a train whose worktree is a clone of it.
type fixture struct {
	t      *testing.T
	origin string
	seed   string
	root   string
	dir    string
	base   string
	regen  *goldenRegen
}

func newFixture(t *testing.T, baseFiles map[string]string) *fixture {
	t.Helper()
	isolateGit(t)
	tmp := t.TempDir()
	f := &fixture{t: t, origin: filepath.Join(tmp, "origin.git"), seed: filepath.Join(tmp, "seed"), root: filepath.Join(tmp, "train"), regen: &goldenRegen{}}
	mustGit(t, tmp, "init", "--quiet", "--bare", f.origin)
	mustGit(t, tmp, "clone", "--quiet", f.origin, f.seed)
	writeFiles(t, f.seed, baseFiles)
	mustGit(t, f.seed, "add", "-A")
	if err := f.regen.Regenerate(context.Background(), f.seed); err != nil {
		t.Fatal(err)
	}
	mustGit(t, f.seed, "add", "-A")
	mustGit(t, f.seed, "commit", "--quiet", "-m", "base")
	mustGit(t, f.seed, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	f.base = mustGit(t, f.seed, "rev-parse", "HEAD")
	return f
}

// pr commits files on a branch off the base, re-pins the golden as a real
// pull request does, pushes it and returns the PR.
func (f *fixture) pr(n int, files map[string]string) PR {
	return f.prRepin(n, files, true)
}

// prRepin is pr, optionally WITHOUT re-pinning the golden: a pull request
// red on its own CI run.
func (f *fixture) prRepin(n int, files map[string]string, repin bool) PR {
	f.t.Helper()
	branch := fmt.Sprintf("pr-%d", n)
	mustGit(f.t, f.seed, "checkout", "--quiet", "-B", branch, f.base)
	writeFiles(f.t, f.seed, files)
	mustGit(f.t, f.seed, "add", "-A")
	if repin {
		if err := f.regen.Regenerate(context.Background(), f.seed); err != nil && !strings.Contains(err.Error(), "undefined: Foo") {
			f.t.Fatal(err)
		}
	}
	mustGit(f.t, f.seed, "add", "-A")
	mustGit(f.t, f.seed, "commit", "--quiet", "-m", fmt.Sprintf("change %d\n\nCo-Authored-By: Someone <s@example.com>", n))
	mustGit(f.t, f.seed, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/"+branch)
	sha := mustGit(f.t, f.seed, "rev-parse", "HEAD")
	var names []string
	for p := range files {
		names = append(names, p)
	}
	return PR{Number: n, Title: fmt.Sprintf("change %d", n), State: "open", BaseRef: "main", HeadRef: branch, HeadSHA: sha, Files: names}
}

func (f *fixture) train(gate Gate) *Train {
	f.t.Helper()
	if err := os.MkdirAll(f.root, 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.dir = filepath.Join(f.root, "tree")
	if _, err := os.Stat(f.dir); os.IsNotExist(err) {
		mustGit(f.t, f.root, "clone", "--quiet", f.origin, f.dir)
	}
	return &Train{ID: "t-test", Dir: f.dir, Root: f.root, Git: &gitRunner{}, Gate: gate, Regen: f.regen, Log: io.Discard, MaxEject: 3}
}

// markerGate is red on member m when the checkout holds bad-<m>.txt. It can
// be told to flake: report a member red on the first full gate only.
type markerGate struct {
	mu         sync.Mutex
	flakeOnce  []string
	fulls      int
	probes     []string // "dir-commit:member" per probe
	refuseCode int
	// refused and incomplete ride along an exit 1, as gate.sh reports them
	// when a red member takes precedence.
	refused    []string
	incomplete bool
	// dirty makes Full leave the tree rewritten, as vendor-check does, and
	// Probe refuse a dirty tree, as vendor-check refuses its own dirt.
	dirty   bool
	fullErr error
}

func (g *markerGate) redIn(dir string, members []string) []string {
	var red []string
	for _, m := range members {
		if _, err := os.Stat(filepath.Join(dir, "bad-"+m+".txt")); err == nil {
			red = append(red, m)
		}
	}
	return red
}

func (g *markerGate) allMembers(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "bad-*.txt"))
	var ms []string
	for _, p := range matches {
		ms = append(ms, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "bad-"), ".txt"))
	}
	return ms
}

func (g *markerGate) Full(ctx context.Context, dir, ev string) (GateResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fulls++
	if g.fullErr != nil {
		return GateResult{}, g.fullErr
	}
	if g.dirty {
		if err := os.WriteFile(filepath.Join(dir, "vendor-dirt.txt"), []byte("x"), 0o644); err != nil {
			return GateResult{}, err
		}
	}
	if g.refuseCode != 0 {
		return GateResult{Code: g.refuseCode, Refused: []string{"ci:mongo-conformance"}}, nil
	}
	red := g.redIn(dir, g.allMembers(dir))
	if g.fulls == 1 {
		red = append(red, g.flakeOnce...)
	}
	if len(red) == 0 {
		return GateResult{Code: 0}, nil
	}
	return GateResult{Code: 1, Red: red, Refused: g.refused, Incomplete: g.incomplete}, nil
}

func (g *markerGate) Probe(ctx context.Context, dir, ev string, members []string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, "vendor-dirt.txt")); err == nil {
		return nil, fmt.Errorf("member ci:vendor-check refused to run (exit 2): the tree is dirty")
	}
	head, _ := (&gitRunner{}).run(ctx, dir, "rev-parse", "--short", "HEAD")
	for _, m := range members {
		g.probes = append(g.probes, strings.TrimSpace(head)+":"+m)
	}
	return g.redIn(dir, members), nil
}

func stepNumbers(steps []Step) []int {
	var ns []int
	for _, s := range steps {
		ns = append(ns, s.PR.Number)
	}
	return ns
}

func ejectedNumbers(ej []Ejection) []int {
	var ns []int
	for _, e := range ej {
		ns = append(ns, e.PR)
	}
	return ns
}

func TestBisectFindsTheFirstRedPrefixInLogProbes(t *testing.T) {
	for n := 1; n <= 9; n++ {
		for culprit := 1; culprit <= n; culprit++ {
			var probed []int
			k, _, err := bisect(context.Background(), n, []string{"test"}, func(_ context.Context, k int, _ []string) ([]string, error) {
				probed = append(probed, k)
				if k >= culprit {
					return []string{"test"}, nil
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if k != culprit {
				t.Errorf("n=%d culprit=%d: bisect said %d (probed %v)", n, culprit, k, probed)
			}
			if limit := bits.Len(uint(n - 1)); len(probed) > limit {
				t.Errorf("n=%d culprit=%d: %d probes %v, want ≤ ⌈log₂ n⌉ = %d", n, culprit, len(probed), probed, limit)
			}
			for _, p := range probed {
				if p <= 0 || p >= n {
					t.Errorf("n=%d: probed prefix %d, an endpoint the caller already proved", n, p)
				}
			}
		}
	}
}

func TestBisectRefusesAnEmptyLotOrNoMember(t *testing.T) {
	never := func(context.Context, int, []string) ([]string, error) { return nil, nil }
	if _, _, err := bisect(context.Background(), 0, []string{"test"}, never); err == nil {
		t.Fatal("a lot of 0 has no culprit, want an error")
	}
	if _, _, err := bisect(context.Background(), 3, nil, never); err == nil {
		t.Fatal("no member to probe, want an error")
	}
}

func TestCheckLot(t *testing.T) {
	ok := PR{Number: 1, State: "open", BaseRef: "main", Files: []string{"pkg/x/x.go"}}
	ci := PR{Number: 2, State: "open", BaseRef: "main", Files: []string{".github/workflows/tests.yml"}}
	fork := PR{Number: 9, State: "open", BaseRef: "main", CrossRepo: true, Author: "someone", Files: []string{"pkg/x/x.go"}}
	judge := func(n int, f string) PR {
		return PR{Number: n, State: "open", BaseRef: "main", Files: []string{f}}
	}
	for _, tc := range []struct {
		name      string
		prs       []PR
		allowFork bool
		want      string
	}{
		{"a clean lot", []PR{ok, {Number: 3, State: "open", BaseRef: "main"}}, false, ""},
		{"a CI pull request, even alone", []PR{ci}, false, "#2 touches the train's judge (.github/workflows/tests.yml)"},
		{"a CI pull request in a lot", []PR{ok, ci}, false, "#2 touches the train's judge"},
		{"the Taskfile", []PR{judge(4, "Taskfile.yml")}, false, "#4 touches the train's judge (Taskfile.yml)"},
		{"devbox.json", []PR{judge(5, "devbox.json")}, false, "#5 touches the train's judge (devbox.json)"},
		{"devbox.lock", []PR{judge(6, "devbox.lock")}, false, "#6 touches the train's judge (devbox.lock)"},
		{"the gate script", []PR{judge(10, "scripts/ci/gate.sh")}, false, "#10 touches the train's judge (scripts/ci/gate.sh)"},
		{"a quarantine row rides a lot", []PR{ok, {Number: 7, State: "open", BaseRef: "main", Files: []string{"scripts/ci/test-quarantine.tsv"}}}, false, ""},
		{"a quarantine row with a CI script does not", []PR{ok, {Number: 8, State: "open", BaseRef: "main", Files: []string{"scripts/ci/test-quarantine.tsv", "scripts/ci/gate.sh"}}}, false, "#8 touches the train's judge (scripts/ci/gate.sh)"},
		{"a fork", []PR{ok, fork}, false, "#9 comes from a fork (by someone)"},
		{"a fork the operator allows", []PR{ok, fork}, true, ""},
		{"a draft", []PR{{Number: 4, State: "open", BaseRef: "main", Draft: true}}, false, "#4 is a draft"},
		{"closed", []PR{{Number: 5, State: "closed", BaseRef: "main"}}, false, "#5 is closed"},
		{"another base", []PR{{Number: 6, State: "open", BaseRef: "release/v7"}}, false, "#6 targets"},
		{"listed twice", []PR{ok, ok}, false, "#1 is listed twice"},
		{"empty", nil, false, "no pull request"},
	} {
		err := checkLot(tc.prs, tc.allowFork)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v, want no error", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestIsDerived(t *testing.T) {
	for p, want := range map[string]bool{
		golden:                            true,
		"docs/references/map-packages.md": true,
		"docs/references/map-docs.md":     true,
		"docs/references/maps/x.md":       false,
		"docs/references/map-x/y.md":      false,
		"docs/references/dsl-grammar.md":  false,
		"internal/mdcode/testdata/other":  false,
	} {
		if got := isDerived(p); got != want {
			t.Errorf("isDerived(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestComposeEjectsARealConflictAndKeepsTheRest(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "one\ntwo\n"})
	p1 := f.pr(1, map[string]string{"a.txt": "ONE\ntwo\n"})
	p2 := f.pr(2, map[string]string{"a.txt": "uno\ntwo\n"})
	p3 := f.pr(3, map[string]string{"b.txt": "b\n"})
	tr := f.train(&markerGate{})
	steps, ej, err := tr.compose(context.Background(), f.base, []PR{p1, p2, p3})
	if err != nil {
		t.Fatal(err)
	}
	if got := stepNumbers(steps); !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("steps %v, want [1 3]", got)
	}
	if len(ej) != 1 || ej[0].PR != 2 || !strings.Contains(ej[0].Reason, "a.txt") {
		t.Fatalf("ejected %+v, want #2 on a.txt", ej)
	}
	if st := mustGit(t, f.dir, "status", "--porcelain"); st != "" {
		t.Fatalf("an ejection left the worktree dirty:\n%s", st)
	}
	if got := mustGit(t, f.dir, "show", steps[1].Commit+":a.txt"); got != "ONE\ntwo" {
		t.Fatalf("the lot's a.txt is %q, want #1's", got)
	}
}

func TestComposeRegeneratesAGoldenConflictInsteadOfPickingASide(t *testing.T) {
	f := newFixture(t, map[string]string{"docs/a.md": "a\n", "docs/z.md": "z\n"})
	// Neighbouring pages: their golden lines are adjacent, so the squash of
	// the second conflicts on the golden — and on nothing else.
	p1 := f.pr(1, map[string]string{"docs/b.md": "b\n"})
	p2 := f.pr(2, map[string]string{"docs/c.md": "c\n"})
	tr := f.train(&markerGate{})
	steps, ej, err := tr.compose(context.Background(), f.base, []PR{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	if len(ej) != 0 || !slices.Equal(stepNumbers(steps), []int{1, 2}) {
		t.Fatalf("steps %v ejected %+v, want both squashed", stepNumbers(steps), ej)
	}
	if !slices.Contains(steps[1].Regenerated, golden) {
		t.Fatalf("#2's step regenerated %v, want the golden", steps[1].Regenerated)
	}
	got := mustGit(t, f.dir, "show", steps[1].Commit+":"+golden)
	for _, page := range []string{"docs/a.md", "docs/b.md", "docs/c.md", "docs/z.md"} {
		if !strings.Contains(got, page+" ") {
			t.Errorf("the regenerated golden misses %s:\n%s", page, got)
		}
	}
	if strings.Contains(got, "<<<<<<<") {
		t.Fatalf("conflict markers in the golden:\n%s", got)
	}
}

func TestComposeEjectsAPullRequestAlreadyOnMain(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	p1 := f.pr(1, map[string]string{"b.txt": "b\n"})
	p2 := f.pr(2, map[string]string{"b.txt": "b\n"}) // the same change again
	tr := f.train(&markerGate{})
	steps, ej, err := tr.compose(context.Background(), f.base, []PR{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stepNumbers(steps), []int{1}) || len(ej) != 1 || !strings.Contains(ej[0].Reason, "adds nothing") {
		t.Fatalf("steps %v ejected %+v, want #2 ejected as empty", stepNumbers(steps), ej)
	}
}

func TestRunLandsAGreenLotInOneGate(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
	g := &markerGate{}
	rep, err := f.train(g).Run(context.Background(), f.base, prs)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != verdictGreen || g.fulls != 1 || len(g.probes) != 0 || len(rep.Steps) != 2 {
		t.Fatalf("verdict %s, %d full gate(s), probes %v, steps %v — want green on one gate", rep.Verdict, g.fulls, g.probes, stepNumbers(rep.Steps))
	}
}

// One culprit, at every position of a lot of four: bisection names it, the
// rest is regated in full and stays.
func TestRunEjectsTheCulpritWhereverItSits(t *testing.T) {
	for pos := 1; pos <= 4; pos++ {
		t.Run(fmt.Sprintf("culprit-%d", pos), func(t *testing.T) {
			f := newFixture(t, map[string]string{"a.txt": "a\n"})
			var prs []PR
			for n := 1; n <= 4; n++ {
				files := map[string]string{fmt.Sprintf("f%d.txt", n): "x\n"}
				if n == pos {
					files["bad-test.txt"] = "red\n"
				}
				prs = append(prs, f.pr(n, files))
			}
			g := &markerGate{}
			rep, err := f.train(g).Run(context.Background(), f.base, prs)
			if err != nil {
				t.Fatal(err)
			}
			var want []int
			for n := 1; n <= 4; n++ {
				if n != pos {
					want = append(want, n)
				}
			}
			if rep.Verdict != verdictGreen || !slices.Equal(stepNumbers(rep.Steps), want) || !slices.Equal(ejectedNumbers(rep.Ejected), []int{pos}) {
				t.Fatalf("verdict %s, steps %v, ejected %v — want green %v without #%d", rep.Verdict, stepNumbers(rep.Steps), ejectedNumbers(rep.Ejected), want, pos)
			}
			if g.fulls != 2 {
				t.Fatalf("%d full gates, want 2: the red lot, then the regate of what is left", g.fulls)
			}
			// The landed lot must not carry the culprit's file at any step.
			for _, s := range rep.Steps {
				if _, code, _ := (&gitRunner{}).runCode(context.Background(), f.dir, "cat-file", "-e", s.Commit+":bad-test.txt"); code == 0 {
					t.Fatalf("step #%d carries the culprit's file", s.PR.Number)
				}
			}
			if !strings.Contains(rep.Ejected[0].Reason, "test") {
				t.Fatalf("ejection reason %q does not name the red member", rep.Ejected[0].Reason)
			}
		})
	}
}

func TestRunEjectsTwoCulpritsOnTwoMembers(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{
		f.pr(1, map[string]string{"f1.txt": "x\n"}),
		f.pr(2, map[string]string{"bad-race.txt": "x\n"}),
		f.pr(3, map[string]string{"f3.txt": "x\n"}),
		f.pr(4, map[string]string{"bad-lint.txt": "x\n"}),
		f.pr(5, map[string]string{"f5.txt": "x\n"}),
	}
	rep, err := f.train(&markerGate{}).Run(context.Background(), f.base, prs)
	if err != nil {
		t.Fatal(err)
	}
	ej := ejectedNumbers(rep.Ejected)
	slices.Sort(ej)
	if rep.Verdict != verdictGreen || !slices.Equal(ej, []int{2, 4}) || !slices.Equal(stepNumbers(rep.Steps), []int{1, 3, 5}) {
		t.Fatalf("verdict %s, steps %v, ejected %v — want green [1 3 5] without #2 and #4", rep.Verdict, stepNumbers(rep.Steps), rep.Ejected)
	}
}

func TestRunRecordsAFlakeWithoutEjectingAnyone(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
	g := &markerGate{flakeOnce: []string{"race"}}
	rep, err := f.train(g).Run(context.Background(), f.base, prs)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != verdictGreen || len(rep.Ejected) != 0 || !slices.Equal(rep.Flakes, []string{"race"}) {
		t.Fatalf("verdict %s, ejected %v, flakes %v — want green, nobody ejected, race recorded", rep.Verdict, rep.Ejected, rep.Flakes)
	}
}

func TestRunHoldsAMemberRedOnMainAgainstMainNotTheLot(t *testing.T) {
	base := map[string]string{"a.txt": "a\n", "bad-docs.txt": "main is red\n"}
	t.Run("refused without acceptance", func(t *testing.T) {
		f := newFixture(t, base)
		prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"c.txt": "c\n"})}
		rep, err := f.train(&markerGate{}).Run(context.Background(), f.base, prs)
		if err == nil || !strings.Contains(err.Error(), "red on main too") {
			t.Fatalf("err %v, want main's red named", err)
		}
		if rep.Verdict != verdictRed || len(rep.Ejected) != 0 {
			t.Fatalf("verdict %s, ejected %v — want red, nobody ejected for main's red", rep.Verdict, rep.Ejected)
		}
	})
	t.Run("accepted", func(t *testing.T) {
		f := newFixture(t, base)
		prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"bad-test.txt": "x\n"})}
		tr := f.train(&markerGate{})
		tr.AcceptBaseRed = []string{"docs"}
		rep, err := tr.Run(context.Background(), f.base, prs)
		if err != nil {
			t.Fatal(err)
		}
		// The lot's own red (#2 on test) is still found and ejected; main's
		// red on docs is accepted, recorded, and held against nobody.
		if rep.Verdict != verdictGreen || !slices.Equal(ejectedNumbers(rep.Ejected), []int{2}) || !slices.Equal(rep.BaseRed, []string{"docs"}) {
			t.Fatalf("verdict %s, ejected %v, base red %v — want green, #2 ejected, docs accepted", rep.Verdict, rep.Ejected, rep.BaseRed)
		}
	})
}

func TestRunStopsOnARefusalWithoutAVerdict(t *testing.T) {
	for _, code := range []int{2, 3} {
		f := newFixture(t, map[string]string{"a.txt": "a\n"})
		prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
		g := &markerGate{refuseCode: code}
		rep, err := f.train(g).Run(context.Background(), f.base, prs)
		if err == nil {
			t.Fatalf("gate exit %d: no error, want the run to stop", code)
		}
		want := map[int]string{2: verdictRefused, 3: verdictIncomplete}[code]
		if rep.Verdict != want || len(g.probes) != 0 || len(rep.Ejected) != 0 {
			t.Fatalf("gate exit %d: verdict %q, probes %v, ejected %v — want %q and no bisection", code, rep.Verdict, g.probes, rep.Ejected, want)
		}
	}
}

func TestRunRefusesAMainWhoseDerivedFilesAreStale(t *testing.T) {
	f := newFixture(t, map[string]string{"docs/a.md": "a\n"})
	// main changes a page without re-pinning the golden.
	mustGit(t, f.seed, "checkout", "--quiet", "main")
	writeFiles(t, f.seed, map[string]string{"docs/a.md": "changed\n"})
	mustGit(t, f.seed, "commit", "--quiet", "-am", "stale golden")
	mustGit(t, f.seed, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	stale := mustGit(t, f.seed, "rev-parse", "HEAD")
	pr := f.pr(1, map[string]string{"b.txt": "b\n"})
	tr := f.train(&markerGate{})
	_, err := tr.Run(context.Background(), stale, []PR{pr})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("err %v, want main's stale derived files named", err)
	}
}

// Forgiving every red member gives back what the gate would have said
// without them — gate.sh's exit 1 outranks a refusal (2) and an incomplete
// gate (3), so neither may come back green.
func TestRunForgivenRedIsNotGreenWhenTheGateWasRefusedOrIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		gate   *markerGate
		base   map[string]string
		accept []string
		want   string
	}{
		{"flake over a refusal", &markerGate{flakeOnce: []string{"race"}, refused: []string{"ci:mongo-conformance"}}, nil, nil, verdictRefused},
		{"flake over an incomplete gate", &markerGate{flakeOnce: []string{"race"}, incomplete: true}, nil, nil, verdictIncomplete},
		{"accepted base red over a refusal", &markerGate{refused: []string{"ci:mongo-conformance"}}, map[string]string{"bad-docs.txt": "x\n"}, []string{"docs"}, verdictRefused},
		{"accepted base red over an incomplete gate", &markerGate{incomplete: true}, map[string]string{"bad-docs.txt": "x\n"}, []string{"docs"}, verdictIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := map[string]string{"a.txt": "a\n"}
			for k, v := range tc.base {
				base[k] = v
			}
			f := newFixture(t, base)
			prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
			tr := f.train(tc.gate)
			tr.AcceptBaseRed = tc.accept
			rep, err := tr.Run(context.Background(), f.base, prs)
			if err == nil || rep.Verdict != tc.want {
				t.Fatalf("verdict %q, err %v — want %q and an error", rep.Verdict, err, tc.want)
			}
			r := rep.Rounds[len(rep.Rounds)-1]
			if !slices.Equal(r.Refused, tc.gate.refused) || r.Incomplete != tc.gate.incomplete {
				t.Fatalf("round %+v: the record lost what the gate reported beside its red", r)
			}
		})
	}
}

func TestRunEjectsAPullRequestWhoseDerivedFilesAreStaleOnItsOwnHead(t *testing.T) {
	f := newFixture(t, map[string]string{"docs/a.md": "a\n"})
	p1 := f.pr(1, map[string]string{"docs/b.md": "b\n"})
	p2 := f.prRepin(2, map[string]string{"docs/c.md": "c\n"}, false) // forgot to re-pin
	rep, err := f.train(&markerGate{}).Run(context.Background(), f.base, []PR{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stepNumbers(rep.Steps), []int{1}) || len(rep.Ejected) != 1 || rep.Ejected[0].PR != 2 || !strings.Contains(rep.Ejected[0].Reason, "stale on its own head") {
		t.Fatalf("steps %v, ejected %+v — want #2 ejected for its own stale golden, not re-pinned by the train", stepNumbers(rep.Steps), rep.Ejected)
	}
}

func TestRunRerunsOnACleanCheckoutOfTheHead(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"}), f.pr(2, map[string]string{"bad-test.txt": "x\n"})}
	rep, err := f.train(&markerGate{dirty: true}).Run(context.Background(), f.base, prs)
	if err != nil {
		t.Fatalf("the rerun ran on the tree the full gate left dirty: %v", err)
	}
	if rep.Verdict != verdictGreen || !slices.Equal(ejectedNumbers(rep.Ejected), []int{2}) {
		t.Fatalf("verdict %s, ejected %v — want #2 bisected out", rep.Verdict, rep.Ejected)
	}
}

func TestRunEjectsAPullRequestThatBreaksTheGeneratorsInCombination(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"gen-a.txt": "x\n"}), f.pr(2, map[string]string{"gen-b.txt": "x\n"}), f.pr(3, map[string]string{"c.txt": "x\n"})}
	rep, err := f.train(&markerGate{}).Run(context.Background(), f.base, prs)
	if err != nil {
		t.Fatalf("a generator broken by the combination aborted the train: %v", err)
	}
	if !slices.Equal(stepNumbers(rep.Steps), []int{1, 3}) || !slices.Equal(ejectedNumbers(rep.Ejected), []int{2}) || !strings.Contains(rep.Ejected[0].Reason, "generators fail") {
		t.Fatalf("steps %v, ejected %+v — want #2 ejected, #1 and #3 kept", stepNumbers(rep.Steps), rep.Ejected)
	}
}

func TestComposeResolvesADerivedFileTheLotDeleted(t *testing.T) {
	const m = "docs/references/map-x.md"
	f := newFixture(t, map[string]string{m: "one\n"})
	p1 := f.pr(1, map[string]string{m: "<delete>"})
	p2 := f.pr(2, map[string]string{m: "two\n", "src.txt": "the extractor's input\n"})
	tr := f.train(&markerGate{})
	steps, ej, err := tr.compose(context.Background(), f.base, []PR{p1, p2})
	if err != nil {
		t.Fatalf("a modify/delete conflict on a derived file aborted the train: %v", err)
	}
	if len(ej) != 0 || !slices.Equal(stepNumbers(steps), []int{1, 2}) {
		t.Fatalf("steps %v ejected %+v, want both squashed", stepNumbers(steps), ej)
	}
	if _, code, _ := (&gitRunner{}).runCode(context.Background(), f.dir, "cat-file", "-e", steps[1].Commit+":"+m); code == 0 {
		t.Fatal("the derived file the lot deleted came back: a side was picked")
	}
}

func TestRunReturnsItsReportWhenItStops(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{f.pr(1, map[string]string{"b.txt": "b\n"})}
	rep, err := f.train(&markerGate{fullErr: fmt.Errorf("gate.sh: signal: killed")}).Run(context.Background(), f.base, prs)
	if err == nil || rep == nil || len(rep.Steps) != 1 || rep.Verdict != "" {
		t.Fatalf("rep %+v, err %v — want the composed report back, no verdict, with the error", rep, err)
	}
}

func TestAssertOwnTreeRefusesAnythingButItsWorktree(t *testing.T) {
	isolateGit(t)
	tmp := t.TempDir()
	// The root is a repository of its own here: the case that matters is a
	// root misconfigured onto a real checkout, where only the "strictly
	// under" guard stands between the train and a reset --hard of it.
	root := filepath.Join(tmp, "train")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, root, "init", "--quiet")
	dir := filepath.Join(root, "tree")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "init", "--quiet")
	outside := filepath.Join(tmp, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, outside, "init", "--quiet")
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Each case names the guard that must refuse it, so a guard deleted
	// cannot hide behind another one.
	for _, tc := range []struct{ name, dir, want string }{
		{"the root itself, a repository", root, "not strictly under"},
		{"a repository outside the root", outside, "not strictly under"},
		{"a symlink under the root leaving it", link, "not strictly under"},
		{"a relative path", "train/tree", "not absolute"},
		{"a subdirectory of the tree", sub, "git top level"},
	} {
		tr := &Train{Dir: tc.dir, Root: root, Git: &gitRunner{}}
		err := tr.assertOwnTree(context.Background())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s (%s): %v, want a refusal containing %q", tc.name, tc.dir, err, tc.want)
		}
	}
	tr := &Train{Dir: dir, Root: root, Git: &gitRunner{}}
	if err := tr.assertOwnTree(context.Background()); err != nil {
		t.Fatalf("its own worktree refused: %v", err)
	}
}

func TestRunStopsAboveMaxEject(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	prs := []PR{
		f.pr(1, map[string]string{"bad-race.txt": "x\n"}),
		f.pr(2, map[string]string{"bad-lint.txt": "x\n"}),
		f.pr(3, map[string]string{"f3.txt": "x\n"}),
	}
	tr := f.train(&markerGate{})
	tr.MaxEject = 1
	rep, err := tr.Run(context.Background(), f.base, prs)
	if err == nil || !strings.Contains(err.Error(), "systemic") || rep.Verdict != verdictRed {
		t.Fatalf("err %v, verdict %s — want the train to stop red above -max-eject 1", err, rep.Verdict)
	}
}

func TestReportRoundTrips(t *testing.T) {
	rep := &Report{ID: "t1", Base: "abc", Steps: []Step{{PR: PR{Number: 3, Files: []string{"x"}}, Commit: "c", Tree: "t", Regenerated: []string{golden}}}, Verdict: verdictGreen, Flakes: []string{"race"}}
	path := filepath.Join(t.TempDir(), "lot.json")
	if err := writeReport(path, rep); err != nil {
		t.Fatal(err)
	}
	got, err := readReport(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(path)
	if err := writeReport(path, got); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !bytes.Equal(a, b) {
		t.Fatalf("the report does not round-trip:\n%s\n---\n%s", a, b)
	}
}
