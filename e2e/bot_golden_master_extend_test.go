package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// extendStubs registers the deterministic shape of one extension pass for
// the scenario executor: the base is read, the campaign acts (or refuses),
// the verifier answers with what `report` returns for the pass. The compute
// nodes (extend_gate, extend_result) are the engine's own — they are what
// these tests pin.
func extendStubs(exec *scenarioExecutor, report func(pass int) map[string]any) {
	exec.on("extend_base", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"head": "0123456789abcdef", "dirty": "", "notice": "",
			"pending": []any{map[string]any{"id": "E-1", "lot": "L1"}}, "_tokens": 1,
		}, nil
	})
	pass := 0
	exec.on("extend_campaign", func(_ map[string]any) (map[string]any, error) {
		pass++
		return map[string]any{"summary": "acted what could be acted", "_tokens": 10}, nil
	})
	exec.on("extend_verify", func(_ map[string]any) (map[string]any, error) {
		out := map[string]any{
			"committed": true, "scope_clean": true, "out_of_scope": []any{},
			"additions_ok": true, "refused": []any{}, "no_new_requests": true,
			"still_pending": []any{}, "extended": 1, "log_tail": "", "_tokens": 1,
		}
		for k, v := range report(pass) {
			out[k] = v
		}
		return out, nil
	})
}

func runExtend(t *testing.T, exec *scenarioExecutor, runID string) *store.Run {
	t.Helper()
	wf := compileFixtureStubSafe(t, "golden-master/extend.bot")
	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Checkpoint == nil {
		t.Fatal("run has no checkpoint")
	}
	return run
}

// TestGoldenMasterExtend_PendingRequestIsNotConverged pins the conjunction
// the parent reads: a request this bot refused stays pending, and a pass
// that left one pending is NOT converged, whatever else went green — the
// parent's gate refuses on the pending term either way, and this verdict
// must never read greener than that gate. Measured on a live campaign: the
// child came back `converged: true, extended: 1` with its one request
// refused, and the parent announced "extensions acted by pure addition".
func TestGoldenMasterExtend_PendingRequestIsNotConverged(t *testing.T) {
	exec := newScenarioExecutor()
	extendStubs(exec, func(int) map[string]any {
		return map[string]any{
			"still_pending": []any{map[string]any{"id": "E-1", "lot": "L1"}},
			"refused":       []any{map[string]any{"id": "E-1", "lot": "L1"}},
			"extended":      0, "additions_ok": false,
			"log_tail": "no act block was appended by this run — a run that acted nothing extended nothing",
		}
	})
	run := runExtend(t, exec, "run-extend-pending")
	res, ok := run.Checkpoint.Outputs["extend_result"]
	if !ok {
		t.Fatal("checkpoint carries no extend_result output")
	}
	if res["converged"] != false {
		t.Fatalf("extend_result.converged = %v, want false with a request still pending", res["converged"])
	}
	if n, _ := res["notice"].(string); !strings.Contains(n, "REFUSED") {
		t.Fatalf("notice = %q, want the refusal named", n)
	}
	// The bounded repair loop ran its passes (1 + max_passes = 3), then the
	// honest verdict — never a green one.
	if got := exec.callCount("extend_campaign"); got != 3 {
		t.Fatalf("extend_campaign called %d times, want 3 (the first pass + max_passes repairs, then the verdict)", got)
	}
}

// TestGoldenMasterExtend_EverythingActedConverges: the green path — every
// request acted by this run, nothing pending, converged on the first pass.
func TestGoldenMasterExtend_EverythingActedConverges(t *testing.T) {
	exec := newScenarioExecutor()
	extendStubs(exec, func(int) map[string]any { return nil })
	run := runExtend(t, exec, "run-extend-green")
	res := run.Checkpoint.Outputs["extend_result"]
	if res["converged"] != true {
		t.Fatalf("extend_result.converged = %v, want true", res["converged"])
	}
	if got := exec.callCount("extend_campaign"); got != 1 {
		t.Fatalf("extend_campaign called %d times, want 1", got)
	}
}

// TestGoldenMasterExtend_GreenTermsWithAPendingRequestStillRefuse is the
// exact live shape: every other term green (the verifier certified an act
// already at the base) and one request pending — the term alone must keep
// the verdict red.
func TestGoldenMasterExtend_GreenTermsWithAPendingRequestStillRefuse(t *testing.T) {
	exec := newScenarioExecutor()
	extendStubs(exec, func(int) map[string]any {
		return map[string]any{"still_pending": []any{map[string]any{"id": "E-1"}}}
	})
	run := runExtend(t, exec, "run-extend-green-but-pending")
	if res := run.Checkpoint.Outputs["extend_result"]; res["converged"] != false {
		t.Fatalf("extend_result.converged = %v with a pending request, want false", res["converged"])
	}
}
