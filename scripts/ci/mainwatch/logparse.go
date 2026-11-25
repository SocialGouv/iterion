package main

import (
	"regexp"
	"slices"
	"strings"
)

// Failure is one failing test read from a job log. Test is the `go test -run`
// path as go test prints it (TestX or TestX/sub_case); it is empty when the
// package failed with no test named — a build or setup failure, or a panic
// outside any test.
type Failure struct {
	Package string
	Test    string
}

var (
	// GitHub prefixes every log line with an RFC 3339 timestamp.
	logTimestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z `)
	ansiEscape   = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")
	failLine     = regexp.MustCompile(`^\s*--- FAIL: (\S+) \(`)
	// go test's per-package verdict: `FAIL\t<pkg>\t1.2s`, or
	// `FAIL\t<pkg> [build failed]` / `[setup failed]`.
	pkgFailLine = regexp.MustCompile(`^FAIL\t(\S+)(\t| \[)`)
	pkgOKLine   = regexp.MustCompile(`^ok  \t(\S+)`)
	// A test binary that hits -timeout lists the tests still running.
	timeoutPanic   = regexp.MustCompile(`^panic: test timed out after `)
	runningTest    = regexp.MustCompile(`^\t\t(\S+) \(`)
	runningHeading = regexp.MustCompile(`^\trunning tests:$`)
)

// parseFailures reads the failing tests out of one job log. go test buffers a
// package's output and prints it as one block ending with that package's
// verdict line, so the FAIL lines seen since the previous verdict belong to
// the package the next `FAIL\t<pkg>` names. A log with no FAIL at all yields
// nothing: the job died outside go test (infrastructure, a cancelled step).
func parseFailures(log string) []Failure {
	var out []Failure
	var pending []string
	inTimeout := false
	for _, raw := range strings.Split(log, "\n") {
		line := ansiEscape.ReplaceAllString(logTimestamp.ReplaceAllString(strings.TrimRight(raw, "\r"), ""), "")
		if inTimeout {
			if runningHeading.MatchString(line) {
				continue
			}
			if m := runningTest.FindStringSubmatch(line); m != nil {
				pending = append(pending, m[1])
				continue
			}
			inTimeout = false
		}
		switch {
		case timeoutPanic.MatchString(line):
			inTimeout = true
		case failLine.MatchString(line):
			pending = append(pending, failLine.FindStringSubmatch(line)[1])
		case pkgFailLine.MatchString(line):
			pkg := pkgFailLine.FindStringSubmatch(line)[1]
			if len(pending) == 0 {
				out = append(out, Failure{Package: pkg})
			}
			for _, t := range pending {
				out = append(out, Failure{Package: pkg, Test: t})
			}
			pending = nil
		case pkgOKLine.MatchString(line):
			pending = nil
		}
	}
	return leaves(out)
}

// leaves keeps, per package, the deepest failing tests: a failing subtest
// fails its parent too, and naming the parent would quarantine every sibling
// that passed. The result is sorted by package then test, each once.
func leaves(fs []Failure) []Failure {
	var out []Failure
	for _, f := range fs {
		parent := false
		for _, g := range fs {
			if f.Package == g.Package && f.Test != g.Test && (f.Test == "" || strings.HasPrefix(g.Test, f.Test+"/")) {
				parent = true
				break
			}
		}
		if !parent {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b Failure) int {
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		return strings.Compare(a.Test, b.Test)
	})
	return slices.Compact(out)
}

// literalLevel is a test-name level whose every character means itself in a
// go test -run pattern, except `.`, which matches itself among others — the
// row selects the test, at worst with a same-length sibling.
var literalLevel = regexp.MustCompile(`^[A-Za-z0-9_\-.=:,]+$`)

// quarantinePath is the -run path a quarantine row names for a failing test:
// the test's own path, cut at the first level a row cannot spell literally
// (scripts/ci/testquarantine reads `* + ?` as regexp operators and refuses
// anything else outside its alphabet). A top-level name is a Go identifier,
// so the result always names at least the Test function.
func quarantinePath(test string) string {
	levels := strings.Split(test, "/")
	kept := levels[:1]
	for _, l := range levels[1:] {
		if !literalLevel.MatchString(l) {
			break
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "/")
}
