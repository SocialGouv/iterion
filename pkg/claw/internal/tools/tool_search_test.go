package tools

import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"testing"
)

var testTools = []api.Tool{
	{Name: "bash", Description: "Execute bash commands"},
	{Name: "read_file", Description: "Read file contents"},
	{Name: "write_file", Description: "Write file contents"},
	{Name: "file_edit", Description: "Edit files with search and replace"},
	{Name: "grep", Description: "Search for patterns in files"},
}

func TestToolSearch_SelectExact(t *testing.T) {
	input := map[string]any{
		"query": "select:bash,grep",
	}
	result, err := ExecuteToolSearch(input, testTools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed ToolSearchResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if len(parsed.Matches) != 2 {
		t.Fatalf("expected 2 matches, got %d: %v", len(parsed.Matches), parsed.Matches)
	}
	found := map[string]bool{}
	for _, m := range parsed.Matches {
		found[m] = true
	}
	if !found["bash"] || !found["grep"] {
		t.Errorf("expected bash and grep in matches, got %v", parsed.Matches)
	}
}

func TestToolSearch_Keywords(t *testing.T) {
	input := map[string]any{
		"query": "file",
	}
	result, err := ExecuteToolSearch(input, testTools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed ToolSearchResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if len(parsed.Matches) < 2 {
		t.Fatalf("expected at least 2 matches for 'file', got %d: %v", len(parsed.Matches), parsed.Matches)
	}
	// Should find read_file, write_file, and file_edit
	found := map[string]bool{}
	for _, m := range parsed.Matches {
		found[m] = true
	}
	for _, expected := range []string{"read_file", "write_file", "file_edit"} {
		if !found[expected] {
			t.Errorf("expected %q in matches, got %v", expected, parsed.Matches)
		}
	}
}

func TestToolSearch_MaxResults(t *testing.T) {
	input := map[string]any{
		"query":       "file",
		"max_results": float64(2), // JSON numbers are float64
	}
	result, err := ExecuteToolSearch(input, testTools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed ToolSearchResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if len(parsed.Matches) > 2 {
		t.Errorf("expected at most 2 matches, got %d: %v", len(parsed.Matches), parsed.Matches)
	}
}

func TestToolSearch_RequiredTerm(t *testing.T) {
	input := map[string]any{
		"query": "+bash search",
	}
	result, err := ExecuteToolSearch(input, testTools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed ToolSearchResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	// All matches must contain "bash" in name or description
	for _, m := range parsed.Matches {
		if m != "bash" {
			t.Errorf("expected only 'bash' to match +bash requirement, got %q", m)
		}
	}
	if len(parsed.Matches) == 0 {
		t.Error("expected at least one match for +bash")
	}
}

func TestToolSearch_Empty(t *testing.T) {
	input := map[string]any{
		"query": "",
	}
	_, err := ExecuteToolSearch(input, testTools)
	if err == nil {
		t.Fatal("expected error for empty query")
	}
}
