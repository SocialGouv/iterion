package permissions

import "testing"

func TestParsePermissionRuleToolOnly(t *testing.T) {
	rule := parsePermissionRule("Bash")
	if rule.toolName != "Bash" {
		t.Errorf("toolName = %q, want 'Bash'", rule.toolName)
	}
	if rule.matcher.kind != matcherAny {
		t.Errorf("matcher.kind = %d, want matcherAny", rule.matcher.kind)
	}
}

func TestParsePermissionRulePrefixMatcher(t *testing.T) {
	rule := parsePermissionRule("Bash(git:*)")
	if rule.toolName != "Bash" {
		t.Errorf("toolName = %q, want 'Bash'", rule.toolName)
	}
	if rule.matcher.kind != matcherPrefix {
		t.Errorf("matcher.kind = %d, want matcherPrefix", rule.matcher.kind)
	}
	if rule.matcher.value != "git" {
		t.Errorf("matcher.value = %q, want 'git'", rule.matcher.value)
	}
}

func TestParsePermissionRuleExactMatcher(t *testing.T) {
	rule := parsePermissionRule("Bash(echo hello)")
	if rule.toolName != "Bash" {
		t.Errorf("toolName = %q, want 'Bash'", rule.toolName)
	}
	if rule.matcher.kind != matcherExact {
		t.Errorf("matcher.kind = %d, want matcherExact", rule.matcher.kind)
	}
	if rule.matcher.value != "echo hello" {
		t.Errorf("matcher.value = %q, want 'echo hello'", rule.matcher.value)
	}
}

func TestParsePermissionRuleWildcard(t *testing.T) {
	rule := parsePermissionRule("Bash(*)")
	if rule.toolName != "Bash" {
		t.Errorf("toolName = %q, want 'Bash'", rule.toolName)
	}
	if rule.matcher.kind != matcherAny {
		t.Errorf("matcher.kind = %d, want matcherAny for wildcard", rule.matcher.kind)
	}
}

func TestParsePermissionRuleEmptyContent(t *testing.T) {
	rule := parsePermissionRule("Bash()")
	if rule.toolName != "Bash" {
		t.Errorf("toolName = %q, want 'Bash'", rule.toolName)
	}
	if rule.matcher.kind != matcherAny {
		t.Errorf("matcher.kind = %d, want matcherAny for empty content", rule.matcher.kind)
	}
}

func TestParsePermissionRuleEscapedParens(t *testing.T) {
	// Escaped parens in content should be unescaped
	rule := parsePermissionRule(`Bash(echo \(hello\))`)
	if rule.toolName != "Bash" {
		t.Errorf("toolName = %q, want 'Bash'", rule.toolName)
	}
	if rule.matcher.kind != matcherExact {
		t.Errorf("matcher.kind = %d, want matcherExact", rule.matcher.kind)
	}
	if rule.matcher.value != "echo (hello)" {
		t.Errorf("matcher.value = %q, want 'echo (hello)'", rule.matcher.value)
	}
}

func TestParsePermissionRuleWhitespace(t *testing.T) {
	rule := parsePermissionRule("  Bash ( git:* ) ")
	if rule.toolName != "Bash" {
		t.Errorf("toolName = %q, want 'Bash'", rule.toolName)
	}
	if rule.matcher.kind != matcherPrefix {
		t.Errorf("matcher.kind = %d, want matcherPrefix", rule.matcher.kind)
	}
	if rule.matcher.value != "git" {
		t.Errorf("matcher.value = %q, want 'git'", rule.matcher.value)
	}
}

func TestPermissionRuleMatches(t *testing.T) {
	tests := []struct {
		name    string
		rule    string
		tool    string
		input   string
		matches bool
	}{
		{
			name:    "any matcher matches all inputs",
			rule:    "bash",
			tool:    "bash",
			input:   `{"command":"anything"}`,
			matches: true,
		},
		{
			name:    "wrong tool name rejects",
			rule:    "bash",
			tool:    "write_file",
			input:   `{"command":"anything"}`,
			matches: false,
		},
		{
			name:    "prefix matches command starting with prefix",
			rule:    "bash(git:*)",
			tool:    "bash",
			input:   `{"command":"git status"}`,
			matches: true,
		},
		{
			name:    "prefix rejects command not starting with prefix",
			rule:    "bash(git:*)",
			tool:    "bash",
			input:   `{"command":"rm -rf /"}`,
			matches: false,
		},
		{
			name:    "exact matches exact command",
			rule:    "bash(ls -la)",
			tool:    "bash",
			input:   `{"command":"ls -la"}`,
			matches: true,
		},
		{
			name:    "exact rejects different command",
			rule:    "bash(ls -la)",
			tool:    "bash",
			input:   `{"command":"ls -l"}`,
			matches: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := parsePermissionRule(tt.rule)
			got := rule.matches(tt.tool, tt.input)
			if got != tt.matches {
				t.Errorf("rule %q matches(%q, %q) = %v, want %v",
					tt.rule, tt.tool, tt.input, got, tt.matches)
			}
		})
	}
}

func TestExtractPermissionSubjectJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "extracts command field",
			input: `{"command":"git status","cwd":"/tmp"}`,
			want:  "git status",
		},
		{
			name:  "extracts path field",
			input: `{"path":"/etc/passwd"}`,
			want:  "/etc/passwd",
		},
		{
			name:  "extracts file_path field",
			input: `{"file_path":"/home/user/file.go"}`,
			want:  "/home/user/file.go",
		},
		{
			name:  "extracts filePath field",
			input: `{"filePath":"/home/user/file.go"}`,
			want:  "/home/user/file.go",
		},
		{
			name:  "extracts url field",
			input: `{"url":"https://example.com"}`,
			want:  "https://example.com",
		},
		{
			name:  "command takes priority over path",
			input: `{"command":"git","path":"/tmp"}`,
			want:  "git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPermissionSubject(tt.input)
			if got != tt.want {
				t.Errorf("extractPermissionSubject(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractPermissionSubjectFallsBackToRaw(t *testing.T) {
	// Non-JSON input falls back to raw string.
	got := extractPermissionSubject("plain text input")
	if got != "plain text input" {
		t.Errorf("expected raw input fallback, got %q", got)
	}
}

func TestExtractPermissionSubjectEmptyInput(t *testing.T) {
	got := extractPermissionSubject("")
	if got != "" {
		t.Errorf("expected empty string for empty input, got %q", got)
	}

	got = extractPermissionSubject("   ")
	if got != "" {
		t.Errorf("expected empty string for whitespace input, got %q", got)
	}
}

func TestFindFirstUnescaped(t *testing.T) {
	tests := []struct {
		input  string
		needle rune
		want   int
	}{
		{"hello(world)", '(', 5},
		{`hello\(world)`, '(', -1},
		{`hello\\(world)`, '(', 7},
		{"no match", '(', -1},
		{"(first", '(', 0},
	}
	for _, tt := range tests {
		got := findFirstUnescaped(tt.input, tt.needle)
		if got != tt.want {
			t.Errorf("findFirstUnescaped(%q, %q) = %d, want %d", tt.input, string(tt.needle), got, tt.want)
		}
	}
}

func TestFindLastUnescaped(t *testing.T) {
	tests := []struct {
		input  string
		needle rune
		want   int
	}{
		{"hello(world)", ')', 11},
		{`hello(world\)`, ')', -1},
		{`hello(world\\)`, ')', 13},
		{"a)b)c", ')', 3},
	}
	for _, tt := range tests {
		got := findLastUnescaped(tt.input, tt.needle)
		if got != tt.want {
			t.Errorf("findLastUnescaped(%q, %q) = %d, want %d", tt.input, string(tt.needle), got, tt.want)
		}
	}
}
