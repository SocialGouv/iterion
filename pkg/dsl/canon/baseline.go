package canon

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A baseline is a tree's committed list of the files its canonical form
// refuses — today, `.fmt-refused` at the root of this repository: the bots
// whose `|` block scalars the writer has no form for (#1612).
//
// It exists so a check can be GREEN on a tree that is not yet wholly
// canonical, and red on the three things that are worth a verdict: a
// formattable file that drifted out of its canonical form, a file that
// newly became unformattable, and a file the baseline still names although
// nothing refuses it any more. That last one is the ratchet: when the
// writer gains the form it lacks, the baseline has to shrink or the check
// reddens — a backlog that cannot be left to rot quietly.
//
// A file renamed or deleted takes the same path: its name no longer appears
// among the refusals, so the baseline names one nothing refuses, and the
// check says exactly that.

// ReadBaseline reads a baseline file: one path per line, `#` comments and
// blank lines ignored, each path normalised to slashes. A missing file is
// an error — a check whose baseline vanished must not read as "nothing is
// refused".
func ReadBaseline(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("baseline: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, NormalizeBaselinePath(line))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("baseline %s: %w", path, err)
	}
	sort.Strings(out)
	return out, nil
}

// NormalizeBaselinePath is the one spelling a path is compared under, so a
// baseline written by hand and a walk's output agree: cleaned, slashed, and
// relative to the working directory when it can be — a check invoked with
// absolute paths otherwise reports every file as both newly refused and no
// longer refused, which is red for the right reason and unreadable.
func NormalizeBaselinePath(p string) string {
	p = filepath.Clean(p)
	if filepath.IsAbs(p) {
		if cwd, err := os.Getwd(); err == nil {
			if rel, err := filepath.Rel(cwd, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				p = rel
			}
		}
	}
	return filepath.ToSlash(p)
}

// DiffBaseline reports how an observed set of refusals differs from the
// baseline: the files newly refused (the tree got worse, or a file arrived
// that cannot be formatted), and the files the baseline names that nothing
// refuses any more (the tree got better, and the baseline owes an update).
// Both are sorted; both empty means the two agree.
func DiffBaseline(baseline, refused []string) (newlyRefused, noLongerRefused []string) {
	in := func(list []string) map[string]bool {
		m := make(map[string]bool, len(list))
		for _, p := range list {
			m[NormalizeBaselinePath(p)] = true
		}
		return m
	}
	known, seen := in(baseline), in(refused)
	for p := range seen {
		if !known[p] {
			newlyRefused = append(newlyRefused, p)
		}
	}
	for p := range known {
		if !seen[p] {
			noLongerRefused = append(noLongerRefused, p)
		}
	}
	sort.Strings(newlyRefused)
	sort.Strings(noLongerRefused)
	return newlyRefused, noLongerRefused
}
