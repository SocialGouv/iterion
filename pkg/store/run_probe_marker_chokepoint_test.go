package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// RunRetryState.ProbeFrom answers TWO questions, and they expire differently:
// which instant the provider announced (AnnouncedProbeFrom — while that
// instant is ahead, whatever the age of the pull) and whether a probe is in
// flight so nobody owes anything for this park (ProbeInFlight — and only while
// the PULL is recent). Anyone reading the bare field gets one of them wrong:
// the marker deliberately outlives the resume it was written for
// (ClearRunRetry keeps it so the pod that parks can read it), so a
// probe-driven resume that never parks leaves it on the row for good, and a
// later park that is nobody's probe collected the whole free treatment —
// uncharged, uncounted, unfed to the shared circuit, silent on the pull
// request — for as long as the announced instant stayed ahead.
//
// Two doors, then, and a door only holds while it is the ONLY one. What this
// walk locks, exactly: every `.ProbeFrom` selector in non-test first-party Go
// under the module (vendor and third_party excluded), and every spelling of
// the BSON path "probe_from" outside the two files that own the document.
// Test files read the bare field on purpose — a checker that asked an
// accessor would agree with a broken one instead of catching it.

// probeFromAllowed are the files that may name the Go field: its declaration
// and the two accessors here, and the test-only twin of the Mongo row
// (internal/retryprobetest), which models the DATABASE's filter terms — the
// writer, not a reader — and judges both doors from the raw fields on purpose.
func probeFromAllowed() map[string]bool {
	return map[string]bool{
		"pkg/store/run.go":                    true,
		"internal/retryprobetest/store.go":    true,
		"internal/retryprobetest/property.go": true,
	}
}

// probeMarkerDoors are the ONLY functions of pkg/store/run.go allowed to name
// the field: the two questions, each with its own expiry rule. A third
// accessor is a third door — adding `RawProbeFrom()` here and calling it from
// a sweeper was a verified bypass of the previous one-name walk, so the
// allowlist is per FUNCTION, not per file.
func probeMarkerDoors() map[string]bool {
	return map[string]bool{"AnnouncedProbeFrom": true, "ProbeInFlight": true}
}

func TestProbeMarkerIsReadOnlyThroughItsTwoDoors(t *testing.T) {
	root := moduleRootForProbeWalk(t)
	allowed := probeFromAllowed()
	doors := probeMarkerDoors()
	var offenders, thirdDoors []string
	seen := map[string]int{}
	walkProbeMarkerSelectors(t, root, func(rel string, line int, fn string) {
		if !allowed[rel] {
			offenders = append(offenders, fmt.Sprintf("%s:%d (in %s)", rel, line, fn))
			return
		}
		seen[rel]++
		if rel == "pkg/store/run.go" && fn != "RunRetryState" && !doors[fn] {
			thirdDoors = append(thirdDoors, fmt.Sprintf("%s:%d (in %s)", rel, line, fn))
		}
	})
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d site(s) read the probe marker directly instead of through AnnouncedProbeFrom (the wall) or ProbeInFlight (who pays):\n  %s\n"+
			"The two questions expire differently; a reader of the bare field answers one of them wrong.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	sort.Strings(thirdDoors)
	if len(thirdDoors) > 0 {
		t.Errorf("%d accessor(s) of the probe marker beyond the two doors:\n  %s\n"+
			"A function here that hands the field out applies no expiry rule, and the walk cannot tell its callers from the accessors'.",
			len(thirdDoors), strings.Join(thirdDoors, "\n  "))
	}
	// And the half a "clean" verdict cannot distinguish from a lock that is
	// simply gone: a rename refactor rewrites every selector it finds and
	// leaves the string literal this walk matches on, so with no coverage
	// assertion the guard reports locked while nothing is locked at all.
	for rel := range allowed {
		if seen[rel] == 0 {
			t.Fatalf("this walk no longer matches the probe marker in %s: the field was renamed or moved, so the chokepoint is UNLOCKED, not clean (it reports offenders only)", rel)
		}
	}
}

// The BSON path is the other spelling of the same marker, invisible to a walk
// on Go selectors: three Mongo filters name it as a string, and none of them
// can apply an expiry rule. They belong to the document's own two files; a
// fourth site would be a reader of the raw marker added in silence.
func TestProbeMarkerBsonPathIsNamedOnlyByTheDocumentsOwnFiles(t *testing.T) {
	root := moduleRootForProbeWalk(t)
	allowed := map[string]bool{
		"pkg/store/run.go":              true, // the bson/json tags
		"pkg/store/mongo/runs_retry.go": true, // the filters and updates
	}
	var offenders []string
	seen := map[string]int{}
	walkFirstPartyGo(t, root, func(rel string, f *ast.File, fset *token.FileSet) {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !namesProbeFromPath(lit.Value) {
				return true
			}
			if allowed[rel] {
				seen[rel]++
				return true
			}
			offenders = append(offenders, fmt.Sprintf("%s:%d %s", rel, fset.Position(lit.Pos()).Line, lit.Value))
			return true
		})
	})
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d site(s) name the probe marker's BSON path outside the two files that own the document:\n  %s\n"+
			"A filter term on retry_state.probe_from reads the raw marker, and no accessor stands between it and the row.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	for rel := range allowed {
		if seen[rel] == 0 {
			t.Fatalf("this walk no longer matches \"probe_from\" in %s: the path was renamed and the walk is unlocked, not clean", rel)
		}
	}
}

// The wall door's own rule, from the outside: a marker behind `now` is no
// marker, and the age of the pull does not enter into it — the provider's
// dated word about a wall still ahead outlives the resume that read it.
func TestAnnouncedProbeFromRefusesAWaitThatHasEnded(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	ahead := now.Add(time.Hour)
	behind := now.Add(-time.Second)
	longAgo := now.Add(-72 * time.Hour)

	if got := (&RunRetryState{ProbeFrom: &ahead}).AnnouncedProbeFrom(now); got == nil || !got.Equal(ahead) {
		t.Errorf("a marker one hour ahead reads as %v, want the instant itself", got)
	}
	if got := (&RunRetryState{ProbeFrom: &ahead, LastProbeAt: &longAgo}).AnnouncedProbeFrom(now); got == nil || !got.Equal(ahead) {
		t.Errorf("a marker one hour ahead pulled three days ago reads as %v, want the instant itself: the wall is the row's, not the pull's", got)
	}
	if got := (&RunRetryState{ProbeFrom: &behind}).AnnouncedProbeFrom(now); got != nil {
		t.Errorf("a marker one second behind reads as %s, want nothing: that wait is over", got.Format(time.RFC3339))
	}
	if got := (&RunRetryState{ProbeFrom: &now}).AnnouncedProbeFrom(now); got != nil {
		t.Errorf("a marker exactly at now reads as %s, want nothing", got.Format(time.RFC3339))
	}
	if got := (&RunRetryState{}).AnnouncedProbeFrom(now); got != nil {
		t.Errorf("a row with no marker reads as %s, want nothing", got.Format(time.RFC3339))
	}
	var absent *RunRetryState
	if got := absent.AnnouncedProbeFrom(now); got != nil {
		t.Errorf("a nil retry state reads as %s, want nothing", got.Format(time.RFC3339))
	}
}

// And the payment door's: it expires with the PULL, and it is a SUBSET of the
// wall door — whoever answers "a probe is paying for this" must also be able
// to name the wall it set aside.
func TestProbeInFlightExpiresWithThePullAndImpliesTheWall(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	ahead := now.Add(7 * 24 * time.Hour)
	behind := now.Add(-time.Second)
	fresh := now.Add(-time.Minute)
	stale := now.Add(-ProbeMarkerTTL - time.Second)
	edge := now.Add(-ProbeMarkerTTL)

	cases := []struct {
		name string
		row  *RunRetryState
		want bool
	}{
		{"a pull a minute old on a wall a week ahead", &RunRetryState{ProbeFrom: &ahead, LastProbeAt: &fresh}, true},
		{"a pull one second past the TTL", &RunRetryState{ProbeFrom: &ahead, LastProbeAt: &stale}, false},
		{"a pull exactly at the TTL", &RunRetryState{ProbeFrom: &ahead, LastProbeAt: &edge}, false},
		{"a fresh pull whose instant has passed", &RunRetryState{ProbeFrom: &behind, LastProbeAt: &fresh}, false},
		{"a marker with no pull stamp at all", &RunRetryState{ProbeFrom: &ahead}, false},
		{"a row with no marker", &RunRetryState{LastProbeAt: &fresh}, false},
		{"no retry state", nil, false},
	}
	for _, c := range cases {
		if got := c.row.ProbeInFlight(now); got != c.want {
			t.Errorf("%s: ProbeInFlight = %v, want %v", c.name, got, c.want)
		}
		if c.row.ProbeInFlight(now) && c.row.AnnouncedProbeFrom(now) == nil {
			t.Errorf("%s: a probe in flight whose wall reads as nothing — the free arming would restore a nil instant", c.name)
		}
	}
}

// namesProbeFromPath reports a string literal that NAMES the field in the
// document — the Mongo path (`retryPath("probe_from")`, a dotted
// "retry_state.probe_from") or the struct tag that declares it — and not a
// failure message that happens to print it.
func namesProbeFromPath(quoted string) bool {
	unquoted := strings.Trim(quoted, "`\"")
	switch {
	case unquoted == "probe_from":
		return true
	case strings.HasSuffix(unquoted, ".probe_from"):
		return true
	case strings.Contains(unquoted, `bson:"probe_from`), strings.Contains(unquoted, `json:"probe_from`):
		return true
	}
	return false
}

func moduleRootForProbeWalk(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file))) // pkg/store/<file> -> module root
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	return root
}

// walkProbeMarkerSelectors reports each `.ProbeFrom` selector of the module's
// own non-test Go, whatever the expression on its left — what matters is that
// the NAME is not spelled, not which variable spells it — together with the
// function or type declaration that encloses it.
func walkProbeMarkerSelectors(t *testing.T, root string, report func(rel string, line int, fn string)) {
	t.Helper()
	walkFirstPartyGo(t, root, func(rel string, f *ast.File, fset *token.FileSet) {
		var stack []string
		ast.Inspect(f, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.FuncDecl:
				stack = append(stack, d.Name.Name)
			case *ast.TypeSpec:
				stack = append(stack, d.Name.Name)
			case *ast.SelectorExpr:
				if d.Sel != nil && d.Sel.Name == "ProbeFrom" {
					in := "(file scope)"
					if len(stack) > 0 {
						in = stack[len(stack)-1]
					}
					report(rel, fset.Position(d.Sel.Pos()).Line, in)
				}
			}
			return true
		})
	})
}

// walkFirstPartyGo parses every non-test .go file of the module's own code —
// every top-level directory except the vendored trees — and hands it to fn.
// bots/, ci/, contrib/, e2e/, examples/ and scripts/ hold first-party Go that
// does not touch the retry row today; walking them is what keeps a new reader
// there from being added in silence.
func walkFirstPartyGo(t *testing.T, root string, fn func(rel string, f *ast.File, fset *token.FileSet)) {
	t.Helper()
	skipDir := map[string]bool{
		"vendor": true, "third_party": true, "node_modules": true, "testdata": true,
		".git": true, ".works": true, ".claude": true, "graphify-out": true,
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		fn(filepath.ToSlash(rel), f, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// The probe's cost has a MILD ERROR, and the pages that size it say so.
//
// ProbeInFlight expires with the PULL, so a resume the due pass reaches more
// than ProbeMarkerTTL after it is refunded by nobody — the tenant's monthly
// run quota pays for that launch (pkg/server,
// TestSweepDueRetries_AResumeOlderThanTheMarkerTTLIsMetered) — and the park it
// causes is charged an attempt (pkg/runner,
// TestArmUsageWindowRetry_AStaleMarkerParkIsChargedAndReopensTheWaitsProbeLedger).
// An operator sizing a quota from a flat "a probe spends no attempt and no
// quota unit" is sizing the best case; the worst is max_attempts metered
// launches and max_attempts attempts per wait, which is the number that
// decides whether probing is affordable on a parked fleet.
//
// So every statement of that cost in the pages an operator reads (docs/*.md;
// the dated records under docs/adr, docs/changelog and docs/reviews keep
// their own history) names what makes it false. A page may stop stating the
// cost — it may not state it flat.
func TestProbeCostClaimsNameTheMarkerTTL(t *testing.T) {
	// The claim, in every spelling the pages use — matched on prose with
	// markdown emphasis stripped, because "spends **no attempt**" is the
	// same promise as "spends no attempt".
	claims := []string{
		"spends no attempt", "spend no attempt", "no attempt and no",
		"free of its attempt", "free of the attempt",
		"not a quota unit", "no monthly run-quota", "no quota unit",
		"re-arms for free", "re-arm for free",
	}
	// What makes it false, named: the marker's TTL is the whole exception.
	qualifiers := []string{"probemarkerttl", "marker's ttl", "marker ttl"}
	// The pages that state the cost today. A page that stops stating it is a
	// deliberate edit; the claim drifting out of this walk's reach in
	// silence is how the guard would pass on anything.
	stated := []string{"docs/quotas-and-limits.md", "docs/scheduling.md", "docs/usage-caps.md"}

	root := moduleRootForProbeWalk(t)
	entries, err := os.ReadDir(filepath.Join(root, "docs"))
	if err != nil {
		t.Fatalf("read docs/: %v", err)
	}
	seen := map[string]int{}
	var flat []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		rel := "docs/" + e.Name()
		raw, rerr := os.ReadFile(filepath.Join(root, "docs", e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", rel, rerr)
		}
		for _, unit := range probeCostUnits(string(raw)) {
			if !strings.Contains(unit.text, "probe") {
				continue
			}
			claim, ok := probeCostFirstMatch(unit.text, claims)
			if !ok {
				continue
			}
			seen[rel]++
			if _, qualified := probeCostFirstMatch(unit.text, qualifiers); !qualified {
				flat = append(flat, fmt.Sprintf("%s:%d — %q", rel, unit.line, claim))
			}
		}
	}
	sort.Strings(flat)
	if len(flat) > 0 {
		t.Errorf("%d statement(s) of the probe's cost promise it flat, with nothing in the same paragraph (or table row) naming store.ProbeMarkerTTL's exception:\n  %s\n"+
			"A resume the due pass reaches more than ProbeMarkerTTL after the pull IS metered and the park it causes IS charged an attempt — at the shipped max_attempts, five of each per wait.",
			len(flat), strings.Join(flat, "\n  "))
	}
	for _, page := range stated {
		if seen[page] == 0 {
			t.Fatalf("this walk no longer sees the probe's cost stated in %s: the phrasing moved, so the guard is DEAD rather than clean (it reports flat claims only)", page)
		}
	}
}

// probeCostUnit is one unit of prose a reader takes a claim away from, with
// the line it starts on.
type probeCostUnit struct {
	line int
	text string
}

// probeCostUnits splits a page into those units: a table ROW stands alone
// (the rows around it qualify nothing), every other run of non-blank lines is
// one paragraph. The text is lowercased with markdown emphasis removed, so a
// phrase split by ** or wrapped in backticks still matches.
func probeCostUnits(page string) []probeCostUnit {
	var units []probeCostUnit
	var para []string
	start := 0
	flush := func() {
		if len(para) > 0 {
			units = append(units, probeCostUnit{line: start, text: probeCostNormalize(strings.Join(para, " "))})
			para = nil
		}
	}
	for i, line := range strings.Split(page, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flush()
		case strings.HasPrefix(trimmed, "|"):
			flush()
			units = append(units, probeCostUnit{line: i + 1, text: probeCostNormalize(trimmed)})
		default:
			if len(para) == 0 {
				start = i + 1
			}
			para = append(para, trimmed)
		}
	}
	flush()
	return units
}

func probeCostNormalize(s string) string {
	return strings.NewReplacer("*", "", "`", "", "_", "").Replace(strings.ToLower(s))
}

// probeCostFirstMatch reports the first of needles the text contains.
func probeCostFirstMatch(text string, needles []string) (string, bool) {
	for _, n := range needles {
		if strings.Contains(text, n) {
			return n, true
		}
	}
	return "", false
}
