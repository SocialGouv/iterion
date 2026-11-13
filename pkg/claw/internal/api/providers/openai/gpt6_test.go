package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestGPT6AlwaysUsesResponses(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "openai/gpt-6-astra"} {
		for _, effort := range []string{"", "high"} {
			for _, tools := range [][]api.Tool{nil, {{Name: "read_file"}}} {
				if !shouldUseResponsesAPI(api.CreateMessageRequest{Model: model, ReasoningEffort: effort, Tools: tools}) {
					t.Errorf("%s effort=%q tools=%d must use Responses", model, effort, len(tools))
				}
			}
		}
	}
	if shouldUseResponsesAPI(api.CreateMessageRequest{Model: "gpt-4.1"}) {
		t.Fatal("legacy model route changed")
	}
}

func TestGPT6HTTPDispatchAndPayload(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"} {
		for _, oauth := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/oauth=%t/explicit=%t", model, oauth, explicit), func(t *testing.T) {
					called := false
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						called = true
						wantPath := "/v1/responses"
						if oauth {
							wantPath = "/responses"
						}
						if r.URL.Path != wantPath {
							t.Errorf("path=%s, want %s", r.URL.Path, wantPath)
						}
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if body["model"] != model {
							t.Errorf("model=%v", body["model"])
						}
						for _, key := range []string{"temperature", "top_p", "max_tokens", "max_completion_tokens"} {
							if _, ok := body[key]; ok {
								t.Errorf("unsupported field %s", key)
							}
						}
						if oauth {
							if body["store"] != false {
								t.Error("OAuth must set store=false")
							}
							if _, ok := body["max_output_tokens"]; ok {
								t.Error("OAuth rejects max_output_tokens")
							}
						} else if body["max_output_tokens"] != float64(200) {
							t.Errorf("output cap=%v", body["max_output_tokens"])
						}
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
					}))
					defer srv.Close()
					client := &Client{Model: model, BaseURL: srv.URL, APIKey: "test-key", HTTPClient: srv.Client()}
					if oauth {
						client.AuthMode = AuthModeChatGPTOAuth
						client.OAuthToken = "test-token"
						client.ChatGPTAccountID = "test-account"
					}
					temp := 0.2
					req := api.CreateMessageRequest{MaxTokens: 200, Temperature: &temp, TopP: &temp, Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hello"}}}}}
					if explicit {
						req.Model = "openai/" + model
					}
					ch, err := client.StreamResponse(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
					for range ch {
					}
					if !called {
						t.Fatal("no request observed")
					}
				})
			}
		}
	}
}
