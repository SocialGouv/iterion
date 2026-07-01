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
			name:  "Task combines subagent_type and description",
			tool:  "Task",
			input: map[string]any{"subagent_type": "code-reviewer", "description": "Run audit", "prompt": "long..."},
			want:  "code-reviewer: Run audit",
		},
		{
			name:  "Task falls back to description when subagent_type missing",
			tool:  "Task",
			input: map[string]any{"description": "Run audit", "prompt": "long..."},
			want:  "Run audit",
		},
		{
			name:  "Agent combines subagent_type and description",
			tool:  "Agent",
			input: map[string]any{"subagent_type": "Explore", "description": "Locate handler", "prompt": "Where is auth.ts?"},
			want:  "Explore: Locate handler",
		},
		{
			name:  "Agent falls back to description-only",
			tool:  "Agent",
			input: map[string]any{"description": "Locate handler"},
			want:  "Locate handler",
		},
		{
			name:  "Agent falls back to subagent_type-only",
			tool:  "Agent",
			input: map[string]any{"subagent_type": "Explore"},
			want:  "Explore",
		},
		{
			name:  "Agent with neither field returns empty",
			tool:  "Agent",
			input: map[string]any{"prompt": "just a prompt"},
			want:  "",
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
		{"agent", map[string]any{"subagent_type": "explore", "description": "Find foo"}, "explore: Find foo"},
		{"agent", map[string]any{"description": "Find foo"}, "Find foo"},
		{"task", map[string]any{"description": "Audit"}, "Audit"},
		{"task", map[string]any{"subagent_type": "verification", "description": "Audit"}, "verification: Audit"},
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

func TestBlockBody_Agent(t *testing.T) {
	prompt := "Step 1: investigate the auth handler.\nStep 2: report back."
	for _, name := range []string{"Agent", "Task", "agent", "task"} {
		t.Run(name, func(t *testing.T) {
			got := BlockBody(name, mustJSON(t, map[string]any{
				"subagent_type": "Explore",
				"description":   "Locate handler",
				"prompt":        prompt,
			}))
			if got != prompt {
				t.Fatalf("BlockBody(%q) = %q, want full prompt", name, got)
			}
		})
	}
	if got := BlockBody("Agent", mustJSON(t, map[string]any{"description": "no prompt here"})); got != "" {
		t.Fatalf("Agent with no prompt should return empty body, got %q", got)
	}
}

func TestShortenWorktreePath(t *testing.T) {
	cases := []struct{ in, want string }{
		// Bare absolute worktree path → workspace-relative.
		{"/home/jo/lab/ai/iterion/.iterion/worktrees/019f1e43-99db-7e74-95c3-b587bd4765c8/e2e/live_feat_ski.go", "e2e/live_feat_ski.go"},
		{"/root/.iterion/worktrees/abc123/pkg/runtime/engine.go", "pkg/runtime/engine.go"},
		// Non-worktree absolute path → untouched.
		{"/home/jo/lab/ai/iterion/pkg/runtime/engine.go", "/home/jo/lab/ai/iterion/pkg/runtime/engine.go"},
		// Bash command containing a worktree path (has spaces) → untouched.
		{"cd /x/worktrees/id/pkg && go test ./...", "cd /x/worktrees/id/pkg && go test ./..."},
		// Relative path / non-absolute → untouched.
		{"e2e/foo.go", "e2e/foo.go"},
		// URL → untouched.
		{"https://example.com/worktrees/x/y", "https://example.com/worktrees/x/y"},
	}
	for _, c := range cases {
		if got := shortenWorktreePath(c.in); got != c.want {
			t.Errorf("shortenWorktreePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// HeaderDetail must show the workspace-relative path for a worktree Read.
func TestHeaderDetail_ShortensWorktreePath(t *testing.T) {
	in := mustJSON(t, map[string]any{"file_path": "/home/u/.iterion/worktrees/run-id-xyz/pkg/runtime/branch.go"})
	got := HeaderDetail("Read", in, CamelCaseKeys)
	if got != "pkg/runtime/branch.go" {
		t.Fatalf("HeaderDetail Read = %q, want workspace-relative %q", got, "pkg/runtime/branch.go")
	}
}
