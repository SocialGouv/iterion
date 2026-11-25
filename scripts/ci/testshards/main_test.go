package main

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func fixture(n int) []string {
	var pkgs []string
	for i := 0; i < n; i++ {
		pkgs = append(pkgs, fmt.Sprintf("example.com/m/pkg%02d", i))
	}
	return pkgs
}

// The partition a merge-queue entry relies on: three matrix legs of one commit
// must agree even though each runs `go list` in its own pod.
func TestShardByHashIsDeterministicAcrossInputOrder(t *testing.T) {
	pkgs := fixture(60)
	want := shardByHash(pkgs, 3)

	shuffled := append([]string(nil), pkgs...)
	rng := rand.New(rand.NewSource(42))
	rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
	got := shardByHash(shuffled, 3)

	for i := range want {
		if strings.Join(want[i], ",") != strings.Join(got[i], ",") {
			t.Fatalf("shard %d changed with input order:\nwant %v\ngot  %v", i, want[i], got[i])
		}
	}
}

func TestShardByHashPartitionsTotalityAndDisjointness(t *testing.T) {
	pkgs := fixture(97)
	shards := shardByHash(pkgs, 5)
	if len(shards) != 5 {
		t.Fatalf("want 5 shards, got %d", len(shards))
	}
	seen := map[string]int{}
	for i, shard := range shards {
		for _, p := range shard {
			if prev, dup := seen[p]; dup {
				t.Fatalf("package %s in shards %d and %d", p, prev, i)
			}
			seen[p] = i
		}
	}
	if len(seen) != len(pkgs) {
		t.Fatalf("want %d packages placed, got %d", len(pkgs), len(seen))
	}
}

// Pins the hash itself: a change here silently repartitions every shard (all
// legs of a queue entry still agree, but the balance churns for no reason).
// Regenerate by running `go run ./scripts/ci/testshards -all` against a tree
// with these package paths — do not recompute by hand.
func TestShardByHashGolden(t *testing.T) {
	pkgs := []string{
		"github.com/SocialGouv/iterion/bots",
		"github.com/SocialGouv/iterion/cmd/iterion",
		"github.com/SocialGouv/iterion/e2e",
		"github.com/SocialGouv/iterion/pkg/server",
		"github.com/SocialGouv/iterion/pkg/store",
	}
	got := shardByHash(pkgs, 3)
	want := [][]string{
		{"github.com/SocialGouv/iterion/pkg/store"},
		{"github.com/SocialGouv/iterion/cmd/iterion", "github.com/SocialGouv/iterion/pkg/server"},
		{"github.com/SocialGouv/iterion/bots", "github.com/SocialGouv/iterion/e2e"},
	}
	for i := range want {
		if strings.Join(got[i], ",") != strings.Join(want[i], ",") {
			t.Fatalf("golden shard %d drifted:\nwant %v\ngot  %v", i, want[i], got[i])
		}
	}
}

func TestShardByWeightsLandsHeavyPackagesAlone(t *testing.T) {
	pkgs := []string{"a", "b", "c", "d", "e", "f"}
	table := map[string]float64{"a": 500, "b": 300}
	shards := shardByWeights(pkgs, 3, table)

	// a (500) and b (300) are the heaviest; LPT must give each its own shard
	// before anything lighter is placed.
	lone := 0
	for _, shard := range shards {
		if len(shard) == 1 && (shard[0] == "a" || shard[0] == "b") {
			lone++
		}
	}
	if lone != 2 {
		t.Fatalf("want a and b alone on their shards, got %v", shards)
	}
}

func TestShardByWeightsBalancesLoads(t *testing.T) {
	pkgs := fixture(30)
	table := map[string]float64{}
	for i, p := range pkgs {
		table[p] = float64(100 - i)
	}
	shards := shardByWeights(pkgs, 3, table)
	seen := map[string]bool{}
	for _, shard := range shards {
		for _, p := range shard {
			if seen[p] {
				t.Fatalf("package %s placed twice", p)
			}
			seen[p] = true
		}
	}
	if len(seen) != len(pkgs) {
		t.Fatalf("want %d packages placed, got %d", len(pkgs), len(seen))
	}
}

func TestShardByWeightsIsDeterministicAcrossInputOrder(t *testing.T) {
	pkgs := fixture(40)
	table := map[string]float64{"example.com/m/pkg07": 999}
	want := shardByWeights(pkgs, 3, table)

	shuffled := append([]string(nil), pkgs...)
	rng := rand.New(rand.NewSource(7))
	rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
	got := shardByWeights(shuffled, 3, table)

	for i := range want {
		if strings.Join(want[i], ",") != strings.Join(got[i], ",") {
			t.Fatalf("shard %d changed with input order:\nwant %v\ngot  %v", i, want[i], got[i])
		}
	}
}

func TestEmitPrintsOneShardSpaceJoined(t *testing.T) {
	var out bytes.Buffer
	err := emit(3, 1, false, fixture(9), nil, &out)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := strings.Fields(out.String())
	want := shardByHash(fixture(9), 3)[1]
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("shard 1 printed %v, want %v", got, want)
	}
}

func TestEmitRefusesAnEmptyShardLoudly(t *testing.T) {
	var out bytes.Buffer
	err := emit(3, 2, false, []string{"only/one/pkg"}, nil, &out)
	if err == nil {
		t.Fatal("want an error for an empty shard, got nil")
	}
	var e *exitError
	if !errors.As(err, &e) || e.code != 1 {
		t.Fatalf("want exitError code 1, got %v", err)
	}
}

func TestEmitOutOfRangeIndexIsUsageError(t *testing.T) {
	var out bytes.Buffer
	err := emit(3, 3, false, fixture(9), nil, &out)
	var e *exitError
	if !errors.As(err, &e) || e.code != 2 {
		t.Fatalf("want exitError code 2, got %v", err)
	}
}

func TestLoadWeights(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "weights.tsv")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("happy path with comments and blank lines", func(t *testing.T) {
		table, err := loadWeights(write(t, "# comment\n\na\t120\nb\t3.5\n"))
		if err != nil {
			t.Fatal(err)
		}
		if table["a"] != 120 || table["b"] != 3.5 {
			t.Fatalf("want a=120 b=3.5, got %v", table)
		}
	})

	t.Run("trailing spaces around path and seconds", func(t *testing.T) {
		table, err := loadWeights(write(t, "a \t120\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := table["a"]; !ok {
			t.Fatalf("trailing space on the path silently dropped the row: %v", table)
		}
	})

	t.Run("CRLF line endings", func(t *testing.T) {
		table, err := loadWeights(write(t, "a\t120\r\nb\t1\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(table) != 2 {
			t.Fatalf("CRLF rows lost: %v", table)
		}
	})

	t.Run("NaN Inf zero and negative seconds are refused", func(t *testing.T) {
		for _, bad := range []string{"NaN", "Inf", "+Inf", "0", "-5"} {
			if _, err := loadWeights(write(t, "a\t"+bad+"\n")); err == nil {
				t.Fatalf("seconds %q accepted", bad)
			}
		}
	})

	t.Run("duplicate rows are refused", func(t *testing.T) {
		if _, err := loadWeights(write(t, "a\t120\na\t1\n")); err == nil {
			t.Fatal("duplicate row accepted")
		}
	})

	t.Run("malformed line is a usage error", func(t *testing.T) {
		_, err := loadWeights(write(t, "a 120\n"))
		var e *exitError
		if !errors.As(err, &e) || e.code != 2 {
			t.Fatalf("want exitError code 2, got %v", err)
		}
	})

	t.Run("missing file surfaces the open error", func(t *testing.T) {
		if _, err := loadWeights(filepath.Join(t.TempDir(), "absent.tsv")); err == nil {
			t.Fatal("missing file accepted")
		}
	})
}

func TestEmitSingleShardIsOneNewlineTerminatedLine(t *testing.T) {
	var out bytes.Buffer
	if err := emit(3, 0, false, fixture(9), nil, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("want exactly one newline-terminated line, got %q", got)
	}
	if strings.Contains(got, "#") || strings.Contains(got, "  ") {
		t.Fatalf("package list polluted or double-spaced: %q", got)
	}
}

// One shuffled input per seed: the partition must not depend on the order the
// packages arrived in, whatever the shuffle.
func TestShardPartitionsAreSeedIndependent(t *testing.T) {
	for _, mode := range []string{"hash", "weights"} {
		for seed := int64(1); seed <= 10; seed++ {
			pkgs := fixture(50)
			table := map[string]float64{"example.com/m/pkg13": 800}
			shuffle := func(in []string) []string {
				out := append([]string(nil), in...)
				rng := rand.New(rand.NewSource(seed))
				rng.Shuffle(len(out), func(a, b int) { out[a], out[b] = out[b], out[a] })
				return out
			}
			var want, got [][]string
			if mode == "hash" {
				want, got = shardByHash(pkgs, 3), shardByHash(shuffle(pkgs), 3)
			} else {
				want, got = shardByWeights(pkgs, 3, table), shardByWeights(shuffle(pkgs), 3, table)
			}
			for i := range want {
				if strings.Join(want[i], ",") != strings.Join(got[i], ",") {
					t.Fatalf("%s seed %d: shard %d changed with input order", mode, seed, i)
				}
			}
		}
	}
}

// splitterCall is one `testshards -weights` invocation of the CI workflow:
// the leg count, the optional -skip regexp and the committed table it packs.
type splitterCall struct {
	n       int
	skip    string
	weights string
}

var (
	splitterNFlag       = regexp.MustCompile(`\s-n (\d+)\b`)
	splitterSkipFlag    = regexp.MustCompile(`\s-skip '([^']*)'`)
	splitterSkipToken   = regexp.MustCompile(`(^|\s)--?skip(\s|=|$)`)
	splitterWeightsFlag = regexp.MustCompile(`\s-weights (\S+)`)
)

// harvestWeightedSplitterCalls reads every non-comment workflow command that
// runs ./scripts/ci/testshards with -weights, so a new weighted leg is judged
// against its own table without a second list to keep in sync. Backslash
// continuations are joined first: a flag on a continued line belongs to the
// call.
func harvestWeightedSplitterCalls(workflow string) ([]splitterCall, error) {
	raw, err := os.ReadFile(workflow)
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	var calls []splitterCall
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		first := i + 1
		line := lines[i]
		for strings.HasSuffix(strings.TrimRight(line, " \t"), "\\") && i+1 < len(lines) {
			line = strings.TrimSuffix(strings.TrimRight(line, " \t"), "\\") + " " + strings.TrimSpace(lines[i+1])
			i++
		}
		call, ok, err := parseWeightedSplitterLine(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", workflow, first, err)
		}
		if ok {
			calls = append(calls, call)
		}
	}
	return calls, nil
}

// splitterSpan returns the splitter call's own arguments: from
// ./scripts/ci/testshards to the first unquoted ) ; & or |, so a flag of a
// later command on the same line is never read as the splitter's.
func splitterSpan(line string) (string, bool) {
	at := strings.Index(line, "./scripts/ci/testshards")
	if at < 0 {
		return "", false
	}
	var quote byte
	for j := at; j < len(line); j++ {
		c := line[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ')' || c == ';' || c == '&' || c == '|':
			return line[at:j], true
		}
	}
	return line[at:], true
}

// parseWeightedSplitterLine parses one workflow command. ok is false when it
// does not run the splitter with -weights. A -skip the parser cannot read is
// an error: dropping it would judge a partition CI never runs.
func parseWeightedSplitterLine(line string) (call splitterCall, ok bool, err error) {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return splitterCall{}, false, nil
	}
	span, found := splitterSpan(line)
	if !found || !strings.Contains(span, "-weights") {
		return splitterCall{}, false, nil
	}
	nm := splitterNFlag.FindStringSubmatch(span)
	if nm == nil {
		return splitterCall{}, false, fmt.Errorf("runs the splitter with -weights but no -n: %s", strings.TrimSpace(line))
	}
	n, aerr := strconv.Atoi(nm[1])
	if aerr != nil || n < 1 {
		return splitterCall{}, false, fmt.Errorf("bad -n %q", nm[1])
	}
	wm := splitterWeightsFlag.FindStringSubmatch(span)
	if wm == nil {
		return splitterCall{}, false, fmt.Errorf("-weights carries no path: %s", strings.TrimSpace(line))
	}
	call = splitterCall{n: n, weights: wm[1]}
	if sm := splitterSkipFlag.FindStringSubmatch(span); sm != nil {
		call.skip = sm[1]
	} else if splitterSkipToken.MatchString(span) {
		return splitterCall{}, false, fmt.Errorf("-skip present but not in the -skip '<re>' form the guard parses: %s", strings.TrimSpace(line))
	}
	return call, true, nil
}

// A -skip written in another valid quoting fails the harvest instead of being
// dropped; the single-quoted form CI uses parses to its regexp, a continued
// line keeps its -skip, and a later command's -skip is not the splitter's.
func TestParseWeightedSplitterLineRefusesAnUnparsedSkip(t *testing.T) {
	const base = `pkgs=$(go run ./scripts/ci/testshards -n 3 -weights scripts/ci/test-shards-unit-weights.tsv`
	call, ok, err := parseWeightedSplitterLine(base + ` -skip '(^|/)e2e$')`)
	if err != nil || !ok || call.skip != "(^|/)e2e$" || call.n != 3 {
		t.Fatalf("single-quoted -skip: got %+v ok=%v err=%v", call, ok, err)
	}
	for _, form := range []string{` -skip "(^|/)e2e$")`, ` -skip='(^|/)e2e$')`} {
		if _, _, err := parseWeightedSplitterLine(base + form); err == nil {
			t.Fatalf("%q: an unparsed -skip must fail the harvest, got no error", form)
		}
	}
	if call, ok, err := parseWeightedSplitterLine(`pkgs=$(go run ./scripts/ci/testshards -n 3 -weights scripts/ci/test-shards-race-skip-weights.tsv)`); err != nil || !ok || call.skip != "" || call.weights != "scripts/ci/test-shards-race-skip-weights.tsv" {
		t.Fatalf("a path containing skip is not a -skip flag: got %+v ok=%v err=%v", call, ok, err)
	}
	if call, ok, err := parseWeightedSplitterLine(base + `) && go test -race -skip 'TestFlaky' $pkgs`); err != nil || !ok || call.skip != "" {
		t.Fatalf("a later command's -skip is not the splitter's: got %+v ok=%v err=%v", call, ok, err)
	}

	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	cont := write("cont.yml", "          pkgs=$(go run ./scripts/ci/testshards -n 3 -weights scripts/ci/test-shards-unit-weights.tsv \\\n            -skip '(^|/)e2e$')\n")
	calls, err := harvestWeightedSplitterCalls(cont)
	if err != nil || len(calls) != 1 || calls[0].skip != "(^|/)e2e$" {
		t.Fatalf("continued -skip: got %+v err=%v", calls, err)
	}
	bad := write("bad.yml", "          pkgs=$(go run ./scripts/ci/testshards -n 3 -weights x.tsv -skip \"(^|/)e2e$\")\n")
	if _, err := harvestWeightedSplitterCalls(bad); err == nil {
		t.Fatalf("the harvest must return the parse error of an unparsed -skip")
	}
}

// Unlisted packages weigh 1: with an empty table, LPT spreads them by count.
func TestUnlistedPackagesWeighOne(t *testing.T) {
	shards := shardByWeights([]string{"a", "b", "c", "d"}, 2, map[string]float64{})
	if len(shards[0]) != 2 || len(shards[1]) != 2 {
		t.Fatalf("unlisted packages must weigh 1 and spread 2/2, got %v", shards)
	}
	if w := shardWeight([]string{"x"}, nil); w != 1 {
		t.Fatalf("an unlisted package must weigh 1 in the dump, got %v", w)
	}
}

// The committed tables are the only thing keeping the weighted legs (race-shard
// and test-unit) balanced, and a green leg never surfaces their decay: a row
// left behind by a package rename silently reweights that package to 1, a
// deleted row hides from every row-wise check while its package keeps
// building at weight 1, a package grown past the per-shard ideal cannot be
// packed away by any partition — it alone owns the leg's wall, and the remedy
// is splitting the package, not tuning the table — and k+1 such heavy
// packages force two of them onto one leg even when each stays under the
// ideal. Each workflow call is judged with its own -n, -skip and table, and
// packed exactly as CI packs it.
func TestTheCommittedWeightsTablesPackTheirLegs(t *testing.T) {
	const tree = "../../.."

	// A deleted row (as opposed to one left stale by a rename) passes every
	// row-wise check. The floor drops only when a refresh legitimately removes
	// rows: that refresh commit lowers its entry to the new count. A table
	// absent from this map is a new weighted leg that must state its floor.
	minRows := map[string]int{
		"scripts/ci/test-shards-weights.tsv":      66, // merge_group run 45186345093 (2026-11-23), less e2e, pkg/sandbox/kubernetes and pkg/server (split by test name, #3684)
		"scripts/ci/test-shards-unit-weights.tsv": 39, // merge_group run 44559477253 (2026-11-19), less pkg/sandbox/kubernetes
	}

	calls, err := harvestWeightedSplitterCalls(filepath.Join(tree, ".github", "workflows", "tests.yml"))
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if len(calls) < 2 {
		t.Fatalf("harvested %d weighted splitter call(s) from tests.yml, want >= 2 (race-shard and test-unit) — the harvest no longer matches the workflow", len(calls))
	}

	all, err := listPackagesAt(tree)
	if err != nil {
		t.Fatalf("package list: %v", err)
	}

	// The package set above comes from the `go list` subprocess, whose output
	// the `go test` result cache cannot see: the cache keys this run on the
	// test binary, the flags, and the files this binary itself opens, so a
	// rename or delete that skips its table row would replay a cached PASS
	// here under the local `task test` gate, which runs without -count=1
	// (every CI invocation carries it, so the drift would surface one gate
	// late). Walk the tree so each package directory is opened by this test
	// binary and lands in the run's input log — cmd/go hashes every logged
	// directory's listing at lookup — turning tree drift into a cache miss
	// and a fresh run, which the checks below then judge. The skip list
	// mirrors what `go list ./...` never lists — dot- and underscore-prefixed
	// dirs (cmd/go search.go skips both), vendor trees, testdata,
	// node_modules — so drift under a dir that contributes no package
	// (studio's __tests__) cannot bust the cache for a partition that cannot
	// change.
	err = filepath.WalkDir(tree, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != tree && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_") || d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tree walk: %v", err)
	}

	for _, call := range calls {
		t.Run(fmt.Sprintf("%s -n %d", call.weights, call.n), func(t *testing.T) {
			table, err := loadWeights(filepath.Join(tree, call.weights))
			if err != nil {
				t.Fatalf("committed weights table: %v", err)
			}
			var filter *regexp.Regexp
			if call.skip != "" {
				filter = regexp.MustCompile(call.skip)
			}
			pkgs := exclude(all, filter)

			floor, ok := minRows[call.weights]
			if !ok {
				t.Fatalf("%s has no row floor in minRows — a new weighted leg states the row count of its committed table", call.weights)
			}
			if len(table) < floor {
				t.Fatalf("%s has %d rows, want >= %d — a row was deleted without a refresh and its package silently weighs 1 now", call.weights, len(table), floor)
			}

			known := make(map[string]bool, len(pkgs))
			for _, p := range pkgs {
				known[p] = true
			}
			total := shardWeight(pkgs, table)
			var stale []string
			for path := range table {
				if !known[path] {
					stale = append(stale, path)
				}
			}
			sort.Strings(stale)
			if len(stale) > 0 {
				t.Fatalf("%s: %d row(s) match no package this leg packs — a package renamed, removed or skipped without its weights row: %s", call.weights, len(stale), strings.Join(stale, " "))
			}

			ideal := total / float64(call.n)
			var floors []string
			for path, w := range table {
				if w > ideal {
					floors = append(floors, fmt.Sprintf("%s weighs %.0fs, ideal shard is %.0fs", path, w, ideal))
				}
			}
			sort.Strings(floors)
			if len(floors) > 0 {
				t.Fatalf("%s: %d package(s) outweigh the per-shard ideal and own the leg's wall:\n%s", call.weights, len(floors), strings.Join(floors, "\n"))
			}

			// Balance is a property of the partition, not of any single row:
			// k+1 rows each under the ideal still force two of them onto one
			// leg. Pack what CI packs and hold every leg near the ideal.
			const maxLegRatio = 1.25
			for i, shard := range shardByWeights(pkgs, call.n, table) {
				if w := shardWeight(shard, table); w > maxLegRatio*ideal {
					t.Fatalf("%s: shard %d packs at %.0fs = %.2fx the %.0fs ideal — the heavy end is too concentrated for %d legs; split the heavy package(s), no table packs them away", call.weights, i, w, w/ideal, ideal, call.n)
				}
			}
		})
	}
}
