package openaiwire

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func pngBlock(data string) api.ContentBlock {
	return api.ContentBlock{Type: "image", Source: &api.ImageSource{Type: "base64", MediaType: "image/png", Data: data}}
}

func wireJSON(t *testing.T, msgs []Message) []map[string]any {
	t.Helper()
	body, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return out
}

func roles(msgs []map[string]any) string {
	r := make([]string, len(msgs))
	for i, m := range msgs {
		r[i] = m["role"].(string)
	}
	return strings.Join(r, ",")
}

func TestConvertMessages_UserImageBecomesImageURLPart(t *testing.T) {
	msgs, err := ConvertMessages("", []api.Message{{Role: "user", Content: []api.ContentBlock{
		{Type: "text", Text: "Read the labels."},
		pngBlock("QUJD"),
		{Type: "image", Source: &api.ImageSource{Type: "url", URL: "https://example.test/a.png"}},
	}}})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := wireJSON(t, msgs)
	if len(got) != 1 {
		t.Fatalf("want 1 message, got %d: %v", len(got), got)
	}
	parts, ok := got[0]["content"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("content is not a 3-part array: %#v", got[0]["content"])
	}
	text := parts[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "Read the labels." {
		t.Errorf("part 0 = %v, want the text part", text)
	}
	b64 := parts[1].(map[string]any)
	if b64["type"] != "image_url" || b64["image_url"].(map[string]any)["url"] != "data:image/png;base64,QUJD" {
		t.Errorf("part 1 = %v, want a data URL image part", b64)
	}
	url := parts[2].(map[string]any)
	if url["image_url"].(map[string]any)["url"] != "https://example.test/a.png" {
		t.Errorf("part 2 = %v, want the source URL", url)
	}
}

func TestConvertMessages_TextOnlyWireIsUnchanged(t *testing.T) {
	msgs, err := ConvertMessages("sys <rules> & more", []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "a<b"}, {Type: "text", Text: "x && y > z"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "c1", Name: "bash", Input: map[string]any{}}}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	// The request marshaller, not json.Marshal: the body a provider receives.
	body, err := MarshalJSON(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"role":"system","content":"sys <rules> & more"},{"role":"user","content":"a<b\nx && y > z"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]}]`
	if string(body) != want {
		t.Errorf("wire =\n%s\nwant\n%s", body, want)
	}
}

func TestConvertMessages_ToolResultImageRidesInBridgedUserMessage(t *testing.T) {
	msgs, err := ConvertMessages("", []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "open it"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "aB3dE5gH9", Name: "read_image", Input: map[string]any{"path": "s.png"}}}},
		{Role: "user", Content: []api.ContentBlock{{
			Type: "tool_result", ToolUseID: "aB3dE5gH9",
			Content: []api.ContentBlock{{Type: "text", Text: "s.png"}, pngBlock("QUJD")},
		}}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := wireJSON(t, msgs)
	if r := roles(got); r != "user,assistant,tool,assistant,user" {
		t.Fatalf("roles = %s, want user,assistant,tool,assistant,user", r)
	}
	tool := got[2]
	if tool["tool_call_id"] != "aB3dE5gH9" {
		t.Errorf("tool_call_id = %v", tool["tool_call_id"])
	}
	toolText, ok := tool["content"].(string)
	if !ok || !strings.HasPrefix(toolText, "s.png\n") || !strings.Contains(toolText, "1 image(s)") {
		t.Errorf("tool content = %#v, want the text then the image notice", tool["content"])
	}
	if strings.Contains(toolText, "QUJD") {
		t.Errorf("tool content carries the image bytes as text: %q", toolText)
	}
	if got[3]["content"] != toolResultsBridge {
		t.Errorf("bridge = %v", got[3]["content"])
	}
	parts, ok := got[4]["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("user content = %#v, want caption + image", got[4]["content"])
	}
	if c := parts[0].(map[string]any); c["type"] != "text" || !strings.Contains(c["text"].(string), "aB3dE5gH9") {
		t.Errorf("caption = %v", c)
	}
	if img := parts[1].(map[string]any); img["image_url"].(map[string]any)["url"] != "data:image/png;base64,QUJD" {
		t.Errorf("image = %v", img)
	}
}

func TestConvertMessages_UserTextAfterToolIsBridged(t *testing.T) {
	msgs, err := ConvertMessages("", []api.Message{
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "c1", Name: "bash", Input: map[string]any{}}}},
		{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: "c1", Content: "ok"}.ToContentBlock()}},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "operator note"}}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := wireJSON(t, msgs)
	if r := roles(got); r != "assistant,tool,assistant,user" {
		t.Fatalf("roles = %s, want assistant,tool,assistant,user", r)
	}
	if got[3]["content"] != "operator note" {
		t.Errorf("user content = %#v, want the plain string", got[3]["content"])
	}
}

func TestConvertMessages_ToolResultsWithoutFollowUpAreNotBridged(t *testing.T) {
	msgs, err := ConvertMessages("", []api.Message{
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "c1", Name: "bash", Input: map[string]any{}}}},
		{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: "c1", Content: "ok"}.ToContentBlock()}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if r := roles(wireJSON(t, msgs)); r != "assistant,tool" {
		t.Fatalf("roles = %s, want assistant,tool", r)
	}
}

func TestConvertMessages_RefusesAnImageWithoutData(t *testing.T) {
	for name, block := range map[string]api.ContentBlock{
		"no source":     {Type: "image"},
		"empty base64":  {Type: "image", Source: &api.ImageSource{Type: "base64", MediaType: "image/png"}},
		"no media type": {Type: "image", Source: &api.ImageSource{Type: "base64", Data: "QUJD"}},
		"empty url":     {Type: "image", Source: &api.ImageSource{Type: "url"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ConvertMessages("", []api.Message{{Role: "user", Content: []api.ContentBlock{block}}}); err == nil {
				t.Error("user image: want an error, got none")
			}
			inTool := api.ContentBlock{Type: "tool_result", ToolUseID: "c1", Content: []api.ContentBlock{block}}
			if _, err := ConvertMessages("", []api.Message{{Role: "user", Content: []api.ContentBlock{inTool}}}); err == nil {
				t.Error("tool image: want an error, got none")
			}
		})
	}
}

// A message injected after a tool image (a runtime reminder, an operator
// message) joins the image carrier: two user messages in a row break
// templates that require strict role alternation.
func TestConvertMessages_InjectedUserMessageJoinsTheToolImageCarrier(t *testing.T) {
	msgs, err := ConvertMessages("", []api.Message{
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "c1", Name: "read_image", Input: map[string]any{}}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "tool_result", ToolUseID: "c1", Content: []api.ContentBlock{pngBlock("QUJD")}}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "operator note"}}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := wireJSON(t, msgs)
	if r := roles(got); r != "assistant,tool,assistant,user" {
		t.Fatalf("roles = %s, want assistant,tool,assistant,user", r)
	}
	parts := got[3]["content"].([]any)
	if len(parts) != 4 || parts[1].(map[string]any)["type"] != "image_url" ||
		parts[2].(map[string]any)["text"] != UserContentCaption || parts[3].(map[string]any)["text"] != "operator note" {
		t.Errorf("carrier = %v, want caption, image, the user caption, then the injected text", parts)
	}
}

// A user image that follows a tool image in a separate message is captioned
// as the user's own: the carrier does not let it read as the tool's.
func TestConvertMessages_AUserImageAfterAToolImageHasItsOwnCaption(t *testing.T) {
	msgs, err := ConvertMessages("", []api.Message{
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "c1", Name: "read_image", Input: map[string]any{}}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "tool_result", ToolUseID: "c1", Content: []api.ContentBlock{pngBlock("QUJD")}}}},
		{Role: "user", Content: []api.ContentBlock{pngBlock("VVNS")}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := wireJSON(t, msgs)
	parts := got[len(got)-1]["content"].([]any)
	want := []string{"text", "image_url", "text", "image_url"}
	if len(parts) != len(want) {
		t.Fatalf("carrier = %v, want tool caption, tool image, user caption, user image", parts)
	}
	for i, w := range want {
		if parts[i].(map[string]any)["type"] != w {
			t.Fatalf("carrier part %d = %v, want %s", i, parts[i], w)
		}
	}
	if parts[2].(map[string]any)["text"] != UserContentCaption {
		t.Errorf("separator = %v, want %q", parts[2], UserContentCaption)
	}
	if parts[3].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,VVNS" {
		t.Errorf("user image = %v", parts[3])
	}
}

// Tool results of one assistant turn spread over several input messages stay
// contiguous on the wire: the image carrier waits for the end of the run, so
// no tool message is cut off from its tool_calls.
func TestConvertMessages_ToolResultsSplitAcrossMessagesStayContiguous(t *testing.T) {
	msgs, err := ConvertMessages("", []api.Message{
		{Role: "assistant", Content: []api.ContentBlock{
			{Type: "tool_use", ID: "c1", Name: "read_image", Input: map[string]any{}},
			{Type: "tool_use", ID: "c2", Name: "bash", Input: map[string]any{}},
		}},
		{Role: "user", Content: []api.ContentBlock{{Type: "tool_result", ToolUseID: "c1", Content: []api.ContentBlock{pngBlock("QUJD")}}}},
		{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: "c2", Content: "ok"}.ToContentBlock()}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "done"}}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := wireJSON(t, msgs)
	if r := roles(got); r != "assistant,tool,tool,assistant,user,assistant" {
		t.Fatalf("roles = %s, want assistant,tool,tool,assistant,user,assistant", r)
	}
	if got[1]["tool_call_id"] != "c1" || got[2]["tool_call_id"] != "c2" {
		t.Errorf("tool messages = %v, %v", got[1], got[2])
	}
}

func TestConvertMessages_AnUnencodableImageIsARequestValidationError(t *testing.T) {
	_, err := ConvertMessages("", []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "image"}}}})
	var invalid *api.RequestValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v (%T), want *api.RequestValidationError", err, err)
	}
	if invalid.IsRetryable() {
		t.Error("a request that cannot be encoded must not be retried")
	}
}
