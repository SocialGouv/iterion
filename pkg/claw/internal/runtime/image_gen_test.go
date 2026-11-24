package runtime

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

type imageLoopClient struct {
	turns      int
	png        []byte
	seenResult bool
}

func (c *imageLoopClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.turns++
	ch := make(chan api.StreamEvent, 8)
	ch <- api.StreamEvent{Type: api.EventMessageStart}
	if c.turns == 1 {
		advertised := false
		for _, tool := range req.Tools {
			advertised = advertised || tool.Name == "image_gen"
		}
		if !advertised {
			ch <- api.StreamEvent{Type: api.EventError, ErrorMessage: "image_gen not advertised"}
		} else {
			ch <- api.StreamEvent{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", ID: "call-1", Name: "image_gen"}}
			ch <- api.StreamEvent{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"prompt":"a red square"}`}}
			ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
			ch <- api.StreamEvent{Type: api.EventMessageDelta, StopReason: "tool_use"}
		}
	} else {
		for _, message := range req.Messages {
			for _, block := range message.Content {
				if block.Type == "tool_result" && block.ToolUseID == "call-1" && !block.IsError &&
					len(block.Content) > 0 && strings.Contains(block.Content[0].Text, "Generated PNG saved to ") {
					c.seenResult = true
				}
			}
		}
		ch <- api.StreamEvent{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text"}}
		ch <- api.StreamEvent{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "Image ready."}}
		ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
		ch <- api.StreamEvent{Type: api.EventMessageDelta, StopReason: "end_turn"}
	}
	ch <- api.StreamEvent{Type: api.EventMessageStop}
	close(ch)
	return ch, nil
}

func (c *imageLoopClient) GenerateImage(_ context.Context, _ string) (api.GeneratedImage, error) {
	return api.GeneratedImage{Data: c.png}, nil
}

type unsupportedImageLoopClient struct{ imageLoopClient }

func (c *unsupportedImageLoopClient) SupportsImageGeneration() bool { return false }

type textOnlyLoopClient struct{}

func (c *textOnlyLoopClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	ch := make(chan api.StreamEvent)
	close(ch)
	return ch, nil
}

func loopTestPNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestImageGenDynamicAvailability(t *testing.T) {
	loop := NewConversationLoop(&Config{ProviderName: "openai", Model: "gpt-5.5", GeneratedImageDir: t.TempDir()}, &textOnlyLoopClient{})
	hasTool := func() bool {
		for _, tool := range loop.allTools() {
			if tool.Name == "image_gen" {
				return true
			}
		}
		return false
	}
	if hasTool() {
		t.Fatal("text-only client advertised image_gen")
	}
	loop.Client = &imageLoopClient{png: loopTestPNG(t)}
	if !hasTool() {
		t.Fatal("image-capable client did not advertise image_gen")
	}
	loop.imageGenDisabled = true
	if hasTool() {
		t.Fatal("restricted subagent advertised image_gen")
	}
	denied := loop.ExecuteTool(context.Background(), "image_gen", map[string]any{"prompt": "test"})
	if !denied.IsError || !strings.Contains(denied.Content[0].Text, "does not support") {
		t.Fatalf("restricted subagent executed image_gen: %+v", denied)
	}
	loop.imageGenDisabled = false
	loop.Client = &textOnlyLoopClient{}
	if hasTool() {
		t.Fatal("stale image_gen after client switch")
	}
	loop.Client = &unsupportedImageLoopClient{}
	if hasTool() {
		t.Fatal("unsupported image endpoint advertised image_gen")
	}
	result := loop.ExecuteTool(context.Background(), "image_gen", map[string]any{"prompt": "test"})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "does not support") {
		t.Fatalf("stale tool execution = %+v", result)
	}
	loop.Client = &imageLoopClient{png: loopTestPNG(t)}
	loop.Config.ProviderName = "anthropic"
	if hasTool() {
		t.Fatal("non-OpenAI provider advertised image_gen")
	}
}

func TestImageGenBothConversationPaths(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "plain"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			client := &imageLoopClient{png: loopTestPNG(t)}
			loop := NewConversationLoop(&Config{ProviderName: "openai", Model: "gpt-5.5", GeneratedImageDir: root}, client)
			var err error
			if streaming {
				events := make(chan TurnEvent, 32)
				err = loop.SendMessageStreaming(context.Background(), "generate an image", events)
			} else {
				err = loop.SendMessage(context.Background(), "generate an image")
			}
			if err != nil || client.turns != 2 || !client.seenResult {
				t.Fatalf("conversation = turns %d, result %v, error %v", client.turns, client.seenResult, err)
			}
			files, err := os.ReadDir(root)
			if err != nil || len(files) != 1 {
				t.Fatalf("image artifacts = %v, %v", files, err)
			}
			imageFile, err := os.Open(filepath.Join(root, files[0].Name()))
			if err != nil {
				t.Fatal(err)
			}
			_, err = png.Decode(imageFile)
			imageFile.Close()
			if err != nil {
				t.Fatalf("saved artifact is not PNG: %v", err)
			}
			sessionDir := t.TempDir()
			if err := SaveSession(sessionDir, loop.Session); err != nil {
				t.Fatal(err)
			}
			serialized, err := os.ReadFile(filepath.Join(sessionDir, loop.Session.ID+".json"))
			if err != nil || len(serialized) > 8*1024 || strings.Contains(string(serialized), "b64_json") {
				t.Fatalf("saved session is missing or contains image bytes: %d bytes, %v", len(serialized), err)
			}
		})
	}
}
