package sessionboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The session-board evaluator sends its model's wire id whole: the routing
// prefix comes off once, in the model package. Red when the evaluator
// strips the prefix itself before handing the spec on.
func TestLLMEvaluator_SendsTheWireIDWhole(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "test-key")

	// The answer is not a board decision, so Evaluate fails after the
	// call; the request it sent is what this test reads.
	_, _, _ = NewLLMEvaluator("openai/meta-llama/Llama-3.3-70B").Evaluate(context.Background(), EvalInput{})

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the evaluator sent no request")
	}
	for _, m := range seen {
		if m != "meta-llama/Llama-3.3-70B" {
			t.Errorf("wire model %q, want meta-llama/Llama-3.3-70B", m)
		}
	}
}
