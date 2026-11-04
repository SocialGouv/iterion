package bots

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The CLI tool names are case-sensitive. The survey skill uses both
// "Task sub-agents" and "sub-agents with the Task tool"; ordinary tasks and
// prose about an agent are not promises to expose a native tool.
var subagentPromise = regexp.MustCompile(`\b(?:Task|Agent)\b[^\n]*\bsub-?agents?\b|\bsub-?agents?\b[^\n]*\b(?:Task|Agent)\b`)

// A skill's audit instructions need a node that can actually delegate on
// every route it declares. Visibility comes from the engine and the native
// argv projection; permission comes from the same gate predicate as lint.
func TestSkillSubagentPromisesHaveACallableTool(t *testing.T) {
	workflows := map[string][]*ir.Workflow{}
	for _, sw := range shippedWorkflows(t) {
		if slug := bundleSlug(sw.prefix); slug != "" {
			workflows[slug] = append(workflows[slug], sw.wf)
		}
	}
	promises := 0
	attributionSeen := false
	for slug, wfs := range workflows {
		dir := filepath.Join(slug, "skills")
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if os.IsNotExist(walkErr) && path == dir {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".md" {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// This page attributes the upstream harness's parallelism; its
			// frontmatter explicitly says it is reference documentation.
			// Pin that reason rather than exempting every disabled skill.
			if filepath.ToSlash(path) == "sec-audit-source/skills/HARNESS-ATTRIBUTION.md" &&
				strings.Contains(string(body), "Reference documentation, not a runtime skill.") {
				attributionSeen = subagentPromise.Match(body)
				return nil
			}
			for line, text := range strings.Split(string(body), "\n") {
				if !subagentPromise.MatchString(text) {
					continue
				}
				promises++
				if !bundleHasCallableSubagents(wfs) {
					t.Errorf("%s:%d instructs subagent calls but %s has no node whose tools and permission gate admit them on every route: %s", path, line+1, slug, text)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if promises == 0 {
		t.Fatal("no subagent instruction found; the guard lost its subject")
	}
	if !attributionSeen {
		t.Error("the harness-attribution exception lost its quoted parallelism or its reference-only reason; remove or revisit it")
	}
	t.Logf("%d subagent instructions checked across %d bundles", promises, len(workflows))
}

func bundleHasCallableSubagents(wfs []*ir.Workflow) bool {
	for _, wf := range wfs {
		exec := model.NewProgramExecutor(wf)
		for _, node := range wf.Nodes {
			llm, ok := node.(ir.LLMNode)
			if !ok {
				continue
			}
			routes := bundlelint.NodeBackends(llm, wf)
			all := true
			for _, route := range routes {
				held := exec.EffectiveToolNamesOnRoutes(node, false, []string{route})
				switch route {
				case delegate.BackendClaudeCode:
					surface := delegate.ClaudeCodeToolSurface(delegate.Task{
						AllowedTools: held, ToolsDeclared: toolcatalog.ToolsDeclared(llm.GetTools()),
						Readonly: llm.GetLLMFields().Readonly,
					})
					for _, native := range []string{"Agent", "Task"} {
						if !slices.Contains(surface.Granted, native) || !bundlelint.PermissionGateLeavesOnRoute(llm, wf, route, native) {
							all = false
						}
					}
				case delegate.BackendClaw:
					if !slices.Contains(held, "agent") || !bundlelint.PermissionGateLeavesOnRoute(llm, wf, route, "agent") {
						all = false
					}
				default:
					all = false
				}
			}
			if all {
				return true
			}
		}
	}
	return false
}

func TestSubagentPromiseChecksAConditionalFallback(t *testing.T) {
	wf := compileBotFile(t, "whats-next/main.bot")
	node := wf.Nodes["nexie"].(*ir.AgentNode)
	node.Fallbacks = []ir.Fallback{{Backend: "pi", Model: "test/model", When: "vars.rescue == true"}}
	run := model.NewProgramExecutor(wf)
	run.SetVars(map[string]any{"rescue": true})
	if !slices.Contains(run.EffectiveFallbackBackends(node), "pi") {
		t.Fatal("the fixture did not activate its conditional fallback")
	}
	if bundleHasCallableSubagents([]*ir.Workflow{wf}) {
		t.Fatal("the guard accepted a subagent promise that its conditional pi route cannot serve")
	}
	// A skip never dispatches; a model-only fallback inherits the primary
	// backend. Neither introduces a second tool surface to reject.
	node.Fallbacks[0].Action = ir.FallbackActionSkip
	if !bundleHasCallableSubagents([]*ir.Workflow{wf}) {
		t.Fatal("a skipped fallback removed Nexie's callable subagent tool")
	}
	node.Fallbacks[0].Action = ""
	node.Fallbacks[0].Backend = ""
	if !bundleHasCallableSubagents([]*ir.Workflow{wf}) {
		t.Fatal("a model-only fallback did not inherit Nexie's backend")
	}
}

func TestSubagentPromiseChecksAConditionalEnvironmentFallback(t *testing.T) {
	t.Setenv("ITERION_TEST_SUBAGENT_BACKEND", "pi")
	wf := compileBotFile(t, "whats-next/main.bot")
	node := wf.Nodes["nexie"].(*ir.AgentNode)
	node.Fallbacks = []ir.Fallback{{Backend: "${ITERION_TEST_SUBAGENT_BACKEND:-claude_code}", Model: "test/model", When: "vars.rescue == true"}}
	run := model.NewProgramExecutor(wf)
	run.SetVars(map[string]any{"rescue": true})
	if !slices.Contains(run.EffectiveFallbackBackends(node), "pi") {
		t.Fatal("the fixture did not resolve its active fallback to pi")
	}
	if bundleHasCallableSubagents([]*ir.Workflow{wf}) {
		t.Fatal("the guard used the source default instead of the conditional fallback's resolved backend")
	}
}
