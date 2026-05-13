package delegate

import (
	"testing"
)

func TestToolUseDetail_CoversStandardTools(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input map[string]any
		want  string
	}{
		{"Read", "Read", map[string]any{"file_path": "/tmp/x.go"}, "/tmp/x.go"},
		{"Grep", "Grep", map[string]any{"pattern": "func .*", "path": "/src"}, "func .*"},
		{"WebFetch", "WebFetch", map[string]any{"url": "https://example.com/api", "prompt": "summarise"}, "https://example.com/api"},
		{"WebSearch", "WebSearch", map[string]any{"query": "iterion docs"}, "iterion docs"},
		{"ToolSearch", "ToolSearch", map[string]any{"query": "select:Read"}, "select:Read"},
		{"Bash", "Bash", map[string]any{"command": "echo hi"}, "echo hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolUseDetail(tc.tool, tc.input)
			if got != tc.want {
				t.Fatalf("toolUseDetail(%q) = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}

func TestToolUseDetail_TodoWriteNonEmpty(t *testing.T) {
	input := map[string]any{"todos": []any{
		map[string]any{"content": "Implement core", "status": "in_progress"},
		map[string]any{"content": "Test", "status": "pending"},
	}}
	got := toolUseDetail("TodoWrite", input)
	if got == "" {
		t.Fatalf("TodoWrite detail should not be empty, got %q", got)
	}
}
