package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func compileSource(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	pr := parser.Parse("test.bot", src)
	if pr.File == nil {
		t.Fatalf("parse failed: %+v", pr.Diagnostics)
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatalf("compile failed: %+v", cr.Diagnostics)
	}
	return cr.Workflow
}

func TestResolveKnob(t *testing.T) {
	cases := []struct {
		name                string
		workflow, env, def  string
		wantValue, wantFrom string
	}{
		{"workflow wins", "ask", "deny", "off", "ask", "workflow"},
		{"env below workflow", "", "deny", "off", "deny", "env"},
		{"default when all unset", "", "", "off", "off", "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveKnob(tc.workflow, tc.env, tc.def)
			if got.Effective != tc.wantValue || got.Source != tc.wantFrom {
				t.Fatalf("got %+v, want (%q, %q)", got, tc.wantValue, tc.wantFrom)
			}
			if got.NodePinned {
				t.Fatal("resolveKnob never sets NodePinned (caller's business)")
			}
		})
	}
}

func TestBuildEffectiveSettings(t *testing.T) {
	t.Setenv("ITERION_COMPRESS", "")
	t.Setenv("ITERION_PERMISSION", "")
	t.Setenv("ITERION_DEFAULT_BACKEND", "")

	t.Run("nil workflow", func(t *testing.T) {
		if got := buildEffectiveSettings(nil); got != nil {
			t.Fatalf("expected nil for nil workflow, got %+v", got)
		}
	})

	t.Run("workflow-level knobs + node pins", func(t *testing.T) {
		wf := compileSource(t, `agent draft:
  model: "anthropic/claude-sonnet-4-6"
  backend: "claw"

judge verify:
  model: "openai/gpt-5.4-mini"
  compress: ultra

workflow main:
  compress: on
  permission: ask
  draft -> verify
  verify -> done
`)
		eff := buildEffectiveSettings(wf)
		if eff == nil {
			t.Fatal("expected settings, got nil")
		}
		if eff.Compress.Effective != "on" || eff.Compress.Source != "workflow" {
			t.Fatalf("compress = %+v, want on/workflow", eff.Compress)
		}
		if !eff.Compress.NodePinned {
			t.Fatal("judge pins compress: ultra → NodePinned expected")
		}
		if eff.Permission.Effective != "ask" || eff.Permission.Source != "workflow" {
			t.Fatalf("permission = %+v, want ask/workflow", eff.Permission)
		}
		if eff.Permission.NodePinned {
			t.Fatal("no node pins permission")
		}
		if eff.Backend.Effective != "auto" || eff.Backend.Source != "default" {
			t.Fatalf("backend = %+v, want auto/default", eff.Backend)
		}
		if !eff.Backend.NodePinned {
			t.Fatal("agent pins backend: claw → NodePinned expected")
		}
	})

	t.Run("env layer below workflow", func(t *testing.T) {
		t.Setenv("ITERION_PERMISSION", "deny")
		wf := compileSource(t, `agent draft:
  model: "anthropic/claude-sonnet-4-6"

workflow main:
  draft -> done
`)
		eff := buildEffectiveSettings(wf)
		if eff.Permission.Effective != "deny" || eff.Permission.Source != "env" {
			t.Fatalf("permission = %+v, want deny/env", eff.Permission)
		}
		if eff.Compress.Effective != "auto" || eff.Compress.Source != "default" {
			t.Fatalf("compress = %+v, want auto/default", eff.Compress)
		}
	})
}

func TestBuildBackendOverrideOptionsMixedPermissions(t *testing.T) {
	t.Setenv("ITERION_PERMISSION", "")
	t.Setenv("ITERION_DEFAULT_BACKEND", "")
	wf := compileSource(t, `agent gated:
  model: "anthropic/claude-sonnet-4-6"
  backend: "claw"
  tools: [read_file]

agent open:
  model: "anthropic/claude-sonnet-4-6"
  backend: "claw"
  permission: off

workflow main:
  permission: deny
  gated -> open
  open -> done
`)

	choices := buildBackendOverrideOptions(
		wf,
		"",
		"",
		[]string{"claw", "claude_code", "codex"},
	)
	if got := choices["gated"]["codex"].UnavailableReason; got == "" {
		t.Fatal("gated node presents codex as selectable")
	}
	if got := choices["open"]["codex"].UnavailableReason; got != "" {
		t.Fatalf("permission: off node rejected codex: %s", got)
	}
	if got := choices["gated"]["claude_code"].Warning; !strings.Contains(got, "tools:") {
		t.Fatalf("claw → claude_code warning = %q, want tools: restriction loss", got)
	}

	// Run-level off wins over BOTH the node and workflow declarations.
	choices = buildBackendOverrideOptions(wf, "off", "", []string{"codex"})
	for _, nodeID := range []string{"gated", "open"} {
		if got := choices[nodeID]["codex"].UnavailableReason; got != "" {
			t.Errorf("run permission: off did not win for %s: %s", nodeID, got)
		}
	}

	// Conversely, a run-level gate wins over the node's explicit off.
	choices = buildBackendOverrideOptions(wf, "ask", "", []string{"codex"})
	for _, nodeID := range []string{"gated", "open"} {
		if got := choices[nodeID]["codex"].UnavailableReason; got == "" {
			t.Errorf("run permission: ask did not gate %s", nodeID)
		}
	}
}

func TestBuildBackendOverrideOptionsRequiresPauseForAskRules(t *testing.T) {
	t.Setenv("ITERION_PERMISSION", "")
	agent := &ir.AgentNode{}
	agent.ID = "work"
	wf := &ir.Workflow{
		Permission: "deny",
		Nodes:      map[string]ir.Node{"work": agent},
	}
	wf.PermissionAsk = []string{"Bash(git push:*)"}
	choices := buildBackendOverrideOptions(wf, "", "", []string{"grok"})
	if got := choices["work"]["grok"].UnavailableReason; !strings.Contains(got, "cannot pause") {
		t.Fatalf("grok assessment = %q, want explicit ask-rule refusal", got)
	}
}

func TestPreviewCost_EffectiveBlock(t *testing.T) {
	t.Setenv("ITERION_COMPRESS", "")
	t.Setenv("ITERION_PERMISSION", "")
	t.Setenv("ITERION_DEFAULT_BACKEND", "")
	_, hs := newTestServer(t)
	src := `agent draft:
  model: "anthropic/claude-sonnet-4-6"

workflow main:
  compress: off
  draft -> done
`
	got := postPreviewCost(t, hs.URL, `{"source":`+jsonString(src)+`}`)
	if got.Effective == nil {
		t.Fatal("expected effective block on preview-cost response")
	}
	if got.Effective.Compress.Effective != "off" || got.Effective.Compress.Source != "workflow" {
		t.Fatalf("compress = %+v, want off/workflow", got.Effective.Compress)
	}
	if got.Effective.Backend.Source != "default" {
		t.Fatalf("backend = %+v, want default source", got.Effective.Backend)
	}
}

func TestPreviewCost_BackendOptionsExposeMixedNodeSafety(t *testing.T) {
	t.Setenv("ITERION_PERMISSION", "")
	_, hs := newTestServer(t)
	src := `agent gated:
  model: "anthropic/claude-sonnet-4-6"

agent open:
  model: "anthropic/claude-sonnet-4-6"
  permission: off

workflow main:
  permission: deny
  gated -> open
  open -> done
`
	body := `{"source":` + jsonString(src) + `,"backend_names":["codex"]}`
	got := postPreviewCost(t, hs.URL, body)
	if got.BackendOptions["gated"]["codex"].UnavailableReason == "" {
		t.Fatal("wire response presents codex as safe for the gated node")
	}
	if reason := got.BackendOptions["open"]["codex"].UnavailableReason; reason != "" {
		t.Fatalf("wire response rejects codex for permission: off node: %s", reason)
	}
}
