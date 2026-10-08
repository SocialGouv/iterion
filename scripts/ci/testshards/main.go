// Command testshards prints the Go test packages of one deterministic shard,
// the mechanism the CI matrix legs use to split `./...` across runners (the
// `race-shard` legs in .github/workflows/tests.yml). The partition is a pure
// function of the package list (and, when given, the weights table): the same
// commit always produces the same shards, which is what lets three matrix legs
// of one merge-queue entry agree without communicating.
//
//	testshards                          # debug dump of every shard (n=3)
//	testshards -n 3 -i 0                # shard 0 of 3, space-joined for `go test`
//	testshards -n 3 -i 1 -skip '(^|/)e2e$' -weights scripts/ci/test-shards-weights.tsv
//
// Consume it through an assignment, never inline command substitution —
// `go test ... $(go run ... testshards ...)` throws the exit code away, so an
// empty-shard refusal would degrade into `go test` with no package arguments:
//
//	pkgs=$(go run -mod=vendor ./scripts/ci/testshards -n 3 -i 0)
//	go test -race -mod=vendor -count=1 $pkgs   # any nonzero exit aborts here
//
// Without -weights the balance is FNV-1a of the import path mod n. With
// -weights (TSV: import-path TAB seconds, # comments) it is LPT bin-packing in
// the total order (weight desc, path asc), ties to the lowest shard index —
// deterministic without a timestamp or network anywhere. A package missing
// from the table weighs 1; a table row matching no package draws a stderr
// warning.
//
// Exit status: 0 when the requested shard printed; nonzero otherwise (1 when
// the requested shard would be empty — a matrix leg must fail loudly, not run
// zero packages — 2 on a usage or I/O error). `go run` collapses child exit
// codes to 1, so consumers can rely on nonzero-ness, not the exact value.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	n := flag.Int("n", 3, "number of shards")
	i := flag.Int("i", -1, "print only this shard's packages (negative with no -all: dump every shard)")
	all := flag.Bool("all", false, "dump every shard with counts and weights")
	skip := flag.String("skip", "", "regexp of import paths to exclude from the partition")
	weights := flag.String("weights", "", "TSV file of import-path TAB seconds to balance by duration (LPT) instead of hashing")
	flag.Parse()

	if err := run(*n, *i, *all, *skip, *weights); err != nil {
		fmt.Fprintln(os.Stderr, "testshards:", err)
		var e *exitError
		if errors.As(err, &e) {
			os.Exit(e.code)
		}
		os.Exit(2)
	}
}

// exitError carries its own process exit code: 1 is a shard-shaped failure the
// matrix leg must surface as red, 2 is usage or I/O.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func fail(code int, format string, a ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, a...)}
}

func run(n, i int, all bool, skip, weightsPath string) error {
	if n < 1 {
		return fail(2, "-n must be >= 1, got %d", n)
	}
	if i >= n {
		return fail(2, "-i %d out of range [0,%d)", i, n)
	}
	var filter *regexp.Regexp
	if skip != "" {
		var err error
		filter, err = regexp.Compile(skip)
		if err != nil {
			return fail(2, "bad -skip regexp %q: %v", skip, err)
		}
	}
	table, err := loadWeightsIfAny(weightsPath)
	if err != nil {
		return err
	}

	pkgs, err := listPackages()
	if err != nil {
		return err
	}
	warnUnknownWeights(table, pkgs)
	return emit(n, i, all, exclude(pkgs, filter), table, os.Stdout)
}

// emit partitions pkgs and prints the requested view. Split from run so the
// empty-shard refusal and both output modes are testable without exec'ing
// `go list`.
func emit(n, i int, all bool, pkgs []string, table map[string]float64, out io.Writer) error {
	if i >= n {
		return fail(2, "-i %d out of range [0,%d)", i, n)
	}
	var shards [][]string
	if table == nil {
		shards = shardByHash(pkgs, n)
	} else {
		shards = shardByWeights(pkgs, n, table)
	}

	if all || i < 0 {
		return dump(shards, table, out)
	}
	if len(shards[i]) == 0 {
		return fail(1, "shard %d of n=%d is empty over %d packages — the matrix leg would run nothing; rebalance -n or the weights table", i, n, len(pkgs))
	}
	fmt.Fprintln(out, strings.Join(shards[i], " "))
	return nil
}

// warnUnknownWeights draws a stderr warning for table rows matching no
// package in the tree — a typo'd or stale row otherwise silently reweights
// that package to 1. Stderr only: stdout must stay the pure package list.
func warnUnknownWeights(table map[string]float64, pkgs []string) {
	if table == nil {
		return // no -weights flag: hash mode, nothing to warn about
	}
	if len(table) == 0 {
		fmt.Fprintln(os.Stderr, "testshards: warning: weights table is empty — balancing by count")
		return
	}
	known := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		known[p] = true
	}
	var unknown []string
	for path := range table {
		if !known[path] {
			unknown = append(unknown, path)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		fmt.Fprintf(os.Stderr, "testshards: warning: %d weights row(s) match no package in ./...: %s\n", len(unknown), strings.Join(unknown, " "))
	}
}

// listPackages runs `go list -mod=vendor ./...` in the working directory, the
// exact list the CI jobs test. go list output is sorted and stable for a given
// tree and toolchain; the sort here is belt and braces.
func listPackages() ([]string, error) {
	out, err := exec.Command("go", "list", "-mod=vendor", "./...").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("go list failed: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("go list failed: %w", err)
	}
	var pkgs []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			pkgs = append(pkgs, line)
		}
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

func exclude(pkgs []string, filter *regexp.Regexp) []string {
	if filter == nil {
		return pkgs
	}
	kept := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		if !filter.MatchString(p) {
			kept = append(kept, p)
		}
	}
	return kept
}

// shardByHash assigns each package to FNV-1a64(path) mod n. Stateless, so a
// merge-queue entry (one commit, three matrix legs) always agrees, and a
// table-free repartition only ever happens when the package set changes.
func shardByHash(pkgs []string, n int) [][]string {
	shards := make([][]string, n)
	for _, p := range pkgs {
		h := fnv.New64a()
		h.Write([]byte(p))
		shards[h.Sum64()%uint64(n)] = append(shards[h.Sum64()%uint64(n)], p)
	}
	for _, shard := range shards {
		sort.Strings(shard)
	}
	return shards
}

// shardByWeights is LPT bin-packing in the total order (weight desc, path
// asc): each package in turn lands on the least-loaded shard, ties to the
// lowest index. Unlisted packages weigh 1 so a stale table degrades to
// balanced-by-count, never to an error.
func shardByWeights(pkgs []string, n int, table map[string]float64) [][]string {
	type item struct {
		path   string
		weight float64
	}
	items := make([]item, 0, len(pkgs))
	for _, p := range pkgs {
		w := 1.0
		if v, ok := table[p]; ok {
			w = v
		}
		items = append(items, item{path: p, weight: w})
	}
	sort.Slice(items, func(a, b int) bool {
		if items[a].weight != items[b].weight {
			return items[a].weight > items[b].weight
		}
		return items[a].path < items[b].path
	})

	shards := make([][]string, n)
	loads := make([]float64, n)
	for _, it := range items {
		min := 0
		for j := 1; j < n; j++ {
			if loads[j] < loads[min] {
				min = j
			}
		}
		shards[min] = append(shards[min], it.path)
		loads[min] += it.weight
	}
	for _, shard := range shards {
		sort.Strings(shard)
	}
	return shards
}

func loadWeightsIfAny(path string) (map[string]float64, error) {
	if path == "" {
		return nil, nil
	}
	return loadWeights(path)
}

func loadWeights(path string) (map[string]float64, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	table := make(map[string]float64)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			return nil, fail(2, "%s: want `import-path<TAB>seconds`, got %q", path, line)
		}
		pkg := strings.TrimSpace(parts[0])
		secs, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		// NaN, +Inf and everything non-positive are refused: any of them
		// wrecks the strict weak ordering the LPT sort relies on.
		if err != nil || secs <= 0 || math.IsInf(secs, 0) || math.IsNaN(secs) {
			return nil, fail(2, "%s: bad seconds for %s: %q", path, pkg, parts[1])
		}
		if _, dup := table[pkg]; dup {
			return nil, fail(2, "%s: duplicate row for %s (a package has one weight)", path, pkg)
		}
		table[pkg] = secs
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return table, nil
}

func dump(shards [][]string, table map[string]float64, out io.Writer) error {
	for i, pkgs := range shards {
		var w float64
		for _, p := range pkgs {
			if v, ok := table[p]; ok {
				w += v
			} else {
				w++
			}
		}
		if _, err := fmt.Fprintf(out, "# shard %d: %d packages, weight %.0f\n%s\n", i, len(pkgs), w, strings.Join(pkgs, " ")); err != nil {
			return err
		}
	}
	return nil
}
