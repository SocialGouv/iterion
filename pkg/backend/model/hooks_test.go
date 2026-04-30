package model

import (
	"encoding/json"
	"testing"
)

func TestSummarizeToolCallInput(t *testing.T) {
	tests := []struct {
		name string
		tool string
		raw  string
		want string
	}{
		{name: "read_file path", tool: "read_file", raw: `{"path":"/tmp/foo.go"}`, want: "/tmp/foo.go"},
		{name: "file_edit path", tool: "file_edit", raw: `{"path":"/a/b.go","old_string":"x"}`, want: "/a/b.go"},
		{name: "write_file path", tool: "write_file", raw: `{"path":"/c.go","content":"..."}`, want: "/c.go"},
		{name: "bash command", tool: "bash", raw: `{"command":"go test ./..."}`, want: "go test ./..."},
		{name: "grep pattern", tool: "grep", raw: `{"pattern":"foo.*bar"}`, want: "foo.*bar"},
		{name: "glob pattern", tool: "glob", raw: `{"pattern":"**/*.go"}`, want: "**/*.go"},
		{name: "web_fetch url", tool: "web_fetch", raw: `{"url":"https://x"}`, want: "https://x"},
		{name: "ask_user question", tool: "ask_user", raw: `{"question":"why?"}`, want: "why?"},
		{name: "newlines collapsed", tool: "bash", raw: `{"command":"line1\nline2"}`, want: "line1 line2"},
		{name: "unknown tool", tool: "exotic_tool", raw: `{"path":"/x"}`, want: ""},
		{name: "empty input", tool: "read_file", raw: ``, want: ""},
		{name: "missing primary key", tool: "read_file", raw: `{"other":"x"}`, want: ""},
		{name: "empty string value", tool: "read_file", raw: `{"path":""}`, want: ""},
		{name: "fallback secondary key", tool: "read_file", raw: `{"file_path":"/x.go"}`, want: "/x.go"},
		{name: "malformed json", tool: "read_file", raw: `{not json`, want: ""},
		{name: "truncation", tool: "bash", raw: `{"command":"` + repeat("a", 250) + `"}`, want: repeat("a", 200) + "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeToolCallInput(tt.tool, json.RawMessage(tt.raw))
			if got != tt.want {
				t.Errorf("summarizeToolCallInput(%q, %q) = %q, want %q", tt.tool, tt.raw, got, tt.want)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
