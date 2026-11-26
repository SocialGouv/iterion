package model

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

func encodedGrayPNG(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func anthropicPolicy(t *testing.T) tool.ImagePolicy {
	t.Helper()
	p, err := tool.RouteImagePolicy("anthropic/claude-sonnet-4-6", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Anthropic refuses a request of more than 20 images any of which is over
// 2000 pixels a side, on every retry and resume. A long QHD capture session
// keeps at most 20 images, or none over 2000 pixels.
func TestBuildRequest_HoldsTheManyImageSideRule(t *testing.T) {
	t.Parallel()
	qhd := encodedGrayPNG(t, 2560, 1440)
	msgs := toolRounds(0, 25, 10)
	for i := 2; i < len(msgs); i += 2 {
		msgs[i].Content[0].Content[1].Source.Data = qhd
	}
	opts := GenerationOptions{Model: "anthropic/claude-sonnet-4-6", images: anthropicPolicy(t)}
	kept, markers := requestToolImages(t, opts, msgs)
	n := 0
	for _, k := range kept {
		if k {
			n++
		}
	}
	if n > 20 {
		t.Fatalf("%d QHD images kept in one request, want at most 20 when they are over 2000 pixels", n)
	}
	if !kept[len(kept)-1] || !strings.Contains(markers[0], "2000") {
		t.Errorf("kept = %v, first marker %q: want the oldest dropped under a marker naming the 2000-pixel rule", kept, markers[0])
	}

	// The prompt's own images alone break the rule: refused, never sent.
	prompt := []api.ContentBlock{{Type: "text", Text: "look"}}
	for i := 0; i < 21; i++ {
		prompt = append(prompt, api.ContentBlock{Type: "image", Source: &api.ImageSource{Type: "base64", MediaType: "image/png", Data: qhd}})
	}
	if _, err := buildRequest(opts, []api.Message{{Role: "user", Content: prompt}}, nil, nil); err == nil {
		t.Error("21 QHD prompt images were sent, want a typed refusal")
	}
}

// The results of one parallel batch that each fit alone, together over the
// budget, are not told calling again will not help: nothing of the prompt's
// stands in their way.
func TestBuildRequest_AParallelBatchIsToldToAskFewerAtATime(t *testing.T) {
	t.Parallel()
	msgs := []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "look"}}}}
	var uses, results []api.ContentBlock
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("p%d", i)
		uses = append(uses, api.ContentBlock{Type: "tool_use", ID: id, Name: "read_image"})
		results = append(results, toolImageResult(id, 30))
	}
	msgs = append(msgs, api.Message{Role: "assistant", Content: uses}, api.Message{Role: "user", Content: results})
	opts := GenerationOptions{Model: "openai/gpt-5", images: tool.ImagePolicy{MaxRequestImageBytes: 100}}
	_, markers := requestToolImages(t, opts, msgs)
	if len(markers) == 0 {
		t.Fatal("no image dropped from a batch of 150 bytes under a 100-byte budget")
	}
	for _, m := range markers {
		if strings.Contains(m, "will not help") || !strings.Contains(m, "fewer") {
			t.Errorf("marker = %q, want it to ask for fewer images at a time, not to say calling again will not help", m)
		}
	}
}

// A reduced capture dropped from a request takes its reduction note with it.
func TestBuildRequest_ADroppedReducedCaptureTakesItsNoteAlong(t *testing.T) {
	t.Parallel()
	msgs := toolRounds(0, 2, 80)
	old := &msgs[2].Content[0]
	old.Content = append(old.Content, api.ContentBlock{Type: "text", Text: "[image 0 was reduced to fit this model route's per-image limits: 4000x3000 png → 2000x1500 JPEG]"})
	opts := GenerationOptions{Model: "openai/gpt-5", images: tool.ImagePolicy{MaxRequestImageBytes: 100}}
	req, err := buildRequest(opts, msgs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range req.Messages[2].Content[0].Content {
		if strings.Contains(c.Text, "was reduced") {
			t.Fatalf("dropped capture's block = %+v, want its reduction note gone with it", req.Messages[2].Content[0].Content)
		}
	}
	if len(msgs[2].Content[0].Content) != 3 {
		t.Error("the conversation lost the note: the bound must shape the request only")
	}
}

// A history image whose media type contradicts its bytes — written before
// the header decided it — goes out under the type its bytes declare.
func TestBuildRequest_TheHeaderDecidesAReplayedImagesMediaType(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 32, 32)), nil); err != nil {
		t.Fatal(err)
	}
	msgs := toolRounds(0, 1, 10)
	msgs[2].Content[0].Content[1].Source.Data = base64.StdEncoding.EncodeToString(buf.Bytes())
	opts := GenerationOptions{Model: "anthropic/claude-sonnet-4-6", images: anthropicPolicy(t)}
	req, err := buildRequest(opts, msgs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Messages[2].Content[0].Content[1].Source.MediaType; got != "image/jpeg" {
		t.Errorf("replayed media type = %q, want image/jpeg", got)
	}
	if msgs[2].Content[0].Content[1].Source.MediaType != "image/png" {
		t.Error("the conversation was rewritten: the correction must shape the request only")
	}
}

// The knobs read are exactly the ones the policy asks for and the crossing
// carries: a name added to one list and not the other would be ignored on
// one side.
func TestImageBoundKnobsAreTheSetThePolicyReads(t *testing.T) {
	t.Parallel()
	knobs := imageBoundKnobs()
	if len(knobs) != len(tool.ImagePolicyEnvVars) {
		t.Fatalf("read %d knobs, the policy names %d", len(knobs), len(tool.ImagePolicyEnvVars))
	}
	for _, name := range tool.ImagePolicyEnvVars {
		if _, ok := knobs[name]; !ok {
			t.Errorf("%s is not read", name)
		}
	}
}
