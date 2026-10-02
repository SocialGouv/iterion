package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// A claw step whose provider did not report its usage reaches the meter as
// such — through the persisted llm_step_finished event — and the attempt's
// end says it once per route, since every ledger booked it at a lower bound.
// Red when the event drops the key, when the meter ignores it, and when the
// runner does not say it.
func TestUnreportedUsage_ReachesTheMeterAndTheRunnerLog(t *testing.T) {
	usage := newMetricsEmitter(discardEmitter{}, metrics.New())
	hooks := model.NewStoreEventHooks(context.Background(), usage, "run", iterlog.Nop(), nil)
	hooks.OnLLMRequest("n", model.LLMRequestInfo{Model: "openai_compatible/glm-5"})
	hooks.OnLLMStepFinish("n", model.LLMStepInfo{Number: 1, InputTokens: 40, OutputTokens: 2, UsageUnreported: true})
	hooks.OnLLMStepFinish("n", model.LLMStepInfo{Number: 2, InputTokens: 50, OutputTokens: 3})

	var unreported int64
	for _, totals := range usage.RouteTotals() {
		unreported += totals.unreportedCalls
	}
	if unreported != 1 {
		t.Fatalf("the meter counted %d unreported call(s), want 1", unreported)
	}

	var logBuf bytes.Buffer
	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelWarn, &logBuf)}}
	r.recordOrgSpend(context.Background(), &queue.RunMessage{RunID: "run"}, usage)
	if out := logBuf.String(); strings.Count(out, "whose usage the provider did not report") != 1 || !strings.Contains(out, "made 1 LLM call(s)") {
		t.Errorf("runner log = %q, want one line naming the unreported call", out)
	}
}
