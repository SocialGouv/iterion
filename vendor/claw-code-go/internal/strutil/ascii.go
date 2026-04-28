// Package strutil provides small string helpers shared across packages.
package strutil

// ASCIIToLower lowercases only ASCII A-Z bytes, preserving non-ASCII unchanged.
// This matches Rust's str::to_ascii_lowercase().
func ASCIIToLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
