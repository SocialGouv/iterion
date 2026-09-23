package bots

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The loop gate hands ONE string to the next pass: `fail_log`. In the four bots
// that run an in-loop adversarial review, it was
//
//	if(verify_run.passed, if(review.clean, '', review.findings), verify_run.log_tail)
//
// which reads the review's findings only on the branch where the build passed.
// A missing verify.sh is now a refusal (#1598), so `passed` is false on exactly
// the path where the review still runs — and the review's output was dropped,
// every pass, until the loop exhausted itself. The bot paid for a review node
// per pass and threw the result away.
//
// This test EVALUATES the expression rather than grepping it, because that is
// the lesson this very field already taught the repo: `fail_log` lives inside
// `if(converged, …, <the failure branch>)`, it parses and validates clean, and
// it only runs once something else has already gone wrong — see
// TestCatalogExprConcatIsArrayOnly, written after a `concat('str', …)` shipped
// in a refusal path and died on first contact.
func TestGateFailLogCarriesTheReviewOnASkippedBuild(t *testing.T) {
	type gate struct {
		rel  string
		node string
		ast  *expr.AST
	}

	mains, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	var gates []gate
	for _, rel := range mains {
		pr := parseBotUnit(rel)
		if pr.File == nil {
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			continue
		}
		for node, raw := range cr.Workflow.Nodes {
			cn, ok := raw.(*ir.ComputeNode)
			if !ok {
				continue
			}
			for _, e := range cn.Exprs {
				// The class is "a gate that folds a review verdict into fail_log",
				// named by what the expression READS, not by the node's name.
				if e == nil || e.Key != "fail_log" || e.AST == nil {
					continue
				}
				if !strings.Contains(e.Raw, "outputs.review.findings") {
					continue
				}
				gates = append(gates, gate{rel: rel, node: node, ast: e.AST})
			}
		}
	}
	sort.Slice(gates, func(i, j int) bool {
		if gates[i].rel != gates[j].rel {
			return gates[i].rel < gates[j].rel
		}
		return gates[i].node < gates[j].node
	})
	if len(gates) < 4 {
		t.Fatalf("found %d gates folding a review verdict into fail_log, want >= 4 — the discovery "+
			"is stale and this guard proves nothing", len(gates))
	}

	// outputs.<node>.<field>, as the runtime supplies them.
	ctxFor := func(passed, skipped, clean bool, logTail, findings string) *expr.Context {
		return &expr.Context{
			Outputs: func(path []string) any {
				if len(path) < 2 {
					return nil
				}
				switch path[0] + "." + path[1] {
				case "verify_run.passed":
					return passed
				case "verify_run.skipped":
					return skipped
				case "verify_run.log_tail":
					return logTail
				case "review.clean":
					return clean
				case "review.findings":
					return findings
				}
				return nil
			},
		}
	}

	for _, g := range gates {
		g := g
		t.Run(g.rel+"/"+g.node, func(t *testing.T) {
			// The case the refusal creates: nothing was built, so the review ran
			// and has something to say. Both must survive to the next pass.
			got, err := g.ast.Eval(ctxFor(false, true, false, "NO VERIFY SCRIPT: the refusal", "THE REVIEW FINDING"))
			if err != nil {
				t.Fatalf("fail_log does not evaluate on the skipped path: %v — this field only runs "+
					"when something has already gone wrong, so a type error here turns a reported "+
					"failure into a dead run", err)
			}
			s, ok := got.(string)
			if !ok {
				t.Fatalf("fail_log evaluated to %T, want string", got)
			}
			if !strings.Contains(s, "NO VERIFY SCRIPT") {
				t.Errorf("fail_log = %q — the refusal itself is gone, so the agent is not told the "+
					"gate never ran", s)
			}
			if !strings.Contains(s, "THE REVIEW FINDING") {
				t.Errorf("fail_log = %q — the review ran and its findings were dropped; the bot pays "+
					"for a review node every pass and discards the result", s)
			}
			// The two parts are read by an agent. A separator that arrives as the
			// two characters backslash-n instead of a newline is the escaping
			// going one layer wrong, and it shows up in the prompt verbatim.
			if !strings.Contains(s, "refusal\n") {
				t.Errorf("fail_log = %q — the refusal and the findings are not separated by a real "+
					"newline; the escape resolved to something else", s)
			}

			// And the ordinary red build, where there is no review verdict to fold
			// in: the build log must not be diluted.
			red, err := g.ast.Eval(ctxFor(false, false, false, "BUILD LOG", "SHOULD NOT APPEAR"))
			if err != nil {
				t.Fatalf("fail_log does not evaluate on the red-build path: %v", err)
			}
			rs, _ := red.(string)
			if !strings.Contains(rs, "BUILD LOG") {
				t.Errorf("fail_log = %q on a red build, want the build log", rs)
			}
			if strings.Contains(rs, "SHOULD NOT APPEAR") {
				t.Errorf("fail_log = %q on a red build — the review skipped itself there (the tree "+
					"is mid-implementation), so folding its verdict in reports a stale opinion", rs)
			}
		})
	}
}
