package parser

import "testing"

// TestEveryKeywordNamesItself holds the one of the three keyword tables that
// no test read: `tokenNames`. `TokenType.String()` falls back to
// `fmt.Sprintf("TokenType(%d)", int(t))`, and that fallback lands in a
// user-facing parse error — `expected :, got TokenType(76)` instead of
// `expected :, got auth`, which is what `auth` printed until this test was
// written (measured; `"auth": TokenAuth` was in the keyword map with no name
// entry). A chokepoint rather than a line per keyword: `Keywords()` is
// derived from the keyword map itself, so the census needs no list of its own
// and the next contributor who forgets an entry gets a red here.
func TestEveryKeywordNamesItself(t *testing.T) {
	kws := Keywords()
	if len(kws) < 100 {
		t.Fatalf("Keywords() returned %d entries: this guard is reading the wrong thing", len(kws))
	}
	for _, kw := range kws {
		tok, ok := keywords[kw]
		if !ok {
			t.Errorf("Keywords() offers %q, which the keyword map does not hold", kw)
			continue
		}
		if got := tok.String(); got != kw {
			t.Errorf("keyword %q renders as %q in a parse error: add it to tokenNames", kw, got)
		}
	}
}
