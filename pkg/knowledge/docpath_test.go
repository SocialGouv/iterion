package knowledge

import (
	"errors"
	"testing"
)

func TestValidateDocPath(t *testing.T) {
	ok := []string{"a.md", "findings/2026.md", "a/b/c.md", "dotted.name.md",
		// A backslash is a plain character, not a separator, so a
		// Windows-shaped name is not caught by the canonicality rule.
		`a\b.md`,
	}
	for _, p := range ok {
		if err := ValidateDocPath(p); err != nil {
			t.Errorf("ValidateDocPath(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{
		"",          // empty
		"/abs.md",   // absolute
		"../escape", // traversal
		"a/../../x", // mid traversal
		"..",        // bare dotdot
		`\\windows`, // backslash absolute
		"C:/win.md", // drive letter
		"a\x00b.md", // NUL
		// Characterization: the drive-letter guard rejects ANY path whose
		// second character is ':', even a plausible relative name.
		"a:b.md",
		// Non-canonical spellings of a path that is otherwise perfectly
		// legal. These were ACCEPTED until a reproduced data loss showed why
		// they must not be: they address the same document as their clean
		// form, the FS adapter silently normalises them while the cloud one
		// stores them verbatim, and a consumer that round-trips a document
		// through the filesystem then fails to find the key it was given —
		// reading the mismatch as "the agent deleted this" and destroying an
		// untouched note on both sides.
		"./x.md",   // leading dot segment
		"a//b.md",  // doubled separator
		"a/./b.md", // interior dot segment
		"x.md/",    // trailing separator
	}
	for _, p := range bad {
		if err := ValidateDocPath(p); err == nil {
			t.Errorf("ValidateDocPath(%q) = nil, want error", p)
		} else if !errors.Is(err, ErrInvalidDocPath) {
			t.Errorf("ValidateDocPath(%q) error = %v, want ErrInvalidDocPath", p, err)
		}
	}
}

func TestSpaceRefValidate_RejectsTraversalSegments(t *testing.T) {
	if err := (SpaceRef{Visibility: VisibilityBot, ProjectID: "proj", BotID: "bot", Name: "n"}).Validate(); err != nil {
		t.Fatalf("clean ref should validate: %v", err)
	}
	bad := []SpaceRef{
		{Visibility: VisibilityBot, ProjectID: "../../etc", BotID: "bot", Name: "n"}, // project traversal
		{Visibility: VisibilityBot, ProjectID: "a/b", BotID: "bot", Name: "n"},       // project separator
		{Visibility: VisibilityBot, ProjectID: "proj", BotID: "..", Name: "n"},       // bot traversal
		{Visibility: VisibilityUser, UserID: "../x", Name: "n"},                      // user traversal
		{Visibility: VisibilityOrg, TenantID: `a\b`, Name: "n"},                      // tenant separator
	}
	for i, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("case %d (%+v): traversal segment should be rejected", i, r)
		}
	}
}
