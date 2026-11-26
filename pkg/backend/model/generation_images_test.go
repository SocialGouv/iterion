package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// oversizedCapture is an image tool's output carrying one image of n bytes
// of base64 that no decoder reads, so no reduction can make it fit.
func oversizedCapture(n int) string {
	return `{"description":"capture","blocks":[{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"` + strings.Repeat("A", n) + `"}}]}`
}

// blockText joins a tool_result's text blocks.
func blockText(b api.ContentBlock) string {
	var parts []string
	for _, c := range b.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func imageTool(output string) GenerationTool {
	return GenerationTool{
		Name:        "screenshot",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return output, nil },
		imageResult: true,
	}
}

// The per-image bound is the route's: GenerateTextDirect resolves the policy
// from the spec and the tool door shapes the result under it. A 6 MiB image
// is over Anthropic's 5 MiB and refused there as a recoverable tool error,
// and reaches the model whole on an OpenAI-compatible gateway.
func TestGenerateTextDirect_ToolImagesMeetTheRoutesPerImageBound(t *testing.T) {
	t.Parallel()
	output := oversizedCapture(6 << 20)
	for spec, wantImage := range map[string]bool{
		"anthropic/claude-sonnet-4-6":   false,
		"openai_compatible/pixtral-12b": true,
	} {
		client := newMockClient(toolUseEvents("shot-1", "screenshot", `{}`, 10, 5), textEvents("done", 10, 5))
		_, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
			Model:    spec,
			Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "look"}}}},
			Tools:    []GenerationTool{imageTool(output)},
			MaxSteps: 3,
		})
		if err != nil {
			t.Fatalf("%s: generate: %v", spec, err)
		}
		calls := client.getCalls()
		if len(calls) != 2 {
			t.Fatalf("%s: %d requests, want 2", spec, len(calls))
		}
		msgs := calls[1].Messages
		result := msgs[len(msgs)-1].Content[0]
		hasImage := false
		for _, c := range result.Content {
			hasImage = hasImage || c.Type == "image"
		}
		if hasImage != wantImage || result.IsError == wantImage {
			t.Errorf("%s: tool result is_error=%t image=%t, want image=%t", spec, result.IsError, hasImage, wantImage)
		}
		if !wantImage && !strings.Contains(blockText(result), "lower the display resolution") {
			t.Errorf("%s: refusal = %q, want the capture's own advice", spec, blockText(result))
		}
	}
}

// A program calling an image tool receives the description and the marker —
// never the image bytes, and never the route's per-image refusal: the image
// is not sent anywhere. The model's own call, on the same door, still meets
// the bound.
func TestAScriptsImageToolCallReadsTextOnly(t *testing.T) {
	t.Parallel()
	opts := GenerationOptions{Model: "anthropic/claude-sonnet-4-6"}
	ctx, err := withRouteImagePolicy(context.Background(), &opts)
	if err != nil {
		t.Fatal(err)
	}
	gt := imageTool(oversizedCapture(6 << 20))
	d, dctx := newToolDispatcher(ctx, map[string]*GenerationTool{"screenshot": &gt}, nil, nil, nil, nil, nil)

	direct, abort := d.dispatch(dctx, toolUseBlock{ID: "direct", Name: "screenshot", PartialJSON: `{}`})
	if abort != nil || !direct.IsError {
		t.Fatalf("the model's own call to an over-bound capture: abort=%v is_error=%t, want a recoverable error", abort, direct.IsError)
	}

	srv, err := newScriptToolServer(dctx, "")
	if err != nil {
		t.Fatal(err)
	}
	out, abort := srv.CallTool(programCall(1), "screenshot", json.RawMessage(`{}`))
	if abort != nil {
		t.Fatalf("unexpected abort: %v", abort)
	}
	if out.IsError || out.Text != "capture\n"+tool.TextOnlyImageMarker {
		t.Errorf("the program read is_error=%t %q, want the description then the marker", out.IsError, out.Text)
	}
	if strings.Contains(out.Text, "AAAA") || len(out.Structured) > 0 {
		t.Errorf("the program received image bytes: text %d bytes, structured %d bytes", len(out.Text), len(out.Structured))
	}
}

func toolImageResult(id string, n int) api.ContentBlock {
	return api.ContentBlock{Type: "tool_result", ToolUseID: id, Content: []api.ContentBlock{
		{Type: "text", Text: id},
		{Type: "image", Source: &api.ImageSource{Type: "base64", MediaType: "image/png", Data: strings.Repeat("B", n)}},
	}}
}

// Over the route's per-request budget, the OLDEST tool images are replaced
// by a marker until the request fits; the prompt's image and the newest
// tool image stay, and the conversation itself keeps every image.
func TestBuildRequest_BoundsTheRequestsImagesOldestToolImageFirst(t *testing.T) {
	t.Parallel()
	prompt := api.ContentBlock{Type: "image", Source: &api.ImageSource{Type: "base64", MediaType: "image/png", Data: strings.Repeat("P", 10)}}
	messages := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "look"}, prompt}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "old", Name: "screenshot"}}},
		{Role: "user", Content: []api.ContentBlock{toolImageResult("old", 60)}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "new", Name: "screenshot"}}},
		{Role: "user", Content: []api.ContentBlock{toolImageResult("new", 60)}},
	}
	opts := GenerationOptions{Model: "openai_compatible/pixtral-12b", images: tool.ImagePolicy{MaxRequestImageBytes: 100}}
	req, err := buildRequest(opts, messages, nil, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	old, newest := req.Messages[2].Content[0].Content[1], req.Messages[4].Content[0].Content[1]
	if old.Type != "text" || !strings.Contains(old.Text, "call the tool again") || !strings.Contains(old.Text, tool.EnvRequestImageMaxBytes) {
		t.Errorf("the oldest tool image = %+v, want the omission marker", old)
	}
	if newest.Type != "image" || req.Messages[0].Content[1].Type != "image" {
		t.Errorf("the newest tool image (%s) or the prompt's image (%s) was dropped", newest.Type, req.Messages[0].Content[1].Type)
	}
	if messages[2].Content[0].Content[1].Type != "image" {
		t.Error("the conversation lost its image: the bound must shape the request only")
	}
}

// The prompt's images are the operator's: none is dropped, and when they
// alone exceed the budget the request is refused with a typed error naming
// the escape hatch.
func TestBuildRequest_RefusesPromptImagesOverTheBudget(t *testing.T) {
	t.Parallel()
	messages := []api.Message{{Role: "user", Content: []api.ContentBlock{
		{Type: "image", Source: &api.ImageSource{Type: "base64", MediaType: "image/png", Data: strings.Repeat("P", 200)}},
	}}}
	opts := GenerationOptions{Model: "openai/gpt-5", images: tool.ImagePolicy{Route: "openai/gpt-5", MaxRequestImageBytes: 100}}
	_, err := buildRequest(opts, messages, nil, nil)
	var tooLarge *RequestImagesTooLargeError
	if !errors.As(err, &tooLarge) || !strings.Contains(err.Error(), tool.EnvRequestImageMaxBytes) {
		t.Fatalf("err = %v, want *RequestImagesTooLargeError naming %s", err, tool.EnvRequestImageMaxBytes)
	}
}
