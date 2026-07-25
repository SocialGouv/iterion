package server

import (
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
