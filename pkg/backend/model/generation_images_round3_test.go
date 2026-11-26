package model

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// sentImages reports a built request's image count and how many of them
// are over side pixels on their longer side (or of unreadable size).
func sentImages(t *testing.T, opts GenerationOptions, msgs []api.Message, side int) (n, over int) {
	t.Helper()
	req, err := buildRequest(opts, msgs, nil, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	count := func(c api.ContentBlock) {
		if c.Type != "image" {
			return
		}
		n++
		if w, h, _, ok := tool.ImageHeader(c.Source); !ok || w > side || h > side {
			over++
		}
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			count(b)
			for _, c := range b.Content {
				count(c)
			}
		}
	}
	return n, over
}

// roundsOf is a conversation of one tool round per image of data.
func roundsOf(prompt []string, data ...string) []api.Message {
	msgs := toolRounds(0, len(data), 1)
	for _, p := range prompt {
		msgs[0].Content = append(msgs[0].Content, api.ContentBlock{Type: "image", Source: &api.ImageSource{Type: "base64", MediaType: "image/png", Data: p}})
	}
	for i, d := range data {
		msgs[2+2*i].Content[0].Content[1].Source.Data = d
	}
	return msgs
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// After 20 small images, a QHD capture of the last run is the screen the
// model needs: the many-image rule is met by dropping an older small image,
// never the newest capture (which no tool can make smaller).
func TestBuildRequest_TheManyImageRuleKeepsTheNewestCapture(t *testing.T) {
	t.Parallel()
	small, qhd := encodedGrayPNG(t, 1280, 720), encodedGrayPNG(t, 2560, 1440)
	opts := GenerationOptions{Model: "anthropic/claude-sonnet-4-6", images: anthropicPolicy(t)}
	for _, captures := range []int{1, 4} {
		data := append(repeat(small, 20), repeat(qhd, captures)...)
		kept, markers := requestToolImages(t, opts, roundsOf(nil, data...))
		if !kept[len(kept)-1] {
			t.Fatalf("%d captures: the newest capture was dropped (kept = %v)", captures, kept)
		}
		n, over := sentImages(t, opts, roundsOf(nil, data...), 2000)
		if n > 20 && over > 0 {
			t.Fatalf("%d captures: %d images sent, %d over 2000 px", captures, n, over)
		}
		for _, m := range markers {
			if strings.Contains(m, "smaller image") {
				t.Errorf("marker %q promises a smaller image no tool can produce", m)
			}
		}
	}
}

// Prompt images count toward the many-image rule: 10 prompt images and 11
// QHD captures send at most 20, or none over 2000 px.
func TestBuildRequest_TheManyImageRuleCountsPromptImages(t *testing.T) {
	t.Parallel()
	small, qhd := encodedGrayPNG(t, 1280, 720), encodedGrayPNG(t, 2560, 1440)
	opts := GenerationOptions{Model: "anthropic/claude-sonnet-4-6", images: anthropicPolicy(t)}
	n, over := sentImages(t, opts, roundsOf(repeat(small, 10), repeat(qhd, 11)...), 2000)
	if n > 20 && over > 0 {
		t.Fatalf("%d images sent, %d over 2000 px: the rule holds prompt images too", n, over)
	}
}

// The rule is "more than 20": exactly 20 QHD captures all go; a 21st makes
// one go.
func TestBuildRequest_TheManyImageRuleStartsAfterTwenty(t *testing.T) {
	t.Parallel()
	qhd := encodedGrayPNG(t, 2560, 1440)
	opts := GenerationOptions{Model: "anthropic/claude-sonnet-4-6", images: anthropicPolicy(t)}
	if n, _ := sentImages(t, opts, roundsOf(nil, repeat(qhd, 20)...), 2000); n != 20 {
		t.Fatalf("20 QHD captures: %d sent, want all 20", n)
	}
	if n, _ := sentImages(t, opts, roundsOf(nil, repeat(qhd, 21)...), 2000); n != 20 {
		t.Fatalf("21 QHD captures: %d sent, want 20", n)
	}
}

// A known format whose size cannot be read counts as over the side: 20
// small images and one such JPEG send at most 20.
func TestBuildRequest_AnUnreadableSizeCountsAsOverTheSide(t *testing.T) {
	t.Parallel()
	small := encodedGrayPNG(t, 1280, 720)
	unreadable := "/9j/" + strings.Repeat("A", 64) // JPEG magic, no frame header
	opts := GenerationOptions{Model: "anthropic/claude-sonnet-4-6", images: anthropicPolicy(t)}
	n, over := sentImages(t, opts, roundsOf(nil, append(repeat(small, 20), unreadable)...), 2000)
	if n > 20 && over > 0 {
		t.Fatalf("%d images sent, %d of unknown or over-2000 size", n, over)
	}
}
