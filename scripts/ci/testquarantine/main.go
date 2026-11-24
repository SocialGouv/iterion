// Command testquarantine reads the checked-in flake quarantine
// (scripts/ci/test-quarantine.tsv) and prints what the CI jobs consume:
//
//	testquarantine -list scripts/ci/test-quarantine.tsv -skip      # go test -skip value for the gating jobs
//	testquarantine -list scripts/ci/test-quarantine.tsv -pkgs      # import paths holding a quarantined test
//	testquarantine -list scripts/ci/test-quarantine.tsv -run       # go test -run value for the nightly job
//	testquarantine -list scripts/ci/test-quarantine.tsv -tickets   # the owning ticket numbers, one per line
//	testquarantine -list scripts/ci/test-quarantine.tsv -verify LOG  # every row PASSed in a `go test -v` log
//
// Consume it through an assignment, never inline command substitution, so a
// refused list stops the step instead of degrading into an empty `-skip`:
//
//	quarantine=$(go run -mod=vendor ./scripts/ci/testquarantine -list scripts/ci/test-quarantine.tsv -skip)
//	go test -mod=vendor -count=1 -skip "$quarantine" $pkgs
//
// A row is `import-path<TAB>test<TAB>#ticket`. `test` is a `go test -run`
// path: slash-separated levels, each a regexp anchored here as ^level$ and
// limited to letters, digits and `_ - . * + ? = : ,` — no `|`, `/`, group or
// class, so the alternation this tool builds splits exactly where Go's own
// -run/-skip matcher splits it. An empty list prints an empty -skip (nothing
// skipped) and refuses -run, whose empty value would select every test.
//
// Exit status: 0 on success, 1 when the list or the log refuses, 2 on usage
// or I/O.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// modulePath is the import-path prefix every row must carry: the gating jobs
// test this module and nothing else.
const modulePath = "github.com/SocialGouv/iterion"

func main() {
	list := flag.String("list", "", "the quarantine TSV (import-path TAB test TAB #ticket)")
	skip := flag.Bool("skip", false, "print the go test -skip value")
	runRe := flag.Bool("run", false, "print the go test -run value (refused on an empty list)")
	pkgs := flag.Bool("pkgs", false, "print the import paths holding a quarantined test, space-joined")
	tickets := flag.Bool("tickets", false, "print the owning ticket numbers, one per line")
	verify := flag.String("verify", "", "a `go test -v` log in which every row must have PASSed")
	flag.Parse()

	if err := run(*list, *skip, *runRe, *pkgs, *tickets, *verify, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "testquarantine:", err)
		var e *exitError
		if errors.As(err, &e) {
			os.Exit(e.code)
		}
		os.Exit(2)
	}
}

// exitError carries its own process exit code: 1 is a refusal the CI step must
// surface as red, 2 is usage or I/O.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func fail(code int, format string, a ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, a...)}
}

func run(listPath string, skip, runRe, pkgs, tickets bool, verifyLog string, out io.Writer) error {
	modes := 0
	for _, on := range []bool{skip, runRe, pkgs, tickets, verifyLog != ""} {
		if on {
			modes++
		}
	}
	if listPath == "" || modes != 1 {
		return fail(2, "usage: -list FILE and exactly one of -skip, -run, -pkgs, -tickets, -verify LOG")
	}
	f, err := os.Open(listPath)
	if err != nil {
		return fail(2, "open %s: %v", listPath, err)
	}
	defer f.Close()
	entries, err := parse(f)
	if err != nil {
		return fail(1, "%s: %v", listPath, err)
	}
	switch {
	case skip:
		fmt.Fprintln(out, pattern(entries))
	case runRe:
		if len(entries) == 0 {
			return fail(1, "%s is empty: an empty -run would select every test", listPath)
		}
		fmt.Fprintln(out, pattern(entries))
	case pkgs:
		fmt.Fprintln(out, strings.Join(packages(entries), " "))
	case tickets:
		for _, n := range ticketNumbers(entries) {
			fmt.Fprintln(out, n)
		}
	default:
		log, err := os.ReadFile(verifyLog)
		if err != nil {
			return fail(2, "read %s: %v", verifyLog, err)
		}
		if err := verifyPassed(entries, string(log)); err != nil {
			return fail(1, "%v", err)
		}
	}
	return nil
}

// entry is one quarantined test.
type entry struct {
	Package string   // import path
	Levels  []string // the -run path split on `/`, each level unanchored
	Ticket  string   // digits, without the `#`
	Line    int
}

// Name is the row's test path as written.
func (e entry) Name() string { return strings.Join(e.Levels, "/") }

var (
	levelChars = regexp.MustCompile(`^[A-Za-z0-9_\-.*+?=:,]+$`)
	ticketRef  = regexp.MustCompile(`^#([1-9][0-9]*)$`)
)

// parse reads the TSV and refuses any row a CI job could misread. Blank lines
// and lines starting with `#` are comments.
func parse(r io.Reader) ([]entry, error) {
	var entries []entry
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d: want import-path<TAB>test<TAB>#ticket, got %d field(s): %q", line, len(fields), text)
		}
		pkg, test, ticket := fields[0], fields[1], fields[2]
		if pkg != modulePath && !strings.HasPrefix(pkg, modulePath+"/") {
			return nil, fmt.Errorf("line %d: import path %q is outside module %s", line, pkg, modulePath)
		}
		m := ticketRef.FindStringSubmatch(ticket)
		if m == nil {
			return nil, fmt.Errorf("line %d: %q carries no ticket: a quarantined test names the open issue that owns its fix as #<number>", line, test)
		}
		levels := strings.Split(test, "/")
		for i, level := range levels {
			if !levelChars.MatchString(level) {
				return nil, fmt.Errorf("line %d: level %d of %q is empty or holds a character outside [A-Za-z0-9_-.*+?=:,]", line, i+1, test)
			}
			if _, err := regexp.Compile("^" + level + "$"); err != nil {
				return nil, fmt.Errorf("line %d: level %d of %q is not a regexp: %v", line, i+1, test, err)
			}
		}
		if !strings.HasPrefix(levels[0], "Test") {
			return nil, fmt.Errorf("line %d: %q does not name a Test function at its top level", line, test)
		}
		key := pkg + "\t" + test
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("line %d: %s %s is already quarantined at line %d", line, pkg, test, prev)
		}
		seen[key] = line
		entries = append(entries, entry{Package: pkg, Levels: levels, Ticket: m[1], Line: line})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// pattern is the go test -run/-skip value selecting exactly the rows: one
// alternative per row, each level anchored. Go's matcher splits the value on
// unparenthesised `|` and `/`, which parse keeps out of every level. Shorter
// paths come first: Go's alternation stops at the first alternative that
// matches a name, and a subtest row matches its parent's name only partially,
// which never skips it — so a parent row after its subtest's would leave the
// parent's body running.
func pattern(entries []entry) string {
	entries = slices.Clone(entries)
	slices.SortStableFunc(entries, func(a, b entry) int { return len(a.Levels) - len(b.Levels) })
	alts := make([]string, 0, len(entries))
	for _, e := range entries {
		levels := make([]string, len(e.Levels))
		for i, level := range e.Levels {
			levels[i] = "^" + level + "$"
		}
		alts = append(alts, strings.Join(levels, "/"))
	}
	return strings.Join(alts, "|")
}

// packages lists the import paths in first-seen order, each once.
func packages(entries []entry) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		if !seen[e.Package] {
			seen[e.Package] = true
			out = append(out, e.Package)
		}
	}
	return out
}

// ticketNumbers lists the ticket numbers in first-seen order, each once.
func ticketNumbers(entries []entry) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		if !seen[e.Ticket] {
			seen[e.Ticket] = true
			out = append(out, e.Ticket)
		}
	}
	return out
}

// resultLine is one `go test -v` verdict line; subtests are indented.
var resultLine = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): (\S+) \(`)

// verifyPassed refuses a log in which some row has no PASS line at its own
// depth: a -run that selected nothing, or a test that skipped, proves nothing.
func verifyPassed(entries []entry, log string) error {
	if len(entries) == 0 {
		return errors.New("the quarantine is empty: there is nothing to verify")
	}
	type verdict struct{ status, name string }
	var verdicts []verdict
	for _, line := range strings.Split(log, "\n") {
		if m := resultLine.FindStringSubmatch(line); m != nil {
			verdicts = append(verdicts, verdict{m[1], m[2]})
		}
	}
	var missing []string
	for _, e := range entries {
		matchers := make([]*regexp.Regexp, len(e.Levels))
		for i, level := range e.Levels {
			matchers[i] = regexp.MustCompile("^" + level + "$")
		}
		passed := false
		for _, v := range verdicts {
			parts := strings.Split(v.name, "/")
			if v.status != "PASS" || len(parts) != len(matchers) {
				continue
			}
			all := true
			for i, m := range matchers {
				if !m.MatchString(parts[i]) {
					all = false
				}
			}
			if all {
				passed = true
			}
		}
		if !passed {
			missing = append(missing, fmt.Sprintf("%s %s (#%s)", e.Package, e.Name(), e.Ticket))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d quarantined test(s) did not PASS in the log — not selected, skipped or failed: %s", len(missing), strings.Join(missing, "; "))
	}
	return nil
}
