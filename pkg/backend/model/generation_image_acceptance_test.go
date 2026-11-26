package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// smallImageOutput is read_image's output: one tiny PNG-declared image.
const smallImageOutput = `{"description":"diagram.png","blocks":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}`

func readImageTool() GenerationTool {
	return GenerationTool{
		Name:        "read_image",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return smallImageOutput, nil },
		imageResult: true,
	}
}

// pliageGateway configures the process the way the PLIAGE deployment is: a
// gateway whose catalog provider is models.dev's scaleway, its ids named
// provider-first. hatch is ITERION_IMAGE_ACCEPTANCE ("" unset).
func pliageGateway(t *testing.T, hatch string) {
	t.Helper()
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(compatgw.CatalogProviderEnv, "scaleway")
	t.Setenv(compatgw.CatalogModelsEnv, "")
	t.Setenv(compatgw.ResolvedEnvName, "")
	t.Setenv(tool.EnvImageAcceptance, hatch)
	envtrust.ResetForTest()
	compatgw.ResetCatalogCaches()
	t.Cleanup(envtrust.ResetForTest)
	t.Cleanup(compatgw.ResetCatalogCaches)
}

// anyImage reports whether a request carries an image anywhere — a prompt
// image or a tool result's.
func anyImage(messages []api.Message) bool {
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type == "image" {
				return true
			}
			for _, c := range b.Content {
				if c.Type == "image" {
					return true
				}
			}
		}
	}
	return false
}

// runReadImage drives one generation whose model calls read_image once, and
// returns the requests the provider received.
func runReadImage(t *testing.T, spec string) []api.CreateMessageRequest {
	t.Helper()
	client := newMockClient(toolUseEvents("img-1", "read_image", `{}`, 10, 5), textEvents("done", 10, 5))
	_, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    spec,
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "look at the diagram"}}}},
		Tools:    []GenerationTool{readImageTool()},
		MaxSteps: 3,
	})
	if err != nil {
		t.Fatalf("%s: generate: %v", spec, err)
	}
	calls := client.getCalls()
	if len(calls) != 2 {
		t.Fatalf("%s: %d requests, want 2", spec, len(calls))
	}
	return calls
}

// The PLIAGE case: on openai_compatible/scaleway/glm-5.3, whose models.dev
// row is text-only, read_image's image becomes a recoverable tool error the
// model reads, and no request carries the image. The node continues: the
// second request goes out with the error as the tool result.
func TestReadImage_OnATextOnlyGatewayRoute_IsARecoverableRefusal(t *testing.T) {
	pliageGateway(t, "")
	calls := runReadImage(t, "openai_compatible/scaleway/glm-5.3")
	for i, c := range calls {
		if anyImage(c.Messages) {
			t.Fatalf("request %d carries an image to a route that refuses them", i)
		}
	}
	msgs := calls[1].Messages
	result := msgs[len(msgs)-1].Content[0]
	text := result.Content
	if !result.IsError || len(text) == 0 {
		t.Fatalf("tool result = %+v, want an is_error result", result)
	}
	for _, want := range []string{`"openai_compatible/scaleway/glm-5.3" does not accept images`, "again will not help", "another way", "The tool described the image: diagram.png"} {
		if !strings.Contains(text[0].Text, want) {
			t.Errorf("the refusal %q does not say %q", text[0].Text, want)
		}
	}
}

// A route whose row takes images, and a route no table carries, send the
// image as before.
func TestReadImage_OnAVisionOrUnknownGatewayRoute_SendsTheImage(t *testing.T) {
	pliageGateway(t, "")
	for _, spec := range []string{
		"openai_compatible/scaleway/mistral-small-3.2-24b-instruct-2506",
		"openai_compatible/scaleway/no-such-model",
	} {
		calls := runReadImage(t, spec)
		if !anyImage(calls[1].Messages) {
			t.Errorf("%s: the image did not reach the route", spec)
		}
	}
}

// The operator's override: send ignores the catalog's refusal.
func TestImageAcceptanceSend_OverridesAStatedRefusal(t *testing.T) {
	pliageGateway(t, "send")
	calls := runReadImage(t, "openai_compatible/scaleway/glm-5.3")
	if !anyImage(calls[1].Messages) {
		t.Fatal("ITERION_IMAGE_ACCEPTANCE=send did not send the image")
	}
}

// An invalid override fails the generation, naming the knob, before any
// request goes out.
func TestImageAcceptance_AnInvalidValueIsAnError(t *testing.T) {
	pliageGateway(t, "sometimes")
	opts := GenerationOptions{Model: "openai_compatible/scaleway/glm-5.3"}
	if _, err := withRouteImagePolicy(context.Background(), &opts); err == nil || !strings.Contains(err.Error(), tool.EnvImageAcceptance) {
		t.Fatalf("err = %v, want an error naming %s", err, tool.EnvImageAcceptance)
	}
}

// Images produced under a vision route and replayed onto a text-only one (a
// fallback) go out as markers, the reduction note with them; the history
// keeps them.
func TestBuildRequest_HistoryImagesOnARefusingRouteBecomeMarkers(t *testing.T) {
	pliageGateway(t, "")
	opts := GenerationOptions{Model: "openai_compatible/scaleway/glm-5.3"}
	if _, err := withRouteImagePolicy(context.Background(), &opts); err != nil {
		t.Fatal(err)
	}
	if opts.images.Acceptance != tool.ImagesRefused {
		t.Fatalf("acceptance %v, want ImagesRefused", opts.images.Acceptance)
	}
	shot := toolImageResult("shot", 40)
	shot.Content = append(shot.Content, api.ContentBlock{Type: "text", Text: "[image 1 was reduced to fit this model route's per-image limits: 4000x3000 → 2000x1500]"})
	messages := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "look"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "shot", Name: "screenshot"}}},
		{Role: "user", Content: []api.ContentBlock{shot}},
	}
	req, err := buildRequest(opts, messages, nil, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if anyImage(req.Messages) {
		t.Fatal("the replayed image reached a route that refuses images")
	}
	got := req.Messages[2].Content[0].Content
	if len(got) != 2 || !strings.Contains(got[1].Text, "does not accept images") {
		t.Fatalf("tool result = %+v, want its text then the route marker, the reduction note gone", got)
	}
	if messages[2].Content[0].Content[1].Type != "image" {
		t.Error("the conversation lost its image: the refusal must shape the request only")
	}
}

// A prompt image on a route that refuses images is the operator's
// attachment, which no tool can replace: the generation fails with a typed
// error before any request goes out.
func TestPromptImage_OnARefusingRoute_IsRefusedBeforeAnyRequest(t *testing.T) {
	pliageGateway(t, "")
	client := newMockClient(textEvents("never", 10, 5))
	_, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model: "openai_compatible/scaleway/glm-5.3",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{
			{Type: "text", Text: "what is this"},
			{Type: "image", Source: &api.ImageSource{Type: "base64", MediaType: "image/png", Data: "QUJD"}},
		}}},
	})
	var refused *RequestImagesRefusedError
	if !errors.As(err, &refused) || refused.Count != 1 || !strings.Contains(err.Error(), tool.EnvImageAcceptance) {
		t.Fatalf("err = %v, want *RequestImagesRefusedError naming %s", err, tool.EnvImageAcceptance)
	}
	if n := len(client.getCalls()); n != 0 {
		t.Fatalf("%d requests went out, want none", n)
	}
}

// Native routes: a Claude model on an Anthropic route takes images; another
// provider's route answers from the registry's exact key, a text-only row
// refusing; a bare spec states nothing.
func TestRouteImageInput_NativeRoutes(t *testing.T) {
	pliageGateway(t, "")
	text, vision := false, true
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"openai/text-only-model":   {ImageInput: &text},
		"openai/vision-model":      {ImageInput: &vision},
		"other/text-only-model":    {ImageInput: &vision},
		"zai-coding-plan/glm-5.3":  {ImageInput: &text},
		"zai-coding-plan/glm-5.4":  {ImageInput: &vision},
		"zai/glm-test-zai-only":    {ImageInput: &text},
		"anthropic/glm-5.3":        {ImageInput: &vision},
		"moonshotai/kimi-test":     {ImageInput: &text},
		"google-vertex/gem-test":   {ImageInput: &text},
		"amazon-bedrock/nova-test": {ImageInput: &text},
	})))
	for spec, want := range map[string]tool.ImageAcceptance{
		"anthropic/claude-sonnet-4-6": tool.ImagesAccepted,
		"openai/text-only-model":      tool.ImagesRefused,
		"openai/vision-model":         tool.ImagesAccepted,
		"openai/unlisted":             tool.ImagesUnknown,
		"text-only-model":             tool.ImagesUnknown,
		// z.ai GLM on the anthropic wire (bare or anthropic/) reads z.ai's
		// rows, zai-coding-plan first, never a foreign anthropic/ row.
		"anthropic/glm-5.3":           tool.ImagesRefused,
		"glm-5.3":                     tool.ImagesRefused,
		"anthropic/glm-5.4":           tool.ImagesAccepted,
		"anthropic/glm-test-zai-only": tool.ImagesRefused,
		"anthropic/claude-opus-4-8":   tool.ImagesAccepted,
		"moonshot/kimi-test":          tool.ImagesRefused,
		"vertex/gem-test":             tool.ImagesRefused,
		"bedrock/nova-test":           tool.ImagesRefused,
		"foundry/text-only-model":     tool.ImagesUnknown,
	} {
		opts := GenerationOptions{Model: spec}
		if _, err := withRouteImagePolicy(context.Background(), &opts); err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if opts.images.Acceptance != want {
			t.Errorf("%s: acceptance %v, want %v", spec, opts.images.Acceptance, want)
		}
	}
}

// The capability views show the image flag the policy decides: the
// gateway's catalog row, z.ai's row for a GLM on the anthropic wire, and a
// Claude model as accepted.
func TestResolveCapabilities_ShowsThePolicysImageInput(t *testing.T) {
	pliageGateway(t, "")
	text := false
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"zai-coding-plan/glm-5.3": {ImageInput: &text},
	})))
	for _, tc := range []struct {
		provider, model, want string
	}{
		{"openai_compatible", "scaleway/glm-5.3", "false"},
		{"openai_compatible", "scaleway/mistral-small-3.2-24b-instruct-2506", "true"},
		{"anthropic", "glm-5.3", "false"},
		{"anthropic", "claude-sonnet-4-6", "true"},
		{"openai", "unlisted", "unstated"},
	} {
		got := "unstated"
		if b := ResolveCapabilities(tc.provider, tc.model).ImageInput; b != nil {
			got = map[bool]string{true: "true", false: "false"}[*b]
		}
		if got != tc.want {
			t.Errorf("%s/%s: image_input %s, want %s", tc.provider, tc.model, got, tc.want)
		}
	}
}

// A native route whose live registry has not loaded (an empty table) reads
// the embedded snapshot: z.ai's glm-5.3 row there is text-only, its glm-5.4
// row takes images.
func TestRouteImageInput_NativeFallsBackToTheSnapshot(t *testing.T) {
	pliageGateway(t, "")
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(nil)))
	for spec, want := range map[string]tool.ImageAcceptance{
		"anthropic/glm-5.3": tool.ImagesRefused,
		"anthropic/glm-5.4": tool.ImagesAccepted,
	} {
		opts := GenerationOptions{Model: spec}
		if _, err := withRouteImagePolicy(context.Background(), &opts); err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if opts.images.Acceptance != want {
			t.Errorf("%s: acceptance %v, want %v", spec, opts.images.Acceptance, want)
		}
	}
}
