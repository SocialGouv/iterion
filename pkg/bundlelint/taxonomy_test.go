package bundlelint

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestCheckBotTaxonomy_WarnsOnUnknownCategoryAndTags(t *testing.T) {
	m := &bundle.Manifest{
		Category: "pilot",
		Tags:     []string{"security", "brand-new-facet"},
	}
	diags := CheckConsistency(Input{Manifest: m})

	var cat, tag *Diag
	for i := range diags {
		switch diags[i].Code {
		case DiagBotCategoryUnknown:
			c := diags[i]
			cat = &c
		case DiagBotTagUnknown:
			if diags[i].Field == "tags.brand-new-facet" {
				c := diags[i]
				tag = &c
			}
		}
	}
	if cat == nil {
		t.Fatalf("no C240 for unknown category %q; got %+v", m.Category, diags)
	}
	if cat.Severity != SeverityWarning {
		t.Errorf("C240 severity = %v, want warning (an unknown choice is never a rejection)", cat.Severity)
	}
	if !strings.Contains(cat.Hint, "build") || !strings.Contains(cat.Hint, "steer") {
		t.Errorf("C240 hint = %q, want it to name the known slugs", cat.Hint)
	}
	if tag == nil {
		t.Fatalf("no C241 for unknown tag; got %+v", diags)
	}
	if tag.Severity != SeverityWarning {
		t.Errorf("C241 severity = %v, want warning", tag.Severity)
	}
	// The known tag must NOT be flagged: one warning per unknown tag only.
	for _, d := range diags {
		if d.Code == DiagBotTagUnknown && d.Field == "tags.security" {
			t.Errorf("known tag `security` flagged: %+v", d)
		}
	}
}

func TestCheckBotTaxonomy_SilentOnKnownVocabulary(t *testing.T) {
	m := &bundle.Manifest{
		Category: "verify",
		Tags:     bundle.KnownBotTags,
	}
	for _, d := range CheckConsistency(Input{Manifest: m}) {
		if d.Code == DiagBotCategoryUnknown || d.Code == DiagBotTagUnknown {
			t.Errorf("known vocabulary flagged: %+v", d)
		}
	}
}

func TestCheckBotTaxonomy_SilentWithoutCategory(t *testing.T) {
	// Uncategorized is a legitimate state (loose files, third-party
	// bundles): absence of category produces no diagnostic.
	m := &bundle.Manifest{Name: "bare"}
	for _, d := range CheckConsistency(Input{Manifest: m}) {
		if d.Code == DiagBotCategoryUnknown {
			t.Errorf("empty category flagged: %+v", d)
		}
	}
}
