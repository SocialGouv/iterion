package knowledge

import (
	"errors"
	"testing"
)

func TestValidateDocPath(t *testing.T) {
	ok := []string{"a.md", "findings/2026.md", "a/b/c.md", "dotted.name.md", "./x.md"}
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
