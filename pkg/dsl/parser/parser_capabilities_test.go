package parser_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestAgentCapabilities(t *testing.T) {
	src := `agent po:
  model: "gpt-4"
  capabilities: [board.create, board.move, board.read]
`
	res := parser.ParseLenient("test.bot", src)
	assertNoDiags(t, res)

	if len(res.File.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(res.File.Agents))
	}
	a := res.File.Agents[0]
	if got := strings.Join(a.Capabilities, ","); got != "board.create,board.move,board.read" {
		t.Fatalf("Capabilities = %q", got)
	}
}

func TestJudgeCapabilities(t *testing.T) {
	src := `judge reviewer:
  model: "gpt-4"
  capabilities: [board.read]
`
	res := parser.ParseLenient("test.bot", src)
	assertNoDiags(t, res)

	if len(res.File.Judges) != 1 {
		t.Fatalf("expected 1 judge, got %d", len(res.File.Judges))
	}
	j := res.File.Judges[0]
	if got := strings.Join(j.Capabilities, ","); got != "board.read" {
		t.Fatalf("Capabilities = %q", got)
	}
}

func TestWorkflowCapabilitiesDefault(t *testing.T) {
	src := `agent a:
  model: "gpt-4"

workflow w:
  entry: a
  capabilities: [board.read, board.create]

  a -> done
`
	res := parser.ParseLenient("test.bot", src)
	assertNoDiags(t, res)

	if len(res.File.Workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(res.File.Workflows))
	}
	wf := res.File.Workflows[0]
	if got := strings.Join(wf.Capabilities, ","); got != "board.read,board.create" {
		t.Fatalf("Capabilities = %q", got)
	}
}
