package tracker

import (
	"slices"
	"sort"
)

// Helpers shared by adapters whose state model is "labels on the
// underlying tracker map to a workflow state name". GitHub and
// Forgejo both fit that mold; the native tracker does not.

// labelsMatch reports whether the haystack satisfies the LabelSelector
// (all includes present, no excludes present).
func labelsMatch(have []string, sel LabelSelector) bool {
	for _, want := range sel.LabelsInclude {
		if !slices.Contains(have, want) {
			return false
		}
	}
	for _, no := range sel.LabelsExclude {
		if slices.Contains(have, no) {
			return false
		}
	}
	return true
}

// resolveStateByLabels picks the first state (in sorted order, to
// compensate for Go map iteration randomness) whose selector matches
// the given labels.
func resolveStateByLabels(labels []string, mapping map[string]LabelSelector) string {
	names := make([]string, 0, len(mapping))
	for k := range mapping {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		if labelsMatch(labels, mapping[name]) {
			return name
		}
	}
	return ""
}

func anyOfString(haystack, needles []string) bool {
	for _, n := range needles {
		if slices.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func filterOutString(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// applyLabelDiff returns have with sel's excludes removed and includes
// added (each include only if not already present). Used by UpdateState
// on adapters whose only state-transition mechanism is a full label
// rewrite (gitlab, forgejo) — read the current labels, apply the diff,
// write the full set back.
func applyLabelDiff(have []string, sel LabelSelector) []string {
	for _, l := range sel.LabelsExclude {
		have = filterOutString(have, l)
	}
	for _, l := range sel.LabelsInclude {
		if !slices.Contains(have, l) {
			have = append(have, l)
		}
	}
	return have
}
