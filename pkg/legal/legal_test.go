package legal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_EmptyDirStringPublishesNothing(t *testing.T) {
	docs, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("Load(\"\") = %d documents, want none", len(docs))
	}
}

func TestLoad_MissingDirIsAnError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent"))
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("Load(missing) = %v, want an error naming the dir", err)
	}
}

func TestLoad_ReadsOneFilePerSlugInSlugOrder(t *testing.T) {
	dir := t.TempDir()
	// Written out of order on purpose: the surface is ordered by Slugs, not
	// by the directory listing.
	write(t, dir, "accessibilite.md", "# Déclaration\n\nnon conforme")
	write(t, dir, "mentions-legales.md", "# Mentions\n\néditeur")
	// A ConfigMap mount's own entries: a timestamped dir and the ..data link.
	if err := os.Mkdir(filepath.Join(dir, "..2026_11_03_10_00_00.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_11_03_10_00_00.1", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	docs, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("Load = %d documents, want 2: %+v", len(docs), docs)
	}
	if docs[0].Slug != SlugLegalNotice || docs[1].Slug != SlugAccessibility {
		t.Errorf("order = %s, %s; want mentions-legales then accessibilite", docs[0].Slug, docs[1].Slug)
	}
	if docs[0].Title != "Mentions légales" {
		t.Errorf("Title = %q", docs[0].Title)
	}
	if docs[0].Markdown != "# Mentions\n\néditeur" {
		t.Errorf("Markdown = %q", docs[0].Markdown)
	}
	if docs[0].ETag == "" || docs[0].ETag == docs[1].ETag || !strings.HasPrefix(docs[0].ETag, `"`) {
		t.Errorf("ETags = %q / %q, want two distinct quoted validators", docs[0].ETag, docs[1].ETag)
	}
}

func TestLoad_RefusesAFileThatNamesNoDocument(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "cgu.md", "# CGU")
	write(t, dir, "mentions_legales.md", "# typo in the slug")
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a stray file; want a refusal naming it")
	}
	if !strings.Contains(err.Error(), "mentions_legales.md") || !strings.Contains(err.Error(), "mentions-legales.md") {
		t.Errorf("error = %v, want the stray name and the accepted names", err)
	}
}

func TestLoad_RefusesAnEmptyDocument(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "cgu.md", "  \n")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "cgu.md is empty") {
		t.Fatalf("Load(empty cgu.md) = %v, want a refusal naming the empty file", err)
	}
}

func TestSurfaceDocument(t *testing.T) {
	s := Surface{Documents: []Document{newDocument(SlugTerms, "x")}}
	if _, ok := s.Document(SlugTerms); !ok {
		t.Error("configured slug not found")
	}
	if _, ok := s.Document(SlugPrivacyPolicy); ok {
		t.Error("unconfigured slug found")
	}
	if _, ok := s.Document("nope"); ok {
		t.Error("unknown slug found")
	}
}

func TestParseStatus(t *testing.T) {
	for _, raw := range []string{"", "non_compliant", "Partially_Compliant", " compliant "} {
		if _, err := ParseStatus(raw); err != nil {
			t.Errorf("ParseStatus(%q) = %v", raw, err)
		}
	}
	if _, err := ParseStatus("conforme"); err == nil || !strings.Contains(err.Error(), "non_compliant|partially_compliant|compliant") {
		t.Errorf("ParseStatus(conforme) = %v, want a refusal listing the statuses", err)
	}
}

func TestAccessibilityValidate(t *testing.T) {
	cases := []struct {
		name string
		a    Accessibility
		want string // "" = valid, else a substring of the error
	}{
		{"zero", Accessibility{}, ""},
		{"status only", Accessibility{Status: StatusNonCompliant}, ""},
		{"full", Accessibility{Status: StatusPartiallyCompliant, Date: "2026-11-03", Rate: "72.5"}, ""},
		{"comma rate", Accessibility{Status: StatusPartiallyCompliant, Rate: "72,5"}, ""},
		{"partially at 50", Accessibility{Status: StatusPartiallyCompliant, Rate: "50"}, ""},
		{"compliant at 100", Accessibility{Status: StatusCompliant, Rate: "100"}, ""},
		{"compliant without rate", Accessibility{Status: StatusCompliant}, ""},
		{"non compliant with rate", Accessibility{Status: StatusNonCompliant, Rate: "49.9"}, ""},
		{"compliant below 100", Accessibility{Status: StatusCompliant, Rate: "42"}, "contradicts status compliant"},
		{"partially at 100", Accessibility{Status: StatusPartiallyCompliant, Rate: "100"}, "contradicts status partially_compliant"},
		{"partially below 50", Accessibility{Status: StatusPartiallyCompliant, Rate: "42"}, "contradicts status partially_compliant"},
		{"non compliant at 50", Accessibility{Status: StatusNonCompliant, Rate: "50"}, "contradicts status non_compliant"},
		{"partially without rate", Accessibility{Status: StatusPartiallyCompliant}, "needs its conformance rate"},
		{"unknown status", Accessibility{Status: "conforme"}, "accessibility status"},
		{"date without status", Accessibility{Date: "2026-11-03"}, "without an accessibility status"},
		{"bad date", Accessibility{Status: StatusCompliant, Date: "03/11/2026"}, "want YYYY-MM-DD"},
		{"rate above 100", Accessibility{Status: StatusCompliant, Rate: "101"}, "between 0 and 100"},
		{"rate not a number", Accessibility{Status: StatusCompliant, Rate: "42 %"}, "between 0 and 100"},
		{"rate NaN", Accessibility{Status: StatusCompliant, Rate: "NaN"}, "between 0 and 100"},
		{"rate negative", Accessibility{Status: StatusCompliant, Rate: "-1"}, "between 0 and 100"},
	}
	for _, c := range cases {
		err := c.a.Validate()
		if c.want == "" && err != nil {
			t.Errorf("%s: Validate() = %v, want nil", c.name, err)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: Validate() = %v, want %q", c.name, err, c.want)
		}
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
