package unparse_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// These are the field readers sharing parseToolList / parseDeclaredToolList.
// ArtifactLabels and Skills already quote strings; keep them in the inventory.
func referenceLists(f *ast.File) map[string]*[]string {
	a, j := f.Agents[0], f.Judges[0]
	ga, gj := f.Groups[0].Agents[0], f.Groups[0].Judges[0]
	w := f.Workflows[0]
	return map[string]*[]string{
		"agent.tools": &a.Tools, "agent.policy": &a.ToolPolicy, "agent.capabilities": &a.Capabilities, "agent.labels": &a.ArtifactLabels, "agent.skills": &a.Skills,
		"judge.tools": &j.Tools, "judge.policy": &j.ToolPolicy, "judge.capabilities": &j.Capabilities, "judge.labels": &j.ArtifactLabels, "judge.skills": &j.Skills,
		"workflow.policy": &w.ToolPolicy, "workflow.capabilities": &w.Capabilities, "workflow.skills": &w.Skills,
		"recovery.tools": &f.Tools[0].Recovery.AgentTools, "tool.labels": &f.Tools[0].ArtifactLabels,
		"group.agent.tools": &ga.Tools, "group.agent.policy": &ga.ToolPolicy, "group.agent.capabilities": &ga.Capabilities, "group.agent.labels": &ga.ArtifactLabels, "group.agent.skills": &ga.Skills,
		"group.judge.tools": &gj.Tools, "group.judge.policy": &gj.ToolPolicy, "group.judge.capabilities": &gj.Capabilities, "group.judge.labels": &gj.ArtifactLabels, "group.judge.skills": &gj.Skills,
	}
}

const referenceWorkflow = `schema out:
  ok: bool
prompt p:
  hi
agent a:
  backend: "claw"
  output: out
  user: p
judge j:
  backend: "claw"
  output: out
  user: p
tool t:
  command: "true"
  goal: "produce output"
  postcondition: "true"
  policy: recover
  recovery:
    max_agent_attempts: 1
group g:
  agent ga:
    backend: "claw"
    output: out
    user: p
  judge gj:
    backend: "claw"
    output: out
    user: p
  ga -> gj
workflow w:
  entry: a
  a -> j
  j -> t
  t -> done
`

func TestReferenceListsKeepQuotedValues(t *testing.T) {
	values := []string{"allow:Read", "deny:run_command", "Bash(go test:*)", "git.*", "read_file", "true", "file-name", "two words", `quote"tick` + "`", "back\\slash", "line\nbreak"}
	for _, profile := range []int{1, 2} {
		for name := range referenceLists(parser.Parse("refs.bot", referenceWorkflow).File) {
			t.Run(fmt.Sprintf("v%d/%s", profile, name), func(t *testing.T) {
				p := parser.Parse("refs.bot", referenceWorkflow)
				if len(p.Diagnostics) != 0 {
					t.Fatal(p.Diagnostics)
				}
				f := p.File
				f.Profile = profile
				*referenceLists(f)[name] = values
				out := unparse.Unparse(f)
				back := parser.Parse("refs.bot", out)
				if len(back.Diagnostics) != 0 {
					t.Fatalf("parse: %v\n%s", back.Diagnostics, out)
				}
				if got := *referenceLists(back.File)[name]; !reflect.DeepEqual(got, values) {
					t.Fatalf("got %q want %q", got, values)
				}
				if err := unparse.Verify(f, out); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestReferenceListsKeepBareSpelling(t *testing.T) {
	f := parser.Parse("refs.bot", referenceWorkflow).File
	f.Agents[0].ToolPolicy = []string{"git.*", "read_file", "mcp.server.tool"}
	if out := unparse.Unparse(f); !strings.Contains(out, "tool_policy: [git.*, read_file, mcp.server.tool]") {
		t.Fatal(out)
	}
}
