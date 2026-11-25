package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

const committedList = "../test-quarantine.tsv"

// The rows a CI job could misread are refused at parse, each on its own
// reason, so neither the gating -skip nor the nightly -run is ever built from
// one of them.
func TestParseRefusesARowACIJobCouldMisread(t *testing.T) {
	const pkg = modulePath + "/pkg/x"
	for _, tc := range []struct{ name, row, want string }{
		{"no ticket", pkg + "\tTestA\t", "carries no ticket"},
		{"ticket without hash", pkg + "\tTestA\t3512", "carries no ticket"},
		{"ticket zero", pkg + "\tTestA\t#0", "carries no ticket"},
		{"two fields", pkg + "\tTestA", "got 2 field(s)"},
		{"four fields", pkg + "\tTestA\t#1\textra", "got 4 field(s)"},
		{"foreign module", "example.com/x\tTestA\t#1", "outside module"},
		{"alternation", pkg + "\tTestA|TestB\t#1", "outside [A-Za-z0-9_-.*+?=:,]"},
		{"group", pkg + "\tTest(A)\t#1", "outside [A-Za-z0-9_-.*+?=:,]"},
		{"empty level", pkg + "\tTestA//sub\t#1", "level 2"},
		{"bad regexp", pkg + "\tTestA**\t#1", "not a regexp"},
		{"not a test", pkg + "\tExampleA\t#1", "does not name a Test function"},
		{"duplicate", pkg + "\tTestA\t#1\n" + pkg + "\tTestA\t#2", "already quarantined at line 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(strings.NewReader(tc.row + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse(%q) = %v, want an error containing %q", tc.row, err, tc.want)
			}
		})
	}
	entries, err := parse(strings.NewReader("# comment\n\n" + pkg + "\tTestA/sub-case\t#12\n"))
	if err != nil || len(entries) != 1 || entries[0].Ticket != "12" || !slices.Equal(entries[0].Levels, []string{"TestA", "sub-case"}) {
		t.Fatalf("a well-formed row: got %+v, %v", entries, err)
	}
}

// The pattern is proved against Go's own matcher, not against a reading of
// it: a scratch module holds a quarantined top-level test, a quarantined
// subtest beside a sibling that must keep running, and a test whose name
// extends a quarantined one. -skip must drop exactly the quarantined ones and
// -run must select exactly them.
func TestThePatternSkipsAndSelectsExactlyTheRows(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go is required to prove the pattern against the real matcher: %v", err)
	}
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/q\n\ngo 1.22\n")
	write("q_test.go", `package q

import "testing"

func TestFlaky(t *testing.T)       {}
func TestFlakyButNot(t *testing.T) {}
func TestParent(t *testing.T) {
	t.Run("bad-case", func(t *testing.T) {})
	t.Run("good-case", func(t *testing.T) {})
}
func TestSeed_One(t *testing.T) {}
func TestSeed_Two(t *testing.T) {}
func TestHealthy(t *testing.T)  {}
`)
	entries, err := parse(strings.NewReader(strings.Join([]string{
		modulePath + "/q\tTestFlaky\t#1",
		modulePath + "/q\tTestParent/bad-case\t#2",
		modulePath + "/q\tTestSeed_.*\t#3",
	}, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	ran := func(flag string) []string {
		t.Helper()
		cmd := exec.Command(goBin, "test", "-count=1", "-v", flag, pattern(entries), ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go test %s: %v\n%s", flag, err, out)
		}
		var names []string
		for _, line := range strings.Split(string(out), "\n") {
			if m := resultLine.FindStringSubmatch(line); m != nil && m[1] == "PASS" {
				names = append(names, m[2])
			}
		}
		sort.Strings(names)
		return names
	}
	if got, want := ran("-skip"), []string{"TestFlakyButNot", "TestHealthy", "TestParent", "TestParent/good-case"}; !slices.Equal(got, want) {
		t.Errorf("-skip ran %v, want %v", got, want)
	}
	if got, want := ran("-run"), []string{"TestFlaky", "TestParent", "TestParent/bad-case", "TestSeed_One", "TestSeed_Two"}; !slices.Equal(got, want) {
		t.Errorf("-run ran %v, want %v", got, want)
	}
}

// An empty quarantine skips nothing and refuses -run: an empty -run value
// would select every test of the nightly packages.
func TestAnEmptyListSkipsNothingAndRefusesRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.tsv")
	if err := os.WriteFile(path, []byte("# nothing quarantined\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(path, true, false, false, false, "", &out); err != nil || out.String() != "\n" {
		t.Fatalf("-skip on an empty list: %q, %v", out.String(), err)
	}
	out.Reset()
	err := run(path, false, true, false, false, "", &out)
	var e *exitError
	if !errors.As(err, &e) || e.code != 1 || out.Len() != 0 {
		t.Fatalf("-run on an empty list: want exit 1 and no output, got %v, %q", err, out.String())
	}
}

// The nightly job's proof that each row really ran: a row with no PASS at its
// own depth is refused, whether it was never selected, skipped or failed —
// and a PASS of its parent does not stand in for a quarantined subtest.
func TestVerifyRefusesARowWithoutItsOwnPass(t *testing.T) {
	entries, err := parse(strings.NewReader(modulePath + "/q\tTestFlaky\t#1\n" + modulePath + "/q\tTestParent/bad-case\t#2\n"))
	if err != nil {
		t.Fatal(err)
	}
	const both = "--- PASS: TestFlaky (0.00s)\n--- PASS: TestParent (0.00s)\n    --- PASS: TestParent/bad-case (0.00s)\n"
	if err := verifyPassed(entries, both); err != nil {
		t.Fatalf("every row passed: %v", err)
	}
	for name, log := range map[string]string{
		"parent only":   "--- PASS: TestFlaky (0.00s)\n--- PASS: TestParent (0.00s)\n",
		"skipped":       "--- SKIP: TestFlaky (0.00s)\n--- PASS: TestParent (0.00s)\n    --- PASS: TestParent/bad-case (0.00s)\n",
		"failed":        "--- FAIL: TestFlaky (0.00s)\n--- PASS: TestParent (0.00s)\n    --- PASS: TestParent/bad-case (0.00s)\n",
		"never matched": "--- PASS: TestFlakyButNot (0.00s)\n--- PASS: TestParent (0.00s)\n    --- PASS: TestParent/bad-case (0.00s)\n",
	} {
		if err := verifyPassed(entries, log); err == nil {
			t.Errorf("%s: verify accepted a log in which a row did not pass", name)
		}
	}
}

// testFunc is a top-level Test function declaration in a _test.go file.
var testFunc = regexp.MustCompile(`(?m)^func (Test\w*)\(`)

// The committed list holds rows the gating -skip can apply safely: each top
// level names a Test function of its own package — a renamed or deleted test
// would leave a row that skips nothing while the nightly job proves nothing —
// and of NO other package, because one `go test -skip` spans every package of
// a shard and a shared name would silently skip another package's test too.
func TestTheCommittedQuarantineNamesTestsOfItsOwnPackageOnly(t *testing.T) {
	f, err := os.Open(committedList)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := parse(f)
	if err != nil {
		t.Fatalf("%s: %v", committedList, err)
	}
	const tree = "../../.."
	// Every top-level test name of the module, by import path.
	byPackage := map[string][]string{}
	err = filepath.WalkDir(tree, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != tree && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_") || d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(tree, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := modulePath
		if rel != "." {
			pkg += "/" + filepath.ToSlash(rel)
		}
		for _, m := range testFunc.FindAllStringSubmatch(string(src), -1) {
			byPackage[pkg] = append(byPackage[pkg], m[1])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", tree, err)
	}
	if len(byPackage) < 100 {
		t.Fatalf("found test functions in %d package(s): the walk is not reading this module", len(byPackage))
	}
	for _, e := range entries {
		top := regexp.MustCompile("^" + e.Levels[0] + "$")
		own := false
		for pkg, names := range byPackage {
			for _, name := range names {
				if !top.MatchString(name) {
					continue
				}
				if pkg == e.Package {
					own = true
				} else {
					t.Errorf("%s line %d: %s also matches %s.%s — the gating -skip spans every package of a shard, so it would skip that test too", committedList, e.Line, e.Name(), pkg, name)
				}
			}
		}
		if !own {
			t.Errorf("%s line %d: %s names no Test function of %s — renamed or removed, the row skips nothing and the nightly job proves nothing (#%s)", committedList, e.Line, e.Name(), e.Package, e.Ticket)
		}
	}
}

// A row and its own subtest row skip the parent in either file order: Go's
// alternation stops at the first alternative that matches a name, and a
// subtest row matches its parent's name only partially — which never skips —
// so a parent listed after its subtest would still run its body in the gate.
func TestAQuarantinedParentIsSkippedWhateverTheRowOrder(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go is required to prove the pattern against the real matcher: %v", err)
	}
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod": "module example.com/q\n\ngo 1.22\n",
		"q_test.go": `package q

import "testing"

func TestNest(t *testing.T) {
	t.Log("PARENT BODY RAN")
	t.Run("sub", func(t *testing.T) {})
}
func TestHealthy(t *testing.T) {}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	parent, child := modulePath+"/q\tTestNest\t#1", modulePath+"/q\tTestNest/sub\t#1"
	for name, rows := range map[string][]string{
		"parent first": {parent, child},
		"child first":  {child, parent},
	} {
		t.Run(name, func(t *testing.T) {
			entries, err := parse(strings.NewReader(strings.Join(rows, "\n") + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(goBin, "test", "-count=1", "-v", "-skip", pattern(entries), ".")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go test -skip %q: %v\n%s", pattern(entries), err, out)
			}
			if strings.Contains(string(out), "PARENT BODY RAN") || !strings.Contains(string(out), "--- PASS: TestHealthy") {
				t.Fatalf("-skip %q ran the quarantined parent:\n%s", pattern(entries), out)
			}
		})
	}
}
