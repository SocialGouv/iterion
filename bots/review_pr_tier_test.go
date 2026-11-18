package bots

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestReviewPRTierExpand pins the review-tier resolution (native:685 /
// SocialGouv/iterion#685) at the expression level: guard (the default)
// must reproduce the bot's pre-#685 hardcoded literals byte-for-byte, each
// tier must supply its documented preset, and an explicit (non-sentinel)
// value on any of the underlying vars must win over the tier — the tier is
// a PRESET, never a cage.
func TestReviewPRTierExpand(t *testing.T) {
	pr := parseBotUnit("review-pr/main.bot")
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
	sentinels := func(tier, reviewMode string) map[string]any {
		return map[string]any{
			"review_tier":        tier,
			"severity_threshold": "auto",
			"max_findings":       int64(0),
			"post_to_board":      "auto",
			"review_mode":        reviewMode,
			"publish_mode":       "",
		}
	}

	t.Run("guard reproduces the bot's pre-#685 literal defaults", func(t *testing.T) {
		out := eval(t, sentinels("guard", "single"))
		if out["severity_threshold"] != "medium" {
			t.Errorf("guard severity_threshold = %v, want medium (the bot's historical default)", out["severity_threshold"])
		}
		if out["max_findings"] != int64(15) {
			t.Errorf("guard max_findings = %v, want 15 (the bot's historical default)", out["max_findings"])
		}
		if out["post_to_board"] != true {
			t.Errorf("guard post_to_board = %v, want true (the bot's historical default)", out["post_to_board"])
		}
		if out["effective_review_mode"] != "single" {
			t.Errorf("guard effective_review_mode = %v, want single", out["effective_review_mode"])
		}
	})

	t.Run("glance is frugal", func(t *testing.T) {
		out := eval(t, sentinels("glance", "single"))
		if out["severity_threshold"] != "high" {
			t.Errorf("glance severity_threshold = %v, want high", out["severity_threshold"])
		}
		if out["max_findings"] != int64(5) {
			t.Errorf("glance max_findings = %v, want 5", out["max_findings"])
		}
		if out["post_to_board"] != false {
			t.Errorf("glance post_to_board = %v, want false", out["post_to_board"])
		}
		if out["effective_review_mode"] != "single" {
			t.Errorf("glance effective_review_mode = %v, want single (glance never forces paired)", out["effective_review_mode"])
		}
	})

	t.Run("audit is premium and forces paired regardless of review_mode", func(t *testing.T) {
		out := eval(t, sentinels("audit", "single"))
		if out["severity_threshold"] != "low" {
			t.Errorf("audit severity_threshold = %v, want low", out["severity_threshold"])
		}
		if out["max_findings"] != int64(40) {
			t.Errorf("audit max_findings = %v, want 40", out["max_findings"])
		}
		if out["post_to_board"] != true {
			t.Errorf("audit post_to_board = %v, want true", out["post_to_board"])
		}
		if out["effective_review_mode"] != "paired" {
			t.Errorf("audit effective_review_mode = %v, want paired EVEN THOUGH review_mode itself is %q", out["effective_review_mode"], "single")
		}
	})

	t.Run("an explicit review_mode=paired forces paired on every tier", func(t *testing.T) {
		out := eval(t, sentinels("guard", "paired"))
		if out["effective_review_mode"] != "paired" {
			t.Errorf("effective_review_mode = %v, want paired", out["effective_review_mode"])
		}
	})

	// The tier is a preset, not a cage: an explicit (non-sentinel) value on
	// any underlying var wins over the tier's own preset for that knob,
	// on every tier — glance is the most likely to be overridden per repo,
	// so it is the one exercised here.
	t.Run("explicit overrides win over the tier", func(t *testing.T) {
		vars := sentinels("glance", "single")
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

	// Evaluation mode (publish_mode=off) writes nowhere a team reads, the
	// board included: it is the one thing above an explicit post_to_board,
	// on every tier. Nothing else the tier resolves changes with it.
	t.Run("evaluation mode files no board issue, whatever post_to_board says", func(t *testing.T) {
		for _, tier := range []string{"guard", "glance", "audit"} {
			for _, board := range []string{"auto", "true", "false"} {
				vars := sentinels(tier, "single")
				vars["post_to_board"] = board
				published := eval(t, vars)
				vars["publish_mode"] = "off"
				out := eval(t, vars)
				if out["post_to_board"] != false {
					t.Errorf("tier %s, post_to_board=%s, publish_mode=off: post_to_board = %v, want false", tier, board, out["post_to_board"])
				}
				for _, key := range []string{"severity_threshold", "max_findings", "effective_review_mode"} {
					if out[key] != published[key] {
						t.Errorf("tier %s: publish_mode=off moved %s from %v to %v", tier, key, published[key], out[key])
					}
				}
			}
		}
	})
}
