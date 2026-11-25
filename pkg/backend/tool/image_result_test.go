package tool

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func TestImageResultContent_DecodesDescriptionAndImage(t *testing.T) {
	content, err := ImageResultContent("read_image", `{"description":"schema.png","blocks":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}`, ImagePolicy{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(content) != 2 || content[0].Type != "text" || content[0].Text != "schema.png" {
		t.Fatalf("content = %+v, want the description then the image", content)
	}
	img := content[1]
	if img.Type != "image" || img.Source == nil || img.Source.Data != "QUJD" || img.Source.MediaType != "image/png" {
		t.Fatalf("image block = %+v", img)
	}
}

func TestImageResultContent_AcceptsADescriptionAlone(t *testing.T) {
	content, err := ImageResultContent("read_image", `{"description":"left_click at (1, 2)"}`, ImagePolicy{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(content) != 1 || content[0].Text != "left_click at (1, 2)" {
		t.Fatalf("content = %+v", content)
	}
}

func TestImageResultContent_RefusesWhatItCannotCarry(t *testing.T) {
	for name, output := range map[string]string{
		"not json":         `iVBORw0KGgo`,
		"empty":            `{}`,
		"unknown block":    `{"description":"x","blocks":[{"type":"audio"}]}`,
		"image, no source": `{"description":"x","blocks":[{"type":"image"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ImageResultContent("read_image", output, ImagePolicy{}); err == nil {
				t.Errorf("ImageResultContent(%q) = nil error", output)
			}
		})
	}
}

func TestImageResultTrait_OnlyTheImageToolsCarryIt(t *testing.T) {
	want := map[string]bool{"read_image": true, "screenshot": true, "computer_use": true}
	images := NewRegistry()
	if err := RegisterClawReadImage(images, NewReadScope(t.TempDir())); err != nil {
		t.Fatalf("register read_image: %v", err)
	}
	if err := RegisterClawComputerUse(images); err != nil {
		t.Fatalf("register computer use: %v", err)
	}
	builtins := NewRegistry()
	if err := RegisterClawBuiltins(builtins, t.TempDir()); err != nil {
		t.Fatalf("register builtins: %v", err)
	}
	seen := 0
	for _, reg := range []*Registry{images, builtins} {
		for _, td := range reg.List() {
			seen++
			if got := td.ToDelegateDef().ImageResult; got != want[td.QualifiedName] {
				t.Errorf("%s: ImageResult = %t, want %t", td.QualifiedName, got, want[td.QualifiedName])
			}
		}
	}
	if seen < 6 {
		t.Fatalf("only %d tools registered — the check saw too little to mean anything", seen)
	}
}

// imageResultJSON is an image tool's output carrying one base64 image.
func imageResultJSON(mediaType, data string) string {
	return `{"description":"capture","blocks":[{"type":"image","source":{"type":"base64","media_type":"` + mediaType + `","data":"` + data + `"}}]}`
}

// noisePNG is a w×h PNG of random pixels: PNG cannot compress it, so its
// size is the bound the test means, not a compressible stand-in.
func noisePNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if _, err := rand.Read(img.Pix); err != nil {
		t.Fatalf("random pixels: %v", err)
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xff // opaque: a capture has no transparency
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// An image at the route's per-image bound passes as is; one over it that Go
// can decode is reduced to fit, and the model is told it was.
func TestImageResultContent_ReducesAnImageOverTheRouteBound(t *testing.T) {
	data := noisePNG(t, 1200, 900)
	policy := ImagePolicy{MaxImageBase64Bytes: len(data)}
	content, err := ImageResultContent("screenshot", imageResultJSON("image/png", data), policy)
	if err != nil || len(content) != 2 || content[1].Source.Data != data {
		t.Fatalf("an image at the bound was not passed as is: err=%v content=%d blocks", err, len(content))
	}

	policy.MaxImageBase64Bytes = len(data) / 4
	content, err = ImageResultContent("screenshot", imageResultJSON("image/png", data), policy)
	if err != nil {
		t.Fatalf("an oversized decodable capture was refused: %v", err)
	}
	if len(content) != 3 || content[1].Type != "image" || content[2].Type != "text" {
		t.Fatalf("content = %d blocks, want the description, the reduced image, and the note saying so", len(content))
	}
	got := content[1].Source
	if got.MediaType != "image/jpeg" || len(got.Data) > policy.MaxImageBase64Bytes {
		t.Fatalf("reduced image is %s, %d bytes of base64, want a JPEG within %d", got.MediaType, len(got.Data), policy.MaxImageBase64Bytes)
	}
	raw, err := base64.StdEncoding.DecodeString(got.Data)
	if err != nil {
		t.Fatalf("reduced image is not base64: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("reduced image is not a JPEG: %v", err)
	}
	if !strings.Contains(content[2].Text, "reduced") {
		t.Errorf("note = %q, want it to say the image was reduced", content[2].Text)
	}
}

// An oversized image that cannot be reduced is a typed error whose advice
// fits its producer: a file can be converted, a capture cannot.
func TestImageResultContent_AdvisesEachProducerOnAnIrreducibleImage(t *testing.T) {
	output := imageResultJSON("image/webp", strings.Repeat("A", 4096))
	policy := ImagePolicy{MaxImageBase64Bytes: 1024}
	for name, want := range map[string]string{
		"read_image":   "read that file",
		"screenshot":   "lower the display resolution",
		"computer_use": "lower the display resolution",
	} {
		_, err := ImageResultContent(name, output, policy)
		var tooLarge *ImageTooLargeError
		if !errors.As(err, &tooLarge) {
			t.Fatalf("%s: err = %v, want *ImageTooLargeError", name, err)
		}
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), EnvImageMaxBase64Bytes) {
			t.Errorf("%s: err = %q, want the advice %q and the escape hatch", name, err, want)
		}
	}
}

// A capture whose header declares more pixels than a reduction decodes is
// refused from its header, before any full decode — a few-byte PNG can
// declare a 40 GB buffer — through the producer's typed error. The bound is
// lowered to 1 megapixel so the image stays small: without the check the
// 2000×2000 zero image decodes (16 MB) and fits, and the test reddens.
func TestImageResultContent_RefusesACaptureDeclaringTooManyPixels(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 2000, 2000))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	_, err := ImageResultContent("screenshot", imageResultJSON("image/png", data), ImagePolicy{MaxImageBase64Bytes: len(data) - 1, MaxDecodePixels: 1_000_000})
	var tooLarge *ImageTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want *ImageTooLargeError refusing the declared pixels", err)
	}
	if !strings.Contains(err.Error(), "4000000 pixels") || !strings.Contains(err.Error(), EnvImageMaxDecodePixels) || !strings.Contains(err.Error(), "lower the display resolution") {
		t.Errorf("err = %q, want the declared pixel count, its escape hatch and the capture's advice", err)
	}
}

// A text-only receiver gets the marker in place of the image — whatever its
// size, and without the image bytes.
func TestImageResultContent_TextOnlyReceiverGetsTheMarker(t *testing.T) {
	data := strings.Repeat("A", 4096)
	content, err := ImageResultContent("read_image", imageResultJSON("image/png", data), ImagePolicy{TextOnly: true, MaxImageBase64Bytes: 16})
	if err != nil {
		t.Fatalf("a text-only receiver was refused an image: %v", err)
	}
	if len(content) != 2 || content[1].Type != "text" || content[1].Text != TextOnlyImageMarker {
		t.Fatalf("content = %+v, want the description then the marker", content)
	}
}

// A route known to refuse images turns a tool image into a typed error.
func TestImageResultContent_RefusingRouteIsATypedError(t *testing.T) {
	_, err := ImageResultContent("read_image", imageResultJSON("image/png", "QUJD"), ImagePolicy{Route: "openai_compatible/glm", Acceptance: ImagesRefused})
	var refused *ImagesRefusedError
	if !errors.As(err, &refused) || refused.Route != "openai_compatible/glm" {
		t.Fatalf("err = %v, want *ImagesRefusedError naming the route", err)
	}
}

func TestRouteImagePolicy_BoundsPerRouteFamilyWithAnEscapeHatch(t *testing.T) {
	none := func(string) string { return "" }
	for spec, want := range map[string][2]int{
		"anthropic/claude-sonnet-4-6":     {5 << 20, 24 << 20},
		"bedrock/anthropic.claude-opus":   {5 << 20, 24 << 20},
		"vertex/claude-opus-5-5":          {5 << 20, 24 << 20},
		"claude-opus-5-5":                 {5 << 20, 24 << 20},
		"openai_compatible/pixtral-12b":   {20 << 20, 40 << 20},
		"openai/gpt-5":                    {20 << 20, 40 << 20},
		"meta-llama/Llama-3.3-70B-Vision": {20 << 20, 40 << 20},
	} {
		p, err := RouteImagePolicy(spec, none)
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if p.MaxImageBase64Bytes != want[0] || p.MaxRequestImageBytes != want[1] || p.Route != spec {
			t.Errorf("%s: policy = %+v, want per-image %d, per-request %d", spec, p, want[0], want[1])
		}
	}

	env := map[string]string{EnvImageMaxBase64Bytes: "0", EnvRequestImageMaxBytes: "1000"}
	p, err := RouteImagePolicy("anthropic/claude-sonnet-4-6", func(k string) string { return env[k] })
	if err != nil || p.MaxImageBase64Bytes != 0 || p.MaxRequestImageBytes != 1000 {
		t.Fatalf("override: policy = %+v, err = %v, want no per-image bound and 1000 per request", p, err)
	}
	for _, bad := range []string{"-1", "5MB", "lots"} {
		if _, err := RouteImagePolicy("openai/gpt-5", func(k string) string {
			if k == EnvRequestImageMaxBytes {
				return bad
			}
			return ""
		}); err == nil || !strings.Contains(err.Error(), EnvRequestImageMaxBytes) {
			t.Errorf("%s=%q: err = %v, want an error naming the variable", EnvRequestImageMaxBytes, bad, err)
		}
	}
}
