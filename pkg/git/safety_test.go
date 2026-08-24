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
		{"+force", "leading plus (refspec force sigil at fetch call sites)"},
		{"HEAD", "git check-ref-format --branch refuses it"},
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

// The widening of ValidateBranchName to git's own rules removed a guard that
// was silently load-bearing: git happily accepts `;`, backticks and quotes,
// and a forge-controlled branch name travels on as a launch var that bots
// interpolate into `bash -c` (bots/branch-improve-loop's
// `PUSH_BRANCH={{vars.push_branch}} python3 -c …`, unquoted). The shell gate
// is separate so Renovate's grouped branches keep flowing.
func TestValidateShellSafeRef(t *testing.T) {
	t.Parallel()

	mustPass := []string{
		"main",
		"renovate/npm-(non-major)", // the whole point of the widening
		"renovate/major-(major)-x", // parens: bash syntax error at worst, never RCE
		"dependabot/go_modules/x-1.2.3",
		"iterion/improve/019f-abcd",
		"feat+plus",
		"héllo-branche",
	}
	for _, name := range mustPass {
		if err := ValidateShellSafeRef(name); err != nil {
			t.Errorf("ValidateShellSafeRef(%q) = %v, want nil — a legitimate bot branch must still flow", name, err)
		}
	}

	// Each of these ENDS the current word or command and starts
	// attacker-chosen text. git accepts every one of them.
	mustFail := []struct{ name, why string }{
		{"x;id;#", "command separator — the live RCE shape in an unquoted assignment"},
		{"x|id", "pipe"},
		{"x&id", "background + separator"},
		{"x$(id)", "command substitution"},
		{"x`id`", "backtick substitution"},
		{"x'y", "single quote closes a bot's SCOPE_MODE='…' quoting"},
		{"x\"y", "double quote"},
		{"x<y", "redirect in"},
		{"x>y", "redirect out"},
		{"x!y", "history expansion"},
		{"-x", "flag injection (inherited from ValidateBranchName)"},
	}
	for _, tc := range mustFail {
		if err := ValidateShellSafeRef(tc.name); err == nil {
			t.Errorf("ValidateShellSafeRef(%q) = nil, want error (%s)", tc.name, tc.why)
		}
	}

	// The two gates are deliberately different: git-faithful for fetching,
	// shell-safe for interpolating. A test that let them collapse into one
	// would hide the next regression in either direction.
	if err := ValidateBranchName("x;id;#"); err != nil {
		t.Errorf("ValidateBranchName(%q) = %v, want nil — it must stay faithful to git check-ref-format", "x;id;#", err)
	}
}
