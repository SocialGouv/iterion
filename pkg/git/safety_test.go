package git

import "testing"

func TestValidateBranchName(t *testing.T) {
	t.Parallel()

	valid := []string{
		"main",
		"feature/x",
		"iterion/run/2026-05-19",
		"a",
		"v1.2.3",
		"hot-fix",
		"a.b.c/d_e",
		// git check-ref-format accepts all of these; the old allowlist
		// refused them and made Renovate's grouped branches unreviewable.
		"renovate/npm-(non-major)",
		"renovate/major-(major)-updates",
		"feat+plus",
		"topic@2024",
		"a=b",
		"héllo-branche",
		"_under",
		"deps/bump#42",
	}
	for _, name := range valid {
		if err := ValidateBranchName(name); err != nil {
			t.Errorf("ValidateBranchName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []struct {
		name   string
		reason string
	}{
		{"", "empty"},
		{"-force", "leading dash (flag injection)"},
		{"--force", "looks like a long flag"},
		{"/abs", "leading slash"},
		{".hidden", "leading dot"},
		{"x/.hidden", "component starting with dot"},
		{"x.lock/y", "component ending with .lock"},
		{"@", "the single character @"},
		{"feat with space", "space"},
		{"feat\ttab", "tab"},
		{"feat:colon", "colon (git ref-format)"},
		{"feat?glob", "question mark"},
		{"feat*star", "wildcard"},
		{"feat~tilde", "tilde"},
		{"feat^caret", "caret"},
		{"feat\\bs", "backslash"},
		{"feat@{ref}", "@{ sequence"},
		{"feat\x00null", "null byte"},
		{"branch/..", "contains .."},
		{"feature/../etc", "traversal"},
		{"branch//double", "double slash"},
		{"trailing/", "trailing slash"},
		{"trailing.", "trailing dot"},
		{"trailing.lock", ".lock suffix"},
	}
	for _, tc := range invalid {
		if err := ValidateBranchName(tc.name); err == nil {
			t.Errorf("ValidateBranchName(%q) = nil, want error (%s)", tc.name, tc.reason)
		}
	}

	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateBranchName(string(long)); err == nil {
		t.Errorf("ValidateBranchName(256 bytes) = nil, want error")
	}
}
