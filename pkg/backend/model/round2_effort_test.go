package model

import (
	"bytes"
	"context"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"strings"
	"testing"
)

func TestR2RouteAcceptedReasoningEfforts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(compatgw.CatalogModelsEnv, `{"deepseek":{"context_window":100000}}`)
	t.Setenv("OPENAI_COMPATIBLE_REASONING_EFFORTS", `{"deepseek":["low","high","xhigh","max"]}`)
	t.Setenv(compatgw.ResolvedEnvName, "")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "http://127.0.0.1:8081/v1")
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "test-key")
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
	mock := newMockClient(toolUseEvents("ok", "structured_output", `{"verdict":true,"reason":"ok"}`, 10, 10))
	reg := NewRegistry()
	reg.Register("openai_compatible", func(string) (api.APIClient, error) { return mock, nil })
	var logs bytes.Buffer
	emitter := &captureEmitter{}
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithLogger(iterlog.New(iterlog.LevelDebug, &logs)), WithEventHooks(NewStoreEventHooks(context.Background(), emitter, "effort", iterlog.Nop(), nil, nil)))
	node := verdictJudge("claw")
	node.Model = "openai_compatible/deepseek"
	node.ReasoningEffort = "medium"
	_, err := exec.Execute(context.Background(), node, nil)
	if err != nil {
		t.Fatal(err)
	}
	warnings := 0
	for _, event := range emitter.events {
		if event.Type == "reasoning_effort_adjusted" {
			warnings++
			if event.Data["requested"] != "medium" || event.Data["applied"] != "high" || event.Data["reason"] == "" {
				t.Errorf("incorrect warning: %v", event.Data)
			}
		}
	}
	if warnings != 1 {
		t.Errorf("warning events=%d", warnings)
	}
	calls := mock.getCalls()
	if len(calls) != 1 || calls[0].ReasoningEffort != "high" {
		t.Fatalf("unsupported effort sent: %+v", calls)
	}
	for _, marker := range []string{"reasoning_effort_adjusted", "medium", "high"} {
		if !strings.Contains(logs.String(), marker) {
			t.Errorf("missing warning %q: %s", marker, logs.String())
		}
	}
}
