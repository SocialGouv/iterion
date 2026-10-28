package liveledger

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryLiveTestIsInvocableFromItsOwnTarget enforces the half of the
// #1422 class that TestEveryLiveTestRecordsToTheLedger only ASSUMES:
// its classification dismisses broad -run filters as out of scope
// "because each member is enforced through its own specific target".
// Three members had no such target — the kimi and grok permission-gate
// denials and the codex web_search proof — while
// docs/e2e-coverage-matrix.md cites all three as the live proof of
// their row.
//
// What that costs, and why this is not tidiness: a live test reachable
// only from test:live (5 h) or test:live:feats:all (2 h) runs under
// THAT target's ledger row, and an aggregate row merges its members
// fail-sticky. No verdict, cost or date is ever attributable to the
// test itself, so its own row reads `never` for the life of the repo
// and `task test:live:status` cannot answer "does this one still
// pass?" — the question the ledger exists for (#1423).
func TestEveryLiveTestIsInvocableFromItsOwnTarget(t *testing.T) {
	root := repoRoot(t)
	targets, err := EnumerateLiveTargets(filepath.Join(root, TaskfileRelPath))
	if err != nil {
		t.Fatalf("EnumerateLiveTargets: %v", err)
	}
	sources, err := readLiveTestSources(filepath.Join(root, "e2e"))
	if err != nil {
		t.Fatalf("read live test sources: %v", err)
	}
	fileOf := liveTestFiles(sources)
	// Anti-vacuity: with an empty declaration set the loop below finds
	// no orphan and the guard reports green on a reader that read
	// nothing.
	if len(fileOf) < 40 {
		t.Fatalf("only %d TestLive_* function(s) parsed out of e2e/ — the source reader or the function regexp is broken; an empty set would make this guard pass vacuously (HEAD declares 60)", len(fileOf))
	}
	attributable, err := attributableTargets(targets, fileOf)
	if err != nil {
		t.Fatalf("attributableTargets: %v", err)
	}
	var orphans []string
	for fn := range fileOf {
		if len(attributable[fn]) == 0 {
			orphans = append(orphans, fn)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Fatalf("the following %d live test(s) have no `task test:live:*` target that runs them on their own — they run only inside test:live / test:live:feats:all, whose row merges every member fail-sticky, so no verdict is attributable to them and their own row stays `never` forever:\n  - %s\n\nGive each one a target anchored on its function name (test:live:feat:permission-ask is the shape), then seed its row with `go run ./cmd/iterion-live-status -write-back`.",
			len(orphans), strings.Join(orphans, "\n  - "))
	}
}

// TestEveryRecordingTargetHasALedgerRow holds the other half of "`never`
// is written, never elided" (#1422): the committed ledger carries a row
// for every target a run could record to. An ABSENT row is strictly
// worse than a `never` one — `task test:live:status` lists it from the
// Taskfile at read time, so the gap shows up only on the operator's
// screen and never in the committed file two revisions get diffed on.
// Measured at HEAD before this guard existed: test:live:openai-compatible
// and test:live:feat:workspace-commands had drifted out of the file,
// which also means every "N of M targets read never" count quoted from
// the ledger was taken on the wrong M.
func TestEveryRecordingTargetHasALedgerRow(t *testing.T) {
	root := repoRoot(t)
	targets, err := EnumerateLiveTargets(filepath.Join(root, TaskfileRelPath))
	if err != nil {
		t.Fatalf("EnumerateLiveTargets: %v", err)
	}
	var recording []string
	for _, tt := range targets {
		if tt.Records {
			recording = append(recording, tt.Name)
		}
	}
	// Anti-vacuity: an empty recording set demands nothing of the
	// ledger and would pass on a broken Taskfile parse.
	if len(recording) < 40 {
		t.Fatalf("only %d recording target(s) enumerated from %s — the Taskfile parser is broken; an empty set would make this guard pass vacuously (HEAD declares 59)", len(recording), TaskfileRelPath)
	}
	ledger, err := Load(filepath.Join(root, DefaultRelPath))
	if err != nil {
		t.Fatalf("Load ledger: %v", err)
	}
	var missing []string
	for _, name := range recording {
		if _, ok := ledger.Get(name); !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the following %d recording target(s) have no row in %s — a reader of the committed file cannot tell them from targets that do not exist:\n  - %s\n\nSeed them with `go run ./cmd/iterion-live-status -write-back` and commit the file (and reconcile pkg/backend/backendcaps/live_seed.json, which TestLiveLedgerZeroMove compares cell-for-cell).",
			len(missing), DefaultRelPath, strings.Join(missing, "\n  - "))
	}
}

// liveTestFiles maps every TestLive_* function the e2e sources declare
// to the file declaring it. The file is the surface unit attribution
// below is measured against.
func liveTestFiles(sources map[string]string) map[string]string {
	out := map[string]string{}
	for file, src := range sources {
		for _, m := range funcHeaderRE.FindAllStringSubmatch(src, -1) {
			if strings.HasPrefix(m[1], "TestLive_") {
				out[m[1]] = file
			}
		}
	}
	return out
}

// attributableTargets maps each live test to the targets whose verdict
// would be ITS verdict: the target's -run pattern matches it and
// matches nothing declared outside its own source file. One file is one
// surface, so a target scoped to one file owns a row that means
// something — test:live:openai-compatible ships the two functions of
// one file under one row on purpose, and the ledger's merge rule makes
// that row honest. test:live and test:live:feats:all reach across
// files, so they make nobody attributable.
//
// Matching uses Go's own -run semantics: an unanchored regexp over the
// test name, where the part before the first `/` applies to the
// top-level function. An empty pattern filters nothing — the target
// would run every live test of every file — so it is attributable for
// nobody, which is what skipping it yields.
//
// The residual this rule does NOT catch, by construction: two variants
// of one file that a single unanchored pattern selects together, where
// the second has its own (costlier) target — #2784.
func attributableTargets(targets []LiveTarget, fileOf map[string]string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, tt := range targets {
		if tt.RunPattern == "" {
			continue
		}
		top := tt.RunPattern
		if i := strings.Index(top, "/"); i >= 0 {
			top = top[:i]
		}
		re, err := regexp.Compile(top)
		if err != nil {
			return nil, fmt.Errorf("target %s: -run pattern %q does not compile as a Go test filter: %w", tt.Name, tt.RunPattern, err)
		}
		var matched []string
		files := map[string]bool{}
		for fn, file := range fileOf {
			if re.MatchString(fn) {
				matched = append(matched, fn)
				files[file] = true
			}
		}
		if len(matched) == 0 || len(files) > 1 {
			continue
		}
		for _, fn := range matched {
			out[fn] = append(out[fn], tt.Name)
		}
	}
	return out, nil
}

// attributableTargets' table, pinned on the shapes the real Taskfile
// writes: the anchored single test, the one-file pair that shares a row
// on purpose, the cross-file aggregate that must bless nobody, and the
// two patterns that select nothing.
func TestAttributableTargets(t *testing.T) {
	fileOf := map[string]string{
		"TestLive_Feat_Permission_Deny":            "live_feat_permission_test.go",
		"TestLive_Feat_Permission_Deny_Kimi":       "live_feat_permission_kimi_test.go",
		"TestLive_Feat_OpenAICompatible_InProcess": "live_feat_openai_compatible_test.go",
		"TestLive_Feat_OpenAICompatible_Sandboxed": "live_feat_openai_compatible_test.go",
	}
	got, err := attributableTargets([]LiveTarget{
		{Name: "anchored", RunPattern: "TestLive_Feat_Permission_Deny$"},
		{Name: "one-file-pair", RunPattern: "TestLive_Feat_OpenAICompatible"},
		{Name: "cross-file-aggregate", RunPattern: "TestLive_Feat_"},
		{Name: "compile-only", RunPattern: "^$"},
		{Name: "no-pattern", RunPattern: ""},
	}, fileOf)
	if err != nil {
		t.Fatalf("attributableTargets: %v", err)
	}
	// The anchored pattern must NOT reach the _Kimi variant: the `$`
	// is what separates the two targets, and dropping it (as the
	// feature-dev target does) would silently run both.
	if names := got["TestLive_Feat_Permission_Deny"]; len(names) != 1 || names[0] != "anchored" {
		t.Fatalf("anchored exact: got %v", names)
	}
	if names := got["TestLive_Feat_Permission_Deny_Kimi"]; len(names) != 0 {
		t.Fatalf("the kimi variant has no target of its own, yet got %v — only the cross-file aggregate matches it, and an aggregate row is not its verdict", names)
	}
	for _, fn := range []string{"TestLive_Feat_OpenAICompatible_InProcess", "TestLive_Feat_OpenAICompatible_Sandboxed"} {
		if names := got[fn]; len(names) != 1 || names[0] != "one-file-pair" {
			t.Fatalf("%s: a target scoped to one file is attributable for its members: got %v", fn, names)
		}
	}
	for _, name := range []string{"cross-file-aggregate", "compile-only", "no-pattern"} {
		for fn, names := range got {
			for _, n := range names {
				if n == name {
					t.Fatalf("%s must bless nobody, yet it is attributable for %s", name, fn)
				}
			}
		}
	}
	if _, err := attributableTargets([]LiveTarget{{Name: "broken", RunPattern: "TestLive_(["}}, fileOf); err == nil {
		t.Fatal("a -run pattern that does not compile must be an explicit error, not a target silently treated as matching nothing")
	}
}

// liveTestFiles keeps only the live tests, and the LAST declaration
// wins per name — the regexp walk is the same one the sibling guard
// uses, so a non-live helper must not enter the attribution universe.
func TestLiveTestFiles(t *testing.T) {
	got := liveTestFiles(map[string]string{
		"a_live_test.go": "package e2e\n\nfunc TestLive_One(t *testing.T) {}\n\nfunc helper() {}\n",
		"b_test.go":      "package e2e\n\nfunc TestBundle_Two(t *testing.T) {}\n",
	})
	if got["TestLive_One"] != "a_live_test.go" {
		t.Fatalf("live test not mapped to its file: %v", got)
	}
	if _, ok := got["TestBundle_Two"]; ok {
		t.Fatalf("a non-live test entered the universe: %v", got)
	}
	if _, ok := got["helper"]; ok {
		t.Fatalf("a non-test function entered the universe: %v", got)
	}
}
