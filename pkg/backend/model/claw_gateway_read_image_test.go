package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// A claw node on an OpenAI-compatible gateway that opens an image with
// read_image must hand the model the image itself: the real claw backend,
// the real read_image registration, and a fake chat-completions gateway that
// records what the turn after the tool call carries. Measured on the PLIAGE
// gateway, the only sequence every vision model accepts is the tool message
// (text), an assistant message, then a user message holding the image.
func TestGatewayReadImage_TheModelReceivesTheImage(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "schema.png")
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 200, A: 255})
	var raw bytes.Buffer
	if err := png.Encode(&raw, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if err := os.WriteFile(pngPath, raw.Bytes(), 0o600); err != nil {
		t.Fatalf("write png: %v", err)
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw.Bytes())

	reg := tool.NewRegistry()
	if err := tool.RegisterClawReadImage(reg, tool.NewReadScope(dir)); err != nil {
		t.Fatalf("register read_image: %v", err)
	}
	def, err := reg.Resolve("read_image")
	if err != nil {
		t.Fatalf("resolve read_image: %v", err)
	}
	readImage := def.ToDelegateDef()
	if !readImage.ImageResult {
		t.Fatal("read_image's delegate definition does not attest an image result")
	}

	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
			http.NotFound(w, req)
			return
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		turn := len(bodies)
		mu.Unlock()

		var buf bytes.Buffer
		frame := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(&buf, "data: %s\n\n", b)
		}
		delta := func(d map[string]any) {
			frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": d}}})
		}
		finish := "stop"
		if turn == 1 {
			args, _ := json.Marshal(map[string]string{"path": pngPath})
			delta(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index": 0, "id": "aB3dE5gH9", "type": "function",
				"function": map[string]any{"name": "read_image", "arguments": string(args)},
			}}})
			finish = "tool_calls"
		} else {
			delta(map[string]any{"role": "assistant", "content": "A red pixel."})
		}
		frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
		frame(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10}})
		buf.WriteString("data: [DONE]\n\n")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)

	clearGatewayEnv(t)
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", srv.URL)
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-fake-gateway")
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "")
	t.Setenv("OPENAI_COMPATIBLE_MODELS", `{"scaleway/vision-test": {"context_window": 131072, "max_output_tokens": 4096}}`)
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)

	backend := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1})
	if _, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "voir",
		Model:        "openai_compatible/scaleway/vision-test",
		UserPrompt:   "Open schema.png with read_image and describe it.",
		HasTools:     true,
		ToolMaxSteps: 3,
		ToolDefs:     []delegate.ToolDef{readImage},
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("gateway saw %d requests, want the tool turn and the answer turn", len(bodies))
	}
	var second struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodies[1], &second); err != nil {
		t.Fatalf("decode second request: %v", err)
	}
	var roles []string
	for _, m := range second.Messages {
		roles = append(roles, m.Role)
	}
	n := len(second.Messages)
	if n < 3 || strings.Join(roles[n-3:], ",") != "tool,assistant,user" {
		t.Fatalf("roles = %v, want the turn to end tool, assistant, user", roles)
	}
	var toolText string
	if err := json.Unmarshal(second.Messages[n-3].Content, &toolText); err != nil {
		t.Fatalf("tool content is not a string: %s", second.Messages[n-3].Content)
	}
	if strings.Contains(toolText, base64.StdEncoding.EncodeToString(raw.Bytes())) {
		t.Errorf("the tool message carries the image as base64 text: %q", toolText)
	}
	var parts []struct {
		Type     string `json:"type"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(second.Messages[n-1].Content, &parts); err != nil {
		t.Fatalf("user content is not a parts array: %s", second.Messages[n-1].Content)
	}
	found := false
	for _, p := range parts {
		if p.Type == "image_url" && p.ImageURL.URL == wantURL {
			found = true
		}
	}
	if !found {
		t.Errorf("no image_url part carries the PNG read_image loaded; parts = %s", second.Messages[n-1].Content)
	}
}
