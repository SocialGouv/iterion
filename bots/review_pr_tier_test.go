package bots

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestReviewPRTierExpand pins the review-tier resolution (native:685 /
// SocialGouv/iterion#685) at the expression level: guard (the default)
// must reproduce the bot's pre-#685 hardcoded literals byte-for-byte, each
// tier must supply its documented preset, and an explicit (non-sentinel)
// value on any of the underlying vars must win over the tier — the tier is
// a PRESET, never a cage.
func TestReviewPRTierExpand(t *testing.T) {
	src, err := os.ReadFile("review-pr/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	pr := parser.Parse("review-pr/main.bot", string(src))
	if pr.File == nil {
		t.Fatal("parse produced no File")
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatal("compile produced no Workflow")
	}
	node, ok := cr.Workflow.Nodes["tier_expand"]
	if !ok {
		t.Fatal("workflow does not declare a tier_expand node")
	}
	cn, ok := node.(*ir.ComputeNode)
	if !ok {
		t.Fatalf("tier_expand is %T, want a compute node", node)
	}
	asts := make(map[string]*expr.AST, len(cn.Exprs))
	for _, ce := range cn.Exprs {
		asts[ce.Key] = ce.AST
	}
	for _, want := range []string{"severity_threshold", "max_findings", "post_to_board", "effective_review_mode"} {
		if asts[want] == nil {
			t.Fatalf("tier_expand does not compute %q", want)
		}
	}

	eval := func(t *testing.T, vars map[string]any) map[string]any {
		t.Helper()
		ctx := &expr.Context{Vars: func(path []string) any {
			if len(path) == 1 {
				return vars[path[0]]
			}
			return nil
		}}
		out := make(map[string]any, len(asts))
		for key, ast := range asts {
			v, err := ast.Eval(ctx)
			if err != nil {
				t.Fatalf("eval %q: %v", key, err)
			}
			out[key] = v
		}
		return out
	}

	// Base sentinel vars every scenario starts from — the "operator touched
	// nothing" case for each knob.
	sentinels := func(tier, monoFamily, reviewMode string) map[string]any {
		return map[string]any{
			"review_tier":        tier,
			"severity_threshold": "auto",
			"max_findings":       int64(0),
			"post_to_board":      "auto",
			"review_mode":        reviewMode,
			"mono_family":        monoFamily,
		}
	}

	t.Run("guard reproduces the bot's pre-#685 literal defaults", func(t *testing.T) {
		out := eval(t, sentinels("guard", "claude", "mono"))
		if out["severity_threshold"] != "medium" {
			t.Errorf("guard severity_threshold = %v, want medium (the bot's historical default)", out["severity_threshold"])
		}
		if out["max_findings"] != int64(15) {
			t.Errorf("guard max_findings = %v, want 15 (the bot's historical default)", out["max_findings"])
		}
		if out["post_to_board"] != true {
			t.Errorf("guard post_to_board = %v, want true (the bot's historical default)", out["post_to_board"])
		}
		if out["effective_review_mode"] != "mono" {
			t.Errorf("guard effective_review_mode = %v, want mono", out["effective_review_mode"])
		}
	})

	t.Run("glance is frugal", func(t *testing.T) {
		out := eval(t, sentinels("glance", "claude", "mono"))
		if out["severity_threshold"] != "high" {
			t.Errorf("glance severity_threshold = %v, want high", out["severity_threshold"])
		}
		if out["max_findings"] != int64(5) {
			t.Errorf("glance max_findings = %v, want 5", out["max_findings"])
		}
		if out["post_to_board"] != false {
			t.Errorf("glance post_to_board = %v, want false", out["post_to_board"])
		}
		if out["effective_review_mode"] != "mono" {
			t.Errorf("glance effective_review_mode = %v, want mono (glance never forces dual)", out["effective_review_mode"])
		}
	})

	t.Run("audit is premium and forces dual regardless of review_mode", func(t *testing.T) {
		out := eval(t, sentinels("audit", "claude", "mono"))
		if out["severity_threshold"] != "low" {
			t.Errorf("audit severity_threshold = %v, want low", out["severity_threshold"])
		}
		if out["max_findings"] != int64(40) {
			t.Errorf("audit max_findings = %v, want 40", out["max_findings"])
		}
		if out["post_to_board"] != true {
			t.Errorf("audit post_to_board = %v, want true", out["post_to_board"])
		}
		if out["effective_review_mode"] != "dual" {
			t.Errorf("audit effective_review_mode = %v, want dual EVEN THOUGH review_mode itself is %q", out["effective_review_mode"], "mono")
		}
	})

	t.Run("an explicit review_mode=dual forces dual on every tier", func(t *testing.T) {
		out := eval(t, sentinels("guard", "claude", "dual"))
		if out["effective_review_mode"] != "dual" {
			t.Errorf("effective_review_mode = %v, want dual", out["effective_review_mode"])
		}
	})

	// The tier is a preset, not a cage: an explicit (non-sentinel) value on
	// any underlying var wins over the tier's own preset for that knob,
	// on every tier — glance is the most likely to be overridden per repo,
	// so it is the one exercised here.
	t.Run("explicit overrides win over the tier", func(t *testing.T) {
		vars := sentinels("glance", "claude", "mono")
		vars["severity_threshold"] = "critical"
		vars["max_findings"] = int64(3)
		vars["post_to_board"] = "true"
		out := eval(t, vars)
		if out["severity_threshold"] != "critical" {
			t.Errorf("explicit severity_threshold=critical was overridden by the tier: got %v", out["severity_threshold"])
		}
		if out["max_findings"] != int64(3) {
			t.Errorf("explicit max_findings=3 was overridden by the tier: got %v", out["max_findings"])
		}
		if out["post_to_board"] != true {
			t.Errorf("explicit post_to_board=true was overridden by the tier: got %v", out["post_to_board"])
		}
	})
}
