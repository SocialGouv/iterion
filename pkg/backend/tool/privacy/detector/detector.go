package detector

import (
	"sort"
	"strings"
	"unicode"
)

// Detector scans text for PII spans using the curated regex +
// heuristics rule set. It is stateless after construction and safe
// for concurrent use across goroutines — Scan does not mutate any
// shared state.
type Detector struct {
	rules []Rule
}

// New builds a Detector with the v1 rule set:
//   - email (1 rule)
//   - phone (2 rules)
//   - url (1 rule)
//   - account_number (3 rules — IBAN, credit card, French BBAN)
//   - secret (~22 rules — gitleaks-derived + entropy heuristics)
func New() *Detector {
	return &Detector{rules: registerBuiltinRules()}
}

// NewWithRules constructs a Detector with a custom rule list. Used
// by tests that need to isolate behaviour to a single rule.
func NewWithRules(rules []Rule) *Detector {
	return &Detector{rules: rules}
}

// Scan returns the merged set of PII spans for text. An empty or
// whitespace-only input short-circuits and returns nil — callers
// can use this as a fast no-op signal.
//
// Spans are ordered by ascending Start and never overlap.
func (d *Detector) Scan(text string, opts Options) []Span {
	if IsBlank(text) {
		return nil
	}
	byteToRune := buildByteToRune(text)

	out := make([]Span, 0, 8)
	for _, r := range d.rules {
		if !allowedCategories(r.Category(), opts.Categories) {
			continue
		}
		spans := r.Find(text, byteToRune)
		for _, s := range spans {
			if s.Score < opts.MinScore {
				continue
			}
			out = append(out, s)
		}
	}
	return mergeOverlapping(out)
}

// mergeOverlapping de-duplicates overlapping spans, keeping the one
// with the highest score. When scores are equal, the earlier rule
// in the registration order wins (stable sort preserves arrival
// order for equal keys, so we exploit that).
func mergeOverlapping(spans []Span) []Span {
	if len(spans) <= 1 {
		return spans
	}
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].Start != spans[j].Start {
			return spans[i].Start < spans[j].Start
		}
		if spans[i].Score != spans[j].Score {
			return spans[i].Score > spans[j].Score
		}
		// Longer span wins for the same start+score (so a span
		// covering a substring is dropped in favour of its parent).
		return spans[i].End > spans[j].End
	})

	out := make([]Span, 0, len(spans))
	out = append(out, spans[0])
	for i := 1; i < len(spans); i++ {
		s := spans[i]
		last := &out[len(out)-1]
		if s.Start < last.End {
			// Overlap. Keep the higher-score one. Because we sorted
			// by Start asc then Score desc, `last` is already the
			// preferred span for this region — drop s.
			if s.Score > last.Score {
				*last = s
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// IsBlank returns true if s contains no non-whitespace runes.
// Exported so the privacy package can short-circuit blank inputs
// using the same Unicode-aware definition the detector uses.
func IsBlank(s string) bool {
	if s == "" {
		return true
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return !unicode.IsSpace(r)
	}) == -1
}
