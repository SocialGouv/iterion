package tooldisplay

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestHeaderDetail_CamelCase(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		input   any
		want    string
		wantSub bool
	}{
		{
			name:  "Read shows file_path",
			tool:  "Read",
			input: map[string]any{"file_path": "/tmp/x.go"},
			want:  "/tmp/x.go",
		},
		{
			name:  "Bash shows first line of command",
			tool:  "Bash",
			input: map[string]any{"command": "echo hi\nrm -rf nope"},
			want:  "echo hi",
		},
		{
			name:  "WebFetch shows url",
			tool:  "WebFetch",
			input: map[string]any{"url": "https://example.com/api"},
			want:  "https://example.com/api",
		},
		{
			name:  "WebSearch shows query",
			tool:  "WebSearch",
			input: map[string]any{"query": "iterion docs"},
			want:  "iterion docs",
		},
		{
			name:  "Task shows description",
			tool:  "Task",
			input: map[string]any{"description": "Run audit", "prompt": "long..."},
			want:  "Run audit",
		},
		{
			name:  "ToolSearch shows query",
			tool:  "ToolSearch",
			input: map[string]any{"query": "select:Read"},
			want:  "select:Read",
		},
		{
			name:  "NotebookEdit prefers notebook_path",
			tool:  "NotebookEdit",
			input: map[string]any{"notebook_path": "/x.ipynb", "file_path": "/y"},
			want:  "/x.ipynb",
		},
		{
			name:  "Grep shows pattern",
			tool:  "Grep",
			input: map[string]any{"pattern": "func .*\\(", "path": "/src"},
			want:  "func .*\\(",
		},
		{
			name:    "TodoWrite shows summary",
			tool:    "TodoWrite",
			input:   map[string]any{"todos": []any{map[string]any{"content": "Implement", "status": "in_progress"}, map[string]any{"content": "Test", "status": "pending"}}},
			want:    "Implement",
			wantSub: true,
		},
		{
			name:  "TodoWrite empty list returns empty",
			tool:  "TodoWrite",
			input: map[string]any{"todos": []any{}},
			want:  "",
		},
		{
			name:    "AskUserQuestion shows first question",
			tool:    "AskUserQuestion",
			input:   map[string]any{"questions": []any{map[string]any{"question": "Which option?"}, map[string]any{"question": "And then?"}}},
			want:    "Which option?",
			wantSub: true,
		},
		{
			name:  "Unknown tool falls back to file_path",
			tool:  "CustomTool",
			input: map[string]any{"file_path": "/foo"},
			want:  "/foo",
		},
		{
			name:  "Unknown tool with no matching key returns empty",
			tool:  "WeirdTool",
			input: map[string]any{"weird": "value"},
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HeaderDetail(tc.tool, mustJSON(t, tc.input), CamelCaseKeys)
			if tc.wantSub {
				if !strings.Contains(got, tc.want) {
					t.Fatalf("HeaderDetail(%q) = %q, want substring %q", tc.tool, got, tc.want)
				}
			} else if got != tc.want {
				t.Fatalf("HeaderDetail(%q) = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}

func TestHeaderDetail_SnakeCase(t *testing.T) {
	cases := []struct {
		tool  string
		input any
		want  string
	}{
		{"read_file", map[string]any{"path": "/tmp/x"}, "/tmp/x"},
		{"web_fetch", map[string]any{"url": "https://x"}, "https://x"},
		{"task", map[string]any{"description": "Audit"}, "Audit"},
		{"slash_command", map[string]any{"command_name": "/help"}, "/help"},
		{"slash_command", map[string]any{"command": "/help"}, "/help"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			got := HeaderDetail(tc.tool, mustJSON(t, tc.input), SnakeCaseKeys)
			if got != tc.want {
				t.Fatalf("HeaderDetail(%q) = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}

func TestHeaderDetail_Truncation(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := HeaderDetail("Bash", mustJSON(t, map[string]any{"command": long}), CamelCaseKeys)
	if len(got) > 100 {
		t.Fatalf("expected truncation to 100 chars, got %d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected trailing ellipsis, got %q", got)
	}
}

func TestHeaderDetail_EmptyInput(t *testing.T) {
	if got := HeaderDetail("Read", nil, CamelCaseKeys); got != "" {
		t.Fatalf("expected empty for nil input, got %q", got)
	}
	if got := HeaderDetail("Read", []byte("not json"), CamelCaseKeys); got != "" {
		t.Fatalf("expected empty for invalid JSON, got %q", got)
	}
}

func TestBlockBody_TodoWrite(t *testing.T) {
	input := map[string]any{"todos": []any{
		map[string]any{"content": "Set up project", "status": "pending"},
		map[string]any{"content": "Implement", "status": "in_progress"},
		map[string]any{"content": "Test", "status": "completed"},
	}}
	got := BlockBody("TodoWrite", mustJSON(t, input))
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "☐") {
		t.Errorf("line 0 (pending) should start with ☐, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "★") {
		t.Errorf("line 1 (in_progress) should start with ★, got %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "☑") {
		t.Errorf("line 2 (completed) should start with ☑, got %q", lines[2])
	}
}

func TestBlockBody_BashMultiline(t *testing.T) {
	cmd := "echo hi\nls -la"
	got := BlockBody("Bash", mustJSON(t, map[string]any{"command": cmd}))
	if got != cmd {
		t.Fatalf("expected full multiline command, got %q", got)
	}
}

func TestBlockBody_BashSingleline(t *testing.T) {
	got := BlockBody("Bash", mustJSON(t, map[string]any{"command": "echo hi"}))
	if got != "" {
		t.Fatalf("expected empty body for single-line command, got %q", got)
	}
}

func TestBlockBody_NonStructured(t *testing.T) {
	if got := BlockBody("Read", mustJSON(t, map[string]any{"file_path": "/x"})); got != "" {
		t.Fatalf("expected empty body for Read, got %q", got)
	}
}

func TestIsStructured(t *testing.T) {
	for _, name := range []string{"TodoWrite", "WebFetch", "todo_write", "web_fetch"} {
		if !IsStructured(name) {
			t.Errorf("%q should be structured", name)
		}
	}
	for _, name := range []string{"Read", "Bash", "read_file", "bash"} {
		if IsStructured(name) {
			t.Errorf("%q should NOT be structured (volumetric)", name)
		}
	}
}
