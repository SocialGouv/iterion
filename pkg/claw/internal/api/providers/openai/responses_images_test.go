package openai

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestConvertMessagesToResponsesInput_CarriesImages(t *testing.T) {
	png := api.ContentBlock{Type: "image", Source: &api.ImageSource{Type: "base64", MediaType: "image/png", Data: "QUJD"}}
	input, err := convertMessagesToResponsesInput([]api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "look"}, png}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "call_1", Name: "read_image", Input: map[string]any{"path": "s.png"}}}},
		{Role: "user", Content: []api.ContentBlock{{
			Type: "tool_result", ToolUseID: "call_1",
			Content: []api.ContentBlock{{Type: "text", Text: "s.png"}, png},
		}}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("want user, function_call, function_call_output, user; got %s", body)
	}
	first := items[0]["content"].([]any)
	if img := first[1].(map[string]any); img["type"] != "input_image" || img["image_url"] != "data:image/png;base64,QUJD" {
		t.Errorf("user image part = %v", img)
	}
	out := items[2]
	if out["type"] != "function_call_output" || !strings.Contains(out["output"].(string), "1 image(s)") || strings.Contains(out["output"].(string), "QUJD") {
		t.Errorf("function_call_output = %v, want the text and the notice, not the bytes", out)
	}
	follow := items[3]["content"].([]any)
	if len(follow) != 2 || follow[1].(map[string]any)["image_url"] != "data:image/png;base64,QUJD" {
		t.Errorf("follow-up user content = %v, want caption + input_image", follow)
	}
}

func TestConvertMessagesToResponsesInput_RefusesAnImageWithoutData(t *testing.T) {
	_, err := convertMessagesToResponsesInput([]api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "image"}}}})
	var invalid *api.RequestValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v (%T), want *api.RequestValidationError for an image block with no source", err, err)
	}
}
