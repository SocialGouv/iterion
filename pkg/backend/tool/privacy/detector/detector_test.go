package detector

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scanCategories returns a fresh detector and the spans for text
// limited to the given categories at the lowest score floor.
func scanAll(t *testing.T, text string) []Span {
	t.Helper()
	d := New()
	return d.Scan(text, Options{MinScore: 0})
}

func scanCat(t *testing.T, text string, categories ...string) []Span {
	t.Helper()
	d := New()
	return d.Scan(text, Options{MinScore: 0, Categories: categories})
}

func hasRule(spans []Span, rule string) bool {
	for _, s := range spans {
		if s.Rule == rule {
			return true
		}
	}
	return false
}

func hasCategory(spans []Span, category string) bool {
	for _, s := range spans {
		if s.Category == category {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Email
// ---------------------------------------------------------------------------

func TestEmail_Basic(t *testing.T) {
	spans := scanCat(t, "Contact alice@example.com please", "email")
	if !hasCategory(spans, "email") {
		t.Fatalf("expected email span, got %+v", spans)
	}
}

func TestEmail_NotInUUID(t *testing.T) {
	spans := scanCat(t, "uuid: 12345678-1234-1234-1234-123456789abc", "email")
	if hasCategory(spans, "email") {
		t.Fatalf("UUID matched email rule: %+v", spans)
	}
}

// ---------------------------------------------------------------------------
// Phone
// ---------------------------------------------------------------------------

func TestPhone_E164(t *testing.T) {
	spans := scanCat(t, "Call +33612345678", "phone")
	if !hasRule(spans, "e164") {
		t.Fatalf("expected e164 span, got %+v", spans)
	}
}

func TestPhone_French(t *testing.T) {
	spans := scanCat(t, "Mon numéro: 06 12 34 56 78", "phone")
	if !hasCategory(spans, "phone") {
		t.Fatalf("expected phone span, got %+v", spans)
	}
}

// ---------------------------------------------------------------------------
// URL
// ---------------------------------------------------------------------------

func TestURL_Basic(t *testing.T) {
	spans := scanCat(t, "see https://example.com/path?q=1", "url")
	if !hasCategory(spans, "url") {
		t.Fatalf("expected url span, got %+v", spans)
	}
}

// ---------------------------------------------------------------------------
// Account number — IBAN, credit card, BBAN
// ---------------------------------------------------------------------------

func TestIBAN_Valid(t *testing.T) {
	spans := scanCat(t, "IBAN: FR1420041010050500013M02606", "account_number")
	if !hasRule(spans, "iban") {
		t.Fatalf("expected iban span, got %+v", spans)
	}
}

func TestIBAN_InvalidMod97(t *testing.T) {
	spans := scanCat(t, "IBAN: FR1420041010050500013M02607", "account_number")
	if hasRule(spans, "iban") {
		t.Fatalf("invalid IBAN should be rejected by mod-97, got %+v", spans)
	}
}

func TestCreditCard_Luhn(t *testing.T) {
	spans := scanCat(t, "Card: 4532015112830366", "account_number")
	if !hasRule(spans, "credit_card") {
		t.Fatalf("expected credit_card span, got %+v", spans)
	}
	spans2 := scanCat(t, "Card: 4532015112830367", "account_number")
	if hasRule(spans2, "credit_card") {
		t.Fatalf("invalid Luhn should be rejected, got %+v", spans2)
	}
}

// ---------------------------------------------------------------------------
// Secret
// ---------------------------------------------------------------------------

func TestSecret_AWSAccessKey(t *testing.T) {
	spans := scanCat(t, "key=AKIAIOSFODNN7EXAMPLE", "secret")
	if !hasRule(spans, "aws_access_key") {
		t.Fatalf("expected aws_access_key span, got %+v", spans)
	}
}

func TestSecret_GitHubPAT(t *testing.T) {
	spans := scanCat(t, "token: ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789", "secret")
	if !hasRule(spans, "github_pat") {
		t.Fatalf("expected github_pat span, got %+v", spans)
	}
}

func TestSecret_PEMKey(t *testing.T) {
	src := "-----BEGIN RSA PRIVATE KEY-----\nMIIB...\n-----END RSA PRIVATE KEY-----"
	spans := scanCat(t, src, "secret")
	if !hasRule(spans, "pem_private_key") {
		t.Fatalf("expected pem_private_key span, got %+v", spans)
	}
}

func TestSecret_JWT(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	spans := scanCat(t, jwt, "secret")
	if !hasRule(spans, "jwt") {
		t.Fatalf("expected jwt span, got %+v", spans)
	}
}

func TestSecret_HighEntropyPositive(t *testing.T) {
	// password = "<random>" — bare entropy ≥ 3.5, no identifier shape.
	src := `password = "Xj9nQ8vL5pK2mZ7tR4wH6sB1aD3fG0iC"`
	spans := scanCat(t, src, "secret")
	if !hasCategory(spans, "secret") {
		t.Fatalf("expected secret span for high-entropy password, got %+v", spans)
	}
}

func TestSecret_HighEntropyNegative(t *testing.T) {
	src := `password = "changeme"`
	spans := scanCat(t, src, "secret")
	for _, s := range spans {
		if s.Rule == "password_assignment_high_entropy" || s.Rule == "generic_high_entropy_string" {
			t.Fatalf("low-entropy password incorrectly flagged: %+v", s)
		}
	}
}

// ---------------------------------------------------------------------------
// Merge / span structure
// ---------------------------------------------------------------------------

func TestMerge_OverlappingSpans(t *testing.T) {
	in := []Span{
		{Category: "secret", Rule: "a", Score: 0.6, Start: 0, End: 40},
		{Category: "secret", Rule: "b", Score: 0.9, Start: 5, End: 35},
		{Category: "secret", Rule: "c", Score: 0.7, Start: 50, End: 60},
	}
	out := mergeOverlapping(in)
	if len(out) != 2 {
		t.Fatalf("merge: expected 2 spans, got %d (%+v)", len(out), out)
	}
	// First kept span should be the higher-scoring one in the
	// 0..40 region. Because mergeOverlapping picks the higher score
	// among overlapping spans, expect rule=b.
	if out[0].Rule != "b" {
		t.Fatalf("merge: expected rule b on first span, got %+v", out[0])
	}
	if out[1].Rule != "c" {
		t.Fatalf("merge: expected rule c on second span, got %+v", out[1])
	}
}

func TestScan_RuneIndices(t *testing.T) {
	// Multi-byte prefix then email; rune indices must locate the
	// email correctly.
	prefix := "😀hello "
	email := "alice@example.com"
	src := prefix + email + " bye"
	spans := scanCat(t, src, "email")
	if len(spans) == 0 {
		t.Fatalf("expected email span")
	}
	got := []rune(src)[spans[0].Start:spans[0].End]
	if string(got) != email {
		t.Fatalf("rune slice mismatch: got %q, want %q", string(got), email)
	}
}

func TestScan_BlankInput(t *testing.T) {
	d := New()
	if got := d.Scan("", Options{}); got != nil {
		t.Fatalf("empty input should return nil, got %+v", got)
	}
	if got := d.Scan("   \t\n  ", Options{}); got != nil {
		t.Fatalf("whitespace-only input should return nil, got %+v", got)
	}
}

func TestScan_NoBacktracking(t *testing.T) {
	// 10 KB adversarial input. RE2 guarantees linear time so this
	// must terminate well under the deadline; we use 5 s as a very
	// generous bound to keep CI machines happy.
	src := strings.Repeat("aaaaa(bbbbb)*", 800)
	d := New()
	deadline := time.Now().Add(5 * time.Second)
	d.Scan(src, Options{})
	if time.Now().After(deadline) {
		t.Fatalf("scan exceeded deadline on adversarial input")
	}
}

// ---------------------------------------------------------------------------
// Corpus tests
// ---------------------------------------------------------------------------

func TestRules_PositiveCorpus(t *testing.T) {
	d := New()
	lines := readNonEmptyLines(t, "testdata/secrets_positive.txt")
	for i, ln := range lines {
		spans := d.Scan(ln, Options{})
		if len(spans) == 0 {
			t.Errorf("line %d: %q produced no detections", i+1, ln)
		}
	}
}

func TestRules_NegativeCorpus(t *testing.T) {
	d := New()
	lines := readNonEmptyLines(t, "testdata/secrets_negative.txt")
	for i, ln := range lines {
		spans := d.Scan(ln, Options{})
		// Negative corpus: expect zero spans. Any detection is a FP.
		if len(spans) > 0 {
			t.Errorf("line %d (%q) produced false-positive spans: %+v", i+1, ln, spans)
		}
	}
}

func TestRules_NoFP_OnExamples(t *testing.T) {
	// The examples directory ships fixtures that are read by
	// downstream tests. Any detection here would surface as a
	// real-data leak in the corpus → fail loudly so the maintainer
	// either scrubs the example or extends the negative corpus.
	d := New()
	matches, err := filepath.Glob(filepath.Join("..", "..", "..", "..", "..", "examples", "*.iter"))
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	if len(matches) == 0 {
		t.Skip("examples directory not reachable from test working dir; skipping")
	}
	// Examples are tutorial workflows: they routinely contain
	// URLs (links to docs / gitleaks repo), placeholder phone-shaped
	// strings, and demo emails (`*@iterion.local`). The test scope
	// is *secret* and *account_number* leakage — anything that
	// could cause a real-world credential exfiltration. Allow the
	// content-shape rules.
	allowedCategories := map[string]bool{
		"url":   true,
		"email": true,
		"phone": true,
	}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		spans := d.Scan(string(data), Options{})
		for _, s := range spans {
			if allowedCategories[s.Category] {
				continue
			}
			snippet := []rune(string(data))
			start := s.Start
			end := s.End
			if start < 0 {
				start = 0
			}
			if end > len(snippet) {
				end = len(snippet)
			}
			t.Errorf("%s: rule %s matched on %q (start=%d end=%d)", filepath.Base(path), s.Rule, string(snippet[start:end]), s.Start, s.End)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func readNonEmptyLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" {
			continue
		}
		out = append(out, ln)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}
