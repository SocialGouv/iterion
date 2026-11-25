package tool

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// jpegSegment writes one JPEG marker segment.
func jpegSegment(w *bytes.Buffer, marker byte, payload []byte) {
	w.Write([]byte{0xff, marker})
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(payload)+2))
	w.Write(l[:])
	w.Write(payload)
}

// manyScanCMYKJPEG is a crafted progressive CMYK JPEG (Adobe APP14) whose
// body is `scans` AC scans coding every block as EOB runs: a few bytes per
// scan, while Go's decoder walks every block of the component on each one.
// At 4 MiB of such scans a decode costs minutes of uncancellable CPU; here
// the scan count stays small so the file decodes in milliseconds.
func manyScanCMYKJPEG(width, height, scans int) []byte {
	var w bytes.Buffer
	w.Write([]byte{0xff, 0xd8})
	jpegSegment(&w, 0xee, []byte{'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0})
	q := make([]byte, 65)
	for i := 1; i < 65; i++ {
		q[i] = 1
	}
	jpegSegment(&w, 0xdb, q)
	sof := []byte{8, byte(height >> 8), byte(height), byte(width >> 8), byte(width), 4}
	for c := 0; c < 4; c++ {
		sof = append(sof, byte(c+1), 0x11, 0)
	}
	jpegSegment(&w, 0xc2, sof)
	jpegSegment(&w, 0xc4, []byte{0x10, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xe0})
	blocks := ((width + 7) / 8) * ((height + 7) / 8)
	data := make([]byte, ((blocks+16383)/16384*15+7)/8+2)
	for s := 0; s < scans; s++ {
		jpegSegment(&w, 0xda, []byte{1, byte(s%4 + 1), 0x00, 1, 63, 0})
		w.Write(data)
	}
	w.Write([]byte{0xff, 0xd9})
	return w.Bytes()
}

// read_image hands the model untrusted workspace bytes: over the route's
// per-image bound it is refused from its size alone, never decoded — a
// crafted progressive JPEG costs minutes of CPU and a CMYK one gigabytes to
// decode, under any pixel bound. The crafted file here would decode and
// reduce in milliseconds: a reduction is the defect.
func TestImageResultContent_NeverDecodesReadImageBytes(t *testing.T) {
	raw := manyScanCMYKJPEG(64, 64, 2000)
	if _, format, err := image.DecodeConfig(bytes.NewReader(raw)); err != nil || format != "jpeg" {
		t.Fatalf("the crafted file is not a JPEG Go reads: %v", err)
	}
	data := base64.StdEncoding.EncodeToString(raw)
	content, err := ImageResultContent("read_image", imageResultJSON("image/jpeg", data), ImagePolicy{MaxImageBase64Bytes: len(data) / 2})
	var tooLarge *ImageTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("read_image over the bound: err = %v, content = %d blocks — want *ImageTooLargeError, no reduction", err, len(content))
	}
	if !strings.Contains(err.Error(), "read that file") || strings.Contains(err.Error(), "could not be reduced") {
		t.Errorf("err = %q, want read_image's advice and no claim of a reduction attempt", err)
	}
}

// transparentPNG is a w×h PNG, fully transparent but for an opaque square.
func transparentPNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h/4; y++ {
		for x := 0; x < w/4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// A capture with transparency, reduced, is flattened onto white: the note
// the model reads says so, or white areas read as the capture's own.
func TestImageResultContent_ReductionNoteSaysTransparencyWasFlattened(t *testing.T) {
	data := transparentPNG(t, 400, 300)
	content, err := ImageResultContent("screenshot", imageResultJSON("image/png", data), ImagePolicy{MaxImageBase64Bytes: len(data) - 1})
	if err != nil || len(content) != 3 {
		t.Fatalf("reduce: err = %v, %d blocks", err, len(content))
	}
	if !strings.Contains(content[2].Text, "transparen") {
		t.Errorf("note = %q, want it to say transparency was flattened onto white", content[2].Text)
	}
	opaque := noisePNG(t, 400, 300)
	content, err = ImageResultContent("screenshot", imageResultJSON("image/png", opaque), ImagePolicy{MaxImageBase64Bytes: len(opaque) / 2})
	if err != nil || len(content) != 3 || strings.Contains(content[2].Text, "transparen") {
		t.Fatalf("an opaque capture: err = %v, note %q — want no transparency claim", err, content[len(content)-1].Text)
	}
}

// A gateway route serving a Claude model meets Anthropic's limits: the
// family is the model's, not the routing prefix's.
func TestRouteImagePolicy_AGatewayServingClaudeGetsTheAnthropicLimits(t *testing.T) {
	for _, spec := range []string{"openai_compatible/claude-sonnet-4-6", "openai_compatible/anthropic/claude-opus-5-5", "openai_compatible/Claude-Haiku"} {
		p, err := RouteImagePolicy(spec, func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		if p.MaxImageBase64Bytes != 5<<20 || p.MaxRequestImageBytes != 24<<20 {
			t.Errorf("%s: policy = %+v, want the Anthropic family's bounds", spec, p)
		}
	}
}

func grayPNG(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// The longer side is bounded from the header alone, under the byte bound
// too: read_image is refused with its advice and the side's escape hatch, a
// capture is halved until it fits.
func TestImageResultContent_BoundsTheLongerSide(t *testing.T) {
	wide := grayPNG(t, 9000, 200)
	policy := ImagePolicy{MaxImageBase64Bytes: 5 << 20, MaxImageSide: 8000}
	_, err := ImageResultContent("read_image", imageResultJSON("image/png", wide), policy)
	var tooLarge *ImageTooLargeError
	if !errors.As(err, &tooLarge) || !strings.Contains(err.Error(), "9000x200") || !strings.Contains(err.Error(), EnvImageMaxSide) || tooLarge.Cause != nil {
		t.Fatalf("read_image 9000x200: err = %v, want a side refusal naming %s, no reduction tried", err, EnvImageMaxSide)
	}
	content, err := ImageResultContent("screenshot", imageResultJSON("image/png", wide), policy)
	if err != nil || len(content) != 3 {
		t.Fatalf("screenshot 9000x200: err = %v, %d blocks, want it reduced", err, len(content))
	}
	if w, h, _, ok := ImageHeader(content[1].Source); !ok || w > 8000 || h > 8000 {
		t.Fatalf("reduced capture is %dx%d (ok=%t), want its sides within 8000", w, h, ok)
	}
	if _, err := ImageResultContent("read_image", imageResultJSON("image/png", grayPNG(t, 8000, 10)), policy); err != nil {
		t.Fatalf("an image at the side bound was refused: %v", err)
	}
}

func TestRouteImagePolicy_SideCountAndDecodeBoundsWithEscapeHatches(t *testing.T) {
	none := func(string) string { return "" }
	p, err := RouteImagePolicy("anthropic/claude-sonnet-4-6", none)
	if err != nil || p.MaxImageSide != 8000 || p.MaxRequestImages != 100 || p.MaxDecodePixels != 50_000_000 {
		t.Fatalf("anthropic: policy = %+v, err = %v, want side 8000, 100 images, 50 MP decode", p, err)
	}
	p, err = RouteImagePolicy("openai_compatible/pixtral-12b", none)
	if err != nil || p.MaxImageSide != 0 || p.MaxRequestImages != 0 {
		t.Fatalf("gateway: policy = %+v, err = %v, want no side or count bound", p, err)
	}
	env := map[string]string{EnvImageMaxSide: "0", EnvRequestImageMaxCount: "7", EnvImageMaxDecodePixels: "123"}
	p, err = RouteImagePolicy("anthropic/claude-sonnet-4-6", func(k string) string { return env[k] })
	if err != nil || p.MaxImageSide != 0 || p.MaxRequestImages != 7 || p.MaxDecodePixels != 123 {
		t.Fatalf("overrides: policy = %+v, err = %v", p, err)
	}
}

// A per-image bound over the per-request one would let no such image ever
// be sent: the configuration is refused, naming both escape hatches.
func TestRouteImagePolicy_RefusesAPerImageBoundOverThePerRequestOne(t *testing.T) {
	env := map[string]string{EnvRequestImageMaxBytes: "1000"}
	_, err := RouteImagePolicy("anthropic/claude-sonnet-4-6", func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), EnvImageMaxBase64Bytes) || !strings.Contains(err.Error(), EnvRequestImageMaxBytes) {
		t.Fatalf("err = %v, want a refusal naming both bounds", err)
	}
}
