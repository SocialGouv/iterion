package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

const minimalAgentSource = `agent draft:
  model: "anthropic/claude-sonnet-4-6"
  reasoning_effort: medium

workflow main:
  draft -> done
`

const tinyJudgeSource = `judge verify:
  model: "openai/gpt-5.4-mini"
  reasoning_effort: low
  max_tokens: 256

workflow main:
  verify -> done
`

const toolOnlySource = `tool echo:
  cmd: "echo hi"

workflow main:
  echo -> done
`

func postPreviewCost(t *testing.T, hs string, body string) previewCostResponse {
	t.Helper()
	resp, err := http.Post(hs+"/api/runs/preview-cost", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got previewCostResponse
	decodeJSON(t, resp, &got)
	return got
}

func TestPreviewCost_AgentNode(t *testing.T) {
	_, hs := newTestServer(t)
	got := postPreviewCost(t, hs.URL, `{"source":`+jsonString(minimalAgentSource)+`}`)
	if len(got.Nodes) != 1 {
		t.Fatalf("expected 1 LLM node, got %d", len(got.Nodes))
	}
	n := got.Nodes[0]
	if n.Kind != "agent" || n.NodeID != "draft" {
		t.Fatalf("unexpected node: %+v", n)
	}
	if n.TokensIn == 0 || n.TokensOut == 0 {
		t.Fatalf("expected non-zero token estimate: %+v", n)
	}
	if got.CostMinUSD <= 0 || got.CostMaxUSD <= got.CostMinUSD {
		t.Fatalf("expected positive monotone cost bracket, got min=%v max=%v", got.CostMinUSD, got.CostMaxUSD)
	}
}

func TestPreviewCost_JudgeRespectsMaxTokens(t *testing.T) {
	_, hs := newTestServer(t)
	got := postPreviewCost(t, hs.URL, `{"source":`+jsonString(tinyJudgeSource)+`}`)
	if len(got.Nodes) != 1 {
		t.Fatalf("expected 1 LLM node, got %d", len(got.Nodes))
	}
	n := got.Nodes[0]
	if n.Kind != "judge" {
		t.Fatalf("expected judge kind, got %q", n.Kind)
	}
	if n.TokensOut > 256 {
		t.Fatalf("max_tokens 256 should cap output estimate, got %d", n.TokensOut)
	}
}

func TestPreviewCost_NoLLMNodes(t *testing.T) {
	_, hs := newTestServer(t)
	got := postPreviewCost(t, hs.URL, `{"source":`+jsonString(toolOnlySource)+`}`)
	if len(got.Nodes) != 0 {
		t.Fatalf("expected zero LLM nodes for tool-only workflow")
	}
	if !containsNote(got.Notes, "no_llm_nodes") {
		t.Fatalf("expected no_llm_nodes note, got %+v", got.Notes)
	}
}

func TestPreviewCost_UnparseableSource(t *testing.T) {
	_, hs := newTestServer(t)
	got := postPreviewCost(t, hs.URL, `{"source":"not a valid iter file ::"}`)
	// Endpoint must never 500 on garbage — chip stays hidden, Launch
	// still fails at /api/validate. See cost_preview.go handler header.
	if got.CostMinUSD != 0 || got.CostMaxUSD != 0 {
		t.Fatalf("expected zero cost on unparseable input, got %+v", got)
	}
}

func TestPreviewCost_MissingBoth(t *testing.T) {
	resp, err := http.Post((&newTestHSAdapter{t: t}).URL()+"/api/runs/preview-cost", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// jsonString escapes a Go string for embedding inside a JSON literal.
// Avoids pulling encoding/json just for two test bodies.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func containsNote(notes []string, want string) bool {
	for _, n := range notes {
		if n == want {
			return true
		}
	}
	return false
}

// newTestHSAdapter wraps newTestServer in a struct so TestPreviewCost_MissingBoth
// (which spawns its own server inline) can call .URL() without re-declaring vars.
type newTestHSAdapter struct {
	t *testing.T
}

func (a *newTestHSAdapter) URL() string {
	_, hs := newTestServer(a.t)
	return hs.URL
}
