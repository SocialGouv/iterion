package detector

import "math"

// shannonEntropy returns the bits-per-rune of s using base-2 log.
//
// Calibration:
//   - "password"           ≈ 2.5
//   - random base64 (≥32B) ≥ 5.5
//   - typical UUID         ≥ 4.0
//
// The function operates on runes (not bytes) so non-ASCII inputs see
// a stable result that does not penalise multi-byte encodings.
//
// Entropy is used as a secondary filter on rules that combine a regex
// candidate with a quality threshold (e.g. bearer_token_high_entropy).
// The regex isolates a structural shape; the entropy filter rejects
// dictionary-strength values like `password = "changeme"` whose
// structure would otherwise match.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := make(map[rune]int, len(s))
	total := 0
	for _, r := range s {
		counts[r]++
		total++
	}
	if total == 0 {
		return 0
	}
	n := float64(total)
	var h float64
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
