package parser_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestAgentSkills(t *testing.T) {
	src := `agent draft:
  model: "gpt-4"
  skills: ["changelog-writer", "semver-bump"]
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)
	if len(res.File.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(res.File.Agents))
	}
	if got := strings.Join(res.File.Agents[0].Skills, ","); got != "changelog-writer,semver-bump" {
		t.Fatalf("Skills = %q", got)
	}
}

func TestJudgeSkills(t *testing.T) {
	src := `judge reviewer:
  model: "gpt-4"
  skills: ["review-rubric"]
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)
	if len(res.File.Judges) != 1 {
		t.Fatalf("expected 1 judge, got %d", len(res.File.Judges))
	}
	if got := strings.Join(res.File.Judges[0].Skills, ","); got != "review-rubric" {
		t.Fatalf("Skills = %q", got)
	}
}

func TestWorkflowSkillsDefault(t *testing.T) {
	src := `agent a:
  model: "gpt-4"

workflow main:
  entry: a
  skills: ["house-style"]
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)
	if len(res.File.Workflows) != 1 {
		t.Fatal("expected 1 workflow decl")
	}
	if got := strings.Join(res.File.Workflows[0].Skills, ","); got != "house-style" {
		t.Fatalf("workflow Skills = %q", got)
	}
}
