package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// replaySkipPrefixes are the commit subjects the replay leaves out: release
// and tap bumps touch no code and skip the heavy CI by design.
var replaySkipPrefixes = []string{"chore: release", "chore(brew)"}

type replayCommit struct {
	SHA       string   `json:"sha"`
	Subject   string   `json:"subject"`
	All       bool     `json:"all"`
	Reason    string   `json:"reason,omitempty"`
	Packages  int      `json:"packages"`
	PkgShare  float64  `json:"pkg_share"`
	TimeShare float64  `json:"time_share"`
	Legs      []string `json:"legs"`
	Selected  []string `json:"selected"`
}

type replaySummary struct {
	Commits        int            `json:"commits"`
	Skipped        int            `json:"skipped"`
	All            int            `json:"all"`
	AllShare       float64        `json:"all_share"`
	PkgShareMedian float64        `json:"pkg_share_median"`
	PkgShareP90    float64        `json:"pkg_share_p90"`
	TimeShareMean  float64        `json:"time_share_mean"`
	TimeShareMed   float64        `json:"time_share_median"`
	TimeShareP90   float64        `json:"time_share_p90"`
	LegCounts      map[string]int `json:"leg_counts"`
	// The graph and declarations are the CURRENT tree's, applied to past
	// diffs: an approximation, said rather than hidden.
	Note string `json:"note"`
}

// runReplay runs every first-parent commit of o.replayRef in the date window
// through the selector, each against its first parent.
func runReplay(ctx context.Context, root string, g *graph, in *inputs, o options, w io.Writer) error {
	args := []string{"log", "--first-parent", "--format=%H%x09%s", "--since=" + o.replaySince}
	if o.replayUntil != "" {
		args = append(args, "--until="+o.replayUntil)
	}
	args = append(args, o.replayRef, "--")
	out, err := gitOut(ctx, root, args...)
	if err != nil {
		return err
	}
	weights, err := loadWeights(root, o.weights)
	if err != nil {
		return err
	}
	total := 0.0
	for _, ip := range g.testPackages() {
		total += weightOf(weights, ip)
	}
	var commits []replayCommit
	sum := replaySummary{LegCounts: map[string]int{}, Note: "the current tree's import graph and declarations, applied to each past diff"}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		sha, subject, _ := strings.Cut(line, "\t")
		if hasAnyPrefix(subject, replaySkipPrefixes) {
			sum.Skipped++
			continue
		}
		changes, err := diff(ctx, root, sha+"^1.."+sha)
		if err != nil {
			return err
		}
		res := selectTests(g, in, changes)
		c := replayCommit{SHA: sha, Subject: subject, All: res.All, Packages: len(res.Packages), Legs: []string{}}
		if res.All {
			c.Reason = res.Reasons[0]
		}
		sel := 0.0
		for _, p := range res.Packages {
			sel += weightOf(weights, p.ImportPath)
			if !res.All {
				c.Selected = append(c.Selected, p.Dir)
			}
		}
		if res.TestPackages > 0 {
			c.PkgShare = float64(len(res.Packages)) / float64(res.TestPackages)
		}
		if total > 0 {
			c.TimeShare = sel / total
		}
		for _, l := range res.Legs {
			c.Legs = append(c.Legs, l.Name)
			sum.LegCounts[l.Name]++
		}
		commits = append(commits, c)
	}
	if len(commits) == 0 {
		return fmt.Errorf("no commit of %s since %s to replay", o.replayRef, o.replaySince)
	}
	var pkgShares, timeShares []float64
	for _, c := range commits {
		if c.All {
			sum.All++
		}
		pkgShares = append(pkgShares, c.PkgShare)
		timeShares = append(timeShares, c.TimeShare)
		sum.TimeShareMean += c.TimeShare
	}
	sum.Commits = len(commits)
	sum.AllShare = float64(sum.All) / float64(sum.Commits)
	sum.TimeShareMean /= float64(sum.Commits)
	sum.PkgShareMedian, sum.PkgShareP90 = percentile(pkgShares, 0.5), percentile(pkgShares, 0.9)
	sum.TimeShareMed, sum.TimeShareP90 = percentile(timeShares, 0.5), percentile(timeShares, 0.9)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Summary replaySummary  `json:"summary"`
		Commits []replayCommit `json:"commits"`
	}{sum, commits}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "affected: replayed %d commits (%d release/tap commits skipped)\n", sum.Commits, sum.Skipped)
	fmt.Fprintf(os.Stderr, "  global trigger or unclaimed file (everything): %d (%.0f%%)\n", sum.All, 100*sum.AllShare)
	fmt.Fprintf(os.Stderr, "  Go test packages selected: median %.0f%%, p90 %.0f%%\n", 100*sum.PkgShareMedian, 100*sum.PkgShareP90)
	fmt.Fprintf(os.Stderr, "  unit-test time selected: mean %.0f%%, median %.0f%%, p90 %.0f%%\n", 100*sum.TimeShareMean, 100*sum.TimeShareMed, 100*sum.TimeShareP90)
	for _, name := range sortedKeys(sum.LegCounts) {
		fmt.Fprintf(os.Stderr, "  leg %s: %d of %d\n", name, sum.LegCounts[name], sum.Commits)
	}
	return nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// percentile is the nearest-rank percentile of xs.
func percentile(xs []float64, q float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	return s[max(i, 0)]
}

// loadWeights reads an import-path TAB seconds table (# comments); an empty
// path weighs every package 1.
func loadWeights(root, file string) (map[string]float64, error) {
	weights := map[string]float64{}
	if file == "" {
		return weights, nil
	}
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want import-path TAB seconds", file, n)
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("%s:%d: bad seconds %q", file, n, fields[1])
		}
		weights[fields[0]] = v
	}
	return weights, sc.Err()
}

// weightOf is a package's weight; a package the table omits weighs 1, the
// rule scripts/ci/testshards applies to the same table.
func weightOf(weights map[string]float64, ip string) float64 {
	if v, ok := weights[ip]; ok {
		return v
	}
	return 1
}
