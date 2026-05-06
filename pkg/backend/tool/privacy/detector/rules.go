// Package detector implements the pure-Go PII detection backend used
// by the privacy_filter / privacy_unfilter built-in tools.
//
// The detector is stateless after New() — its compiled regex set is
// constant and safe to call from multiple goroutines concurrently.
// Spans are emitted using rune (character) offsets so callers can
// safely splice the source text without worrying about multi-byte
// boundaries.
package detector

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Span describes a single detected PII occurrence.
//
// Start/End are RUNE offsets, not byte offsets — callers can convert
// the source via []rune(text) and slice with the same indices. This
// matters because regexp returns byte offsets that are unsafe to use
// on strings containing non-ASCII characters such as emoji.
type Span struct {
	Category string  `json:"category"`
	Score    float64 `json:"score"`
	Start    int     `json:"start"`
	End      int     `json:"end"`
	Rule     string  `json:"rule"`
}

// Options configures a single Scan invocation.
//
// Categories filters the rule set by category; an empty/nil slice
// matches every rule. MinScore drops low-confidence matches.
type Options struct {
	Categories []string
	MinScore   float64
}

// Rule is the interface every detection rule satisfies.
//
// Find receives the source text plus a precomputed byte→rune index
// table so emitted Span offsets are rune-indexed regardless of the
// matcher's internal representation. The detector builds the table
// once per Scan and reuses it across rules.
type Rule interface {
	Name() string
	Category() string
	Find(text string, byteToRune []int) []Span
}

// regexRule matches a compiled regular expression against the input
// and (optionally) post-filters the matched substring with a
// validation closure (Luhn, mod-97, entropy, ...).
//
// matchGroup selects which capture group to emit:
//   - 0 → the whole match (default)
//   - >0 → the indexed sub-group; the rule still applies postFilter
//     to the same group's substring
//
// score is constant per rule. Rules that need a dynamic score (e.g.
// a quality bonus when entropy is very high) should embed their
// logic in postFilter and return a per-match score via a sibling
// `scoreFn` variant — none of the v1 rules need that today.
type regexRule struct {
	name       string
	category   string
	score      float64
	re         *regexp.Regexp
	matchGroup int
	postFilter func(match string) bool
}

func (r *regexRule) Name() string     { return r.name }
func (r *regexRule) Category() string { return r.category }

func (r *regexRule) Find(text string, byteToRune []int) []Span {
	if text == "" {
		return nil
	}
	matches := r.re.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]Span, 0, len(matches))
	for _, m := range matches {
		startByte, endByte := m[0], m[1]
		if r.matchGroup > 0 {
			gIdx := r.matchGroup * 2
			if gIdx+1 >= len(m) || m[gIdx] < 0 {
				continue
			}
			startByte, endByte = m[gIdx], m[gIdx+1]
		}
		raw := text[startByte:endByte]
		if r.postFilter != nil && !r.postFilter(raw) {
			continue
		}
		startRune := byteToRuneIndex(byteToRune, startByte)
		endRune := byteToRuneIndex(byteToRune, endByte)
		out = append(out, Span{
			Category: r.category,
			Score:    r.score,
			Start:    startRune,
			End:      endRune,
			Rule:     r.name,
		})
	}
	return out
}

// allowedCategories reports whether category passes the requested
// filter list. An empty filter matches anything.
func allowedCategories(category string, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, c := range want {
		if c == category {
			return true
		}
	}
	return false
}

// buildByteToRune returns a slice such that byteToRune[i] is the
// rune index of the run that starts at byte offset i. For offsets
// that fall in the middle of a multi-byte rune, the previous rune
// index is repeated. The terminal entry (index = len(text)) maps to
// the rune count, so [start, end) byte ranges map cleanly to
// [byteToRune[start], byteToRune[end]) rune ranges.
func buildByteToRune(text string) []int {
	out := make([]int, len(text)+1)
	runeIdx := 0
	for i := range text {
		// `i` is the byte offset of the next rune.
		out[i] = runeIdx
		runeIdx++
	}
	for i := range out {
		if i == 0 {
			continue
		}
		if out[i] == 0 && out[i-1] > 0 {
			out[i] = out[i-1]
		}
	}
	out[len(text)] = runeIdx
	return out
}

// byteToRuneIndex returns the rune index for a byte offset using a
// table built by buildByteToRune. Out-of-range inputs are clamped.
func byteToRuneIndex(table []int, byteOff int) int {
	if byteOff < 0 {
		return 0
	}
	if byteOff >= len(table) {
		return table[len(table)-1]
	}
	return table[byteOff]
}

// emailRules returns the email-category rule set.
func emailRules() []Rule {
	return []Rule{
		&regexRule{
			name:     "rfc5322_simple",
			category: "email",
			score:    1.0,
			re:       regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`),
			postFilter: func(match string) bool {
				// Reject UUID-like prefixes that happen to land before
				// an @ — the regex above already needs an @ + domain,
				// so this is a defence in depth (no-op for v1).
				return !strings.HasPrefix(match, "-")
			},
		},
	}
}

// phoneRules returns the phone-category rule set.
func phoneRules() []Rule {
	return []Rule{
		&regexRule{
			name:     "e164",
			category: "phone",
			score:    1.0,
			re:       regexp.MustCompile(`\+[1-9]\d{6,14}\b`),
		},
		&regexRule{
			name:     "phone_french",
			category: "phone",
			score:    0.9,
			re:       regexp.MustCompile(`\b0[1-9](?:[\s.\-]?\d{2}){4}\b`),
		},
	}
}

// urlRules returns the url-category rule set.
func urlRules() []Rule {
	return []Rule{
		&regexRule{
			name:     "url_basic",
			category: "url",
			score:    1.0,
			re:       regexp.MustCompile(`\bhttps?://[^\s<>"']+`),
		},
	}
}

// accountRules returns the account_number-category rule set.
// Both IBAN and credit_card require structural validation on top of
// the regex shape so we filter false positives at the postFilter
// stage rather than relying on the regex alone.
func accountRules() []Rule {
	return []Rule{
		&regexRule{
			name:       "iban",
			category:   "account_number",
			score:      1.0,
			re:         regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{4}\d{7}[A-Z0-9]{0,16}\b`),
			postFilter: validateIBANMod97,
		},
		&regexRule{
			name:     "credit_card",
			category: "account_number",
			score:    1.0,
			// Restrict to first-digit ranges of the major networks
			// (Visa 4xxx, Mastercard 51-55, Amex 34/37, Discover
			// 6011/65xx). The leading-digit constraint avoids
			// matching UUID groups and other random 16-digit blocks
			// that happen to satisfy Luhn.
			re:         regexp.MustCompile(`\b(?:4\d{3}|5[1-5]\d{2}|3[47]\d{2}|6(?:011|5\d{2}))[\s\-]?\d{4}[\s\-]?\d{4}[\s\-]?\d{4}\b`),
			postFilter: validateLuhn,
		},
		&regexRule{
			name:     "bban_french",
			category: "account_number",
			score:    0.7,
			re:       regexp.MustCompile(`\b\d{5}\d{5}\d{11}\d{2}\b`),
		},
	}
}

// registerBuiltinRules assembles the full v1 rule set across all
// five categories. Order matters only when two rules report
// overlapping spans with equal score: the first one declared wins
// after mergeOverlapping.
func registerBuiltinRules() []Rule {
	rules := make([]Rule, 0, 32)
	rules = append(rules, emailRules()...)
	rules = append(rules, phoneRules()...)
	rules = append(rules, urlRules()...)
	rules = append(rules, accountRules()...)
	rules = append(rules, secretRules()...)
	return rules
}

// validateLuhn returns true if the digit string passes the Luhn
// check. Spaces and hyphens are stripped before validation.
//
// Additionally rejects all-same-digit strings (e.g. "0000000000000000")
// which formally pass Luhn (sum = 0) but are obviously not real card
// numbers; they would otherwise turn every UUID-shape string into a
// false positive.
func validateLuhn(s string) bool {
	digits := stripNonDigits(s)
	if len(digits) < 12 {
		return false
	}
	if isAllSameDigit(digits) {
		return false
	}
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}

func isAllSameDigit(s string) bool {
	if s == "" {
		return false
	}
	first := s[0]
	for i := 1; i < len(s); i++ {
		if s[i] != first {
			return false
		}
	}
	return true
}

// validateIBANMod97 returns true if the IBAN passes the
// classical mod-97 == 1 check. Whitespace is stripped before
// validation.
func validateIBANMod97(s string) bool {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	if len(cleaned) < 15 || len(cleaned) > 34 {
		return false
	}
	// Move the first 4 characters to the end.
	rotated := cleaned[4:] + cleaned[:4]
	// Convert letters to numbers (A=10, B=11, ..., Z=35).
	var b strings.Builder
	for _, r := range rotated {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteString(strconv.Itoa(int(r - 'A' + 10)))
		case r >= 'a' && r <= 'z':
			b.WriteString(strconv.Itoa(int(r - 'a' + 10)))
		default:
			return false
		}
	}
	// Reduce mod 97 in chunks to avoid big integer arithmetic.
	digits := b.String()
	rem := 0
	for _, ch := range digits {
		rem = (rem*10 + int(ch-'0')) % 97
	}
	return rem == 1
}

func stripNonDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
