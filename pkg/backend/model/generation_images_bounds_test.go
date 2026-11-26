package model

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// toolRounds is a conversation of a prompt (with a promptBytes image when
// promptBytes > 0) and n tool rounds, each returning one image of size
// bytes of base64.
func toolRounds(promptBytes, n, size int) []api.Message {
	prompt := []api.ContentBlock{{Type: "text", Text: "look"}}
	if promptBytes > 0 {
		prompt = append(prompt, api.ContentBlock{Type: "image", Source: &api.ImageSource{Type: "base64", MediaType: "image/png", Data: strings.Repeat("P", promptBytes)}})
	}
	msgs := []api.Message{{Role: "user", Content: prompt}}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("t%d", i)
		msgs = append(msgs,
			api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: id, Name: "screenshot"}}},
			api.Message{Role: "user", Content: []api.ContentBlock{toolImageResult(id, size)}})
	}
	return msgs
}

// requestToolImages reports, for each tool round of a built request, whether
// its image is still an image, and the omission marker of each dropped one.
func requestToolImages(t *testing.T, opts GenerationOptions, msgs []api.Message) (kept []bool, markers []string) {
	t.Helper()
	req, err := buildRequest(opts, msgs, nil, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type != "tool_result" {
				continue
			}
			c := b.Content[1]
			kept = append(kept, c.Type == "image")
			if c.Type != "image" {
				markers = append(markers, c.Text)
			}
		}
	}
	return kept, markers
}

func droppedPrefix(kept []bool) int {
	n := 0
	for n < len(kept) && !kept[n] {
		n++
	}
	return n
}

// The per-image bound holds on the request too: a history built under a
// route with a larger bound, replayed on this one, would otherwise send an
// image the provider refuses on every retry. The over-bound tool image is
// replaced by a marker in the request only.
func TestBuildRequest_ReplacesAToolImageOverThePerImageBound(t *testing.T) {
	t.Parallel()
	msgs := toolRounds(0, 2, 60)
	msgs[2].Content[0].Content[1].Source.Data = strings.Repeat("B", 40)
	opts := GenerationOptions{Model: "anthropic/claude-sonnet-4-6", images: tool.ImagePolicy{MaxImageBase64Bytes: 50}}
	kept, markers := requestToolImages(t, opts, msgs)
	if len(kept) != 2 || !kept[0] || kept[1] {
		t.Fatalf("kept = %v, want the 40-byte image kept and the 60-byte one replaced", kept)
	}
	if !strings.Contains(markers[0], "per image") {
		t.Errorf("marker = %q, want it to name the per-image limit", markers[0])
	}
	if msgs[4].Content[0].Content[1].Type != "image" {
		t.Error("the conversation lost its image: the bound must shape the request only")
	}
}

// Once over the request budget, the oldest tool images go until the request
// is well under it, so the cut point — the prompt-cache boundary — moves
// every few turns rather than on every one.
func TestBuildRequest_TheCutPointMovesEveryFewTurns(t *testing.T) {
	t.Parallel()
	opts := GenerationOptions{Model: "openai/gpt-5", images: tool.ImagePolicy{MaxRequestImageBytes: 100}}
	prev, moves := -1, 0
	for n := 4; n <= 9; n++ {
		kept, _ := requestToolImages(t, opts, toolRounds(0, n, 30))
		d := droppedPrefix(kept)
		if d < prev {
			t.Fatalf("%d rounds: the cut point moved back (%d after %d)", n, d, prev)
		}
		if prev >= 0 && d != prev {
			moves++
		}
		prev = d
		if !kept[len(kept)-1] {
			t.Fatalf("%d rounds: the newest image was dropped while it fits", n)
		}
	}
	if moves > 3 {
		t.Errorf("the cut point moved on %d of 5 successive requests, want it to move every few turns", moves)
	}
}

// A tool image of the last run that cannot fit beside the prompt's images
// is replaced by a marker saying calling the tool again will not help — the
// usual marker would invite the model into a loop.
func TestBuildRequest_ALastRunImageThatCannotFitSaysCallingAgainWontHelp(t *testing.T) {
	t.Parallel()
	opts := GenerationOptions{Model: "openai/gpt-5", images: tool.ImagePolicy{MaxRequestImageBytes: 100}}
	kept, markers := requestToolImages(t, opts, toolRounds(80, 1, 40))
	if len(kept) != 1 || kept[0] {
		t.Fatalf("kept = %v, want the last image replaced", kept)
	}
	if !strings.Contains(markers[0], "will not help") {
		t.Errorf("marker = %q, want it to say calling the tool again will not help", markers[0])
	}
}

// The route's image count per request bounds the request like its bytes:
// the oldest tool images go first.
func TestBuildRequest_BoundsTheImageCountPerRequest(t *testing.T) {
	t.Parallel()
	opts := GenerationOptions{Model: "anthropic/claude-sonnet-4-6", images: tool.ImagePolicy{MaxRequestImages: 4}}
	kept, markers := requestToolImages(t, opts, toolRounds(0, 6, 10))
	n := 0
	for _, k := range kept {
		if k {
			n++
		}
	}
	if n > 4 || !kept[len(kept)-1] || kept[0] {
		t.Fatalf("kept = %v, want at most 4 images, the oldest dropped and the newest kept", kept)
	}
	if !strings.Contains(markers[0], tool.EnvRequestImageMaxCount) {
		t.Errorf("marker = %q, want it to name %s", markers[0], tool.EnvRequestImageMaxCount)
	}
	opts.images.MaxRequestImages = 0
	if kept, _ := requestToolImages(t, opts, toolRounds(0, 6, 10)); slices.Contains(kept, false) {
		t.Errorf("with no count bound, kept = %v, want every image", kept)
	}
}

// A tool image whose header declares a side over the route's bound — built
// under another route — is replaced in the request.
func TestBuildRequest_ReplacesAToolImageOverThePerImageSide(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 9000, 10))); err != nil {
		t.Fatal(err)
	}
	msgs := toolRounds(0, 1, 10)
	msgs[2].Content[0].Content[1].Source.Data = base64.StdEncoding.EncodeToString(buf.Bytes())
	opts := GenerationOptions{Model: "anthropic/claude-sonnet-4-6", images: tool.ImagePolicy{MaxImageSide: 8000}}
	kept, markers := requestToolImages(t, opts, msgs)
	if kept[0] || !strings.Contains(markers[0], tool.EnvImageMaxSide) {
		t.Fatalf("kept = %v, markers = %v, want the 9000-pixel image replaced naming %s", kept, markers, tool.EnvImageMaxSide)
	}
}
