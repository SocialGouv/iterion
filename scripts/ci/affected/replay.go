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
	"time"
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
	// Since and Until are the window git log was given, explicit midnights
	// in the local zone: a bare date would take the time of day of the run.
	Since string `json:"since"`
	Until string `json:"until,omitempty"`
	// Oldest and Newest are the first-parent commits that bound the window,
	// skipped ones included: the window can be replayed again by SHA.
	Oldest   string  `json:"oldest"`
	Newest   string  `json:"newest"`
	Commits  int     `json:"commits"`
	Skipped  int     `json:"skipped"`
	All      int     `json:"all"`
	AllShare float64 `json:"all_share"`
	replayShares
	// WithoutAll: the same shares over the commits no global trigger or
	// unclaimed file sent to everything — what the selection itself saves.
	WithoutAll replayShares   `json:"without_all"`
	LegCounts  map[string]int `json:"leg_counts"`
	// The graph and declarations are the CURRENT tree's, applied to past
	// diffs: an approximation, said rather than hidden.
	Note string `json:"note"`
}

type replayShares struct {
	Count          int     `json:"count"`
	PkgShareMedian float64 `json:"pkg_share_median"`
	PkgShareP90    float64 `json:"pkg_share_p90"`
	TimeShareMean  float64 `json:"time_share_mean"`
	TimeShareMed   float64 `json:"time_share_median"`
	TimeShareP90   float64 `json:"time_share_p90"`
}

// logEntry is one first-parent commit of the window, newest first.
type logEntry struct {
	SHA, Subject string
}

// replayDateLayout is the bare date -replay-since and -replay-until take.
const replayDateLayout = "2006-01-02"

// replayBound resolves a window bound: a bare date is that day's midnight
// in loc, an RFC 3339 timestamp is itself; anything else is refused rather
// than handed to git's approxidate, which would fill in the time of day.
func replayBound(s string, loc *time.Location) (string, error) {
	if t, err := time.ParseInLocation(replayDateLayout, s, loc); err == nil {
		return t.Format(time.RFC3339), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("replay bound %q: want a date (2006-01-02, that day's local midnight) or an RFC 3339 timestamp", s)
}

// runReplay runs every first-parent commit of o.replayRef in the date window
// through the selector, each against its first parent.
func runReplay(ctx context.Context, root string, g *graph, in *inputs, o options, stdout, stderr io.Writer) error {
	since, err := replayBound(o.replaySince, time.Local)
	if err != nil {
		return err
	}
	until := ""
	args := []string{"log", "--first-parent", "--format=%H%x09%s", "--since=" + since}
	if o.replayUntil != "" {
		if until, err = replayBound(o.replayUntil, time.Local); err != nil {
			return err
		}
		args = append(args, "--until="+until)
	}
	args = append(args, o.replayRef, "--")
	out, err := gitOut(ctx, root, args...)
	if err != nil {
		return err
	}
	var log []logEntry
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		sha, subject, _ := strings.Cut(line, "\t")
		log = append(log, logEntry{SHA: sha, Subject: subject})
	}
	weights, err := loadWeights(root, o.weights)
	if err != nil {
		return err
	}
	sum, commits, err := replay(g, in, weights, log, func(sha string) ([]change, error) {
		return diff(ctx, root, sha+"^1.."+sha)
	})
	if err != nil {
		return fmt.Errorf("%s since %s: %w", o.replayRef, since, err)
	}
	sum.Since, sum.Until = since, until

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Summary replaySummary  `json:"summary"`
		Commits []replayCommit `json:"commits"`
	}{sum, commits}); err != nil {
		return err
	}
	printReplay(sum, stderr)
	return nil
}

// replay runs each commit of log (newest first) through the selector, with
// diffOf giving its changes, and accounts for the selections.
func replay(g *graph, in *inputs, weights map[string]float64, log []logEntry, diffOf func(sha string) ([]change, error)) (replaySummary, []replayCommit, error) {
	sum := replaySummary{LegCounts: map[string]int{}, Note: "the current tree's import graph and declarations, applied to each past diff"}
	if len(log) == 0 {
		return sum, nil, fmt.Errorf("no commit to replay")
	}
	sum.Newest, sum.Oldest = log[0].SHA, log[len(log)-1].SHA
	total := 0.0
	for _, ip := range g.testPackages() {
		total += weightOf(weights, ip)
	}
	var commits []replayCommit
	for _, e := range log {
		if hasAnyPrefix(e.Subject, replaySkipPrefixes) {
			sum.Skipped++
			continue
		}
		changes, err := diffOf(e.SHA)
		if err != nil {
			return sum, nil, err
		}
		res := selectTests(g, in, changes)
		c := replayCommit{SHA: e.SHA, Subject: e.Subject, All: res.All, Packages: len(res.Packages), Legs: []string{}}
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
		return sum, nil, fmt.Errorf("every commit of the window is a skipped release or tap commit (%d)", sum.Skipped)
	}
	var without []replayCommit
	for _, c := range commits {
		if c.All {
			sum.All++
		} else {
			without = append(without, c)
		}
	}
	sum.Commits = len(commits)
	sum.AllShare = float64(sum.All) / float64(sum.Commits)
	sum.replayShares = sharesOf(commits)
	sum.WithoutAll = sharesOf(without)
	return sum, commits, nil
}

// sharesOf aggregates the package and time shares of commits; zero when
// there is none.
func sharesOf(commits []replayCommit) replayShares {
	s := replayShares{Count: len(commits)}
	if len(commits) == 0 {
		return s
	}
	var pkg, tm []float64
	for _, c := range commits {
		pkg = append(pkg, c.PkgShare)
		tm = append(tm, c.TimeShare)
		s.TimeShareMean += c.TimeShare
	}
	s.TimeShareMean /= float64(len(commits))
	s.PkgShareMedian, s.PkgShareP90 = percentile(pkg, 0.5), percentile(pkg, 0.9)
	s.TimeShareMed, s.TimeShareP90 = percentile(tm, 0.5), percentile(tm, 0.9)
	return s
}

func printReplay(sum replaySummary, w io.Writer) {
	window := "since " + sum.Since
	if sum.Until != "" {
		window += ", until " + sum.Until
	}
	fmt.Fprintf(w, "affected: replayed %d commits (%d release/tap commits skipped), %s: %s..%s\n", sum.Commits, sum.Skipped, window, sum.Oldest, sum.Newest)
	fmt.Fprintf(w, "  global trigger or unclaimed file (everything): %d (%.0f%%)\n", sum.All, 100*sum.AllShare)
	printShares(w, "all commits", sum.replayShares)
	printShares(w, "without a global trigger", sum.WithoutAll)
	for _, name := range sortedKeys(sum.LegCounts) {
		fmt.Fprintf(w, "  leg %s: %d of %d\n", name, sum.LegCounts[name], sum.Commits)
	}
}

func printShares(w io.Writer, label string, s replayShares) {
	if s.Count == 0 {
		fmt.Fprintf(w, "  %s: no commit\n", label)
		return
	}
	fmt.Fprintf(w, "  %s (%d): Go test packages selected median %.1f%%, p90 %.1f%%; unit-test time selected mean %.1f%%, median %.1f%%, p90 %.1f%%\n",
		label, s.Count, 100*s.PkgShareMedian, 100*s.PkgShareP90, 100*s.TimeShareMean, 100*s.TimeShareMed, 100*s.TimeShareP90)
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
