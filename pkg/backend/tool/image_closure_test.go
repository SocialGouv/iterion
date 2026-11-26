package tool

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// palettedStorm is a valid 16×16 paletted PNG whose IHDR is followed by n
// empty ancillary chunks before its PLTE: Go's png.DecodeConfig walks every
// chunk up to PLTE and allocates a buffer per chunk.
func palettedStorm(n int) []byte {
	var b bytes.Buffer
	chunk := func(typ string, data []byte) {
		var hdr [8]byte
		binary.BigEndian.PutUint32(hdr[:4], uint32(len(data)))
		copy(hdr[4:], typ)
		b.Write(hdr[:])
		b.Write(data)
		c := crc32.NewIEEE()
		c.Write([]byte(typ))
		c.Write(data)
		var crc [4]byte
		binary.BigEndian.PutUint32(crc[:], c.Sum32())
		b.Write(crc[:])
	}
	b.WriteString("\x89PNG\r\n\x1a\n")
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:], 16)
	binary.BigEndian.PutUint32(ihdr[4:], 16)
	ihdr[8], ihdr[9] = 8, 3
	chunk("IHDR", ihdr[:])
	for i := 0; i < n; i++ {
		chunk("abCd", nil)
	}
	chunk("PLTE", []byte{255, 0, 0})
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(make([]byte, 16*17))
	zw.Close()
	chunk("IDAT", z.Bytes())
	chunk("IEND", nil)
	return b.Bytes()
}

// A header read is cheap whatever follows the header: a PNG's size is read
// from its IHDR, never by walking its chunks. The storm here allocates
// ~16 MB through png.DecodeConfig; the IHDR read allocates almost nothing.
func TestImageHeader_ReadsAPNGFromItsIHDRAlone(t *testing.T) {
	raw := palettedStorm(4000)
	if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("the storm is not a valid PNG: %v", err)
	}
	src := &api.ImageSource{Type: "base64", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(raw)}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	w, h, format, ok := ImageHeader(src)
	runtime.ReadMemStats(&after)
	if !ok || w != 16 || h != 16 || format != "png" {
		t.Fatalf("header = %dx%d %q ok=%t, want 16x16 png", w, h, format, ok)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 256<<10 {
		t.Errorf("the header read allocated %d bytes, want it bounded (the chunks after IHDR are not walked)", alloc)
	}
}

// A WebP header is read too: its format decides the media type and its
// canvas the side bound.
func TestImageHeader_ReadsAWebPCanvas(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("RIFF\x00\x00\x00\x00WEBPVP8X")
	b.Write([]byte{10, 0, 0, 0, 0, 0, 0, 0})
	b.Write([]byte{0x27, 0x23, 0x00}) // width-1 = 8999
	b.Write([]byte{0x63, 0x00, 0x00}) // height-1 = 99
	src := &api.ImageSource{Type: "base64", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(b.Bytes())}
	w, h, format, ok := ImageHeader(src)
	if !ok || w != 9000 || h != 100 || format != "webp" {
		t.Fatalf("header = %dx%d %q ok=%t, want 9000x100 webp", w, h, format, ok)
	}
}

func jpegBase64(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// The media type an image goes out under is the one its bytes declare: a
// JPEG named .png sent as image/png fails every later request.
func TestImageResultContent_TheHeaderDecidesTheMediaType(t *testing.T) {
	content, err := ImageResultContent("read_image", imageResultJSON("image/png", jpegBase64(t, 64, 64)), ImagePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got := content[1].Source.MediaType; got != "image/jpeg" {
		t.Errorf("media type = %q, want image/jpeg from the bytes", got)
	}
	content, err = ImageResultContent("read_image", imageResultJSON("image/png", "QUJD"), ImagePolicy{})
	if err != nil || content[1].Source.MediaType != "image/png" {
		t.Errorf("an unreadable header: media type = %q, err = %v — want the producer's kept", content[1].Source.MediaType, err)
	}
}

// A computer_use capture is never reduced: a reduced capture changes the
// coordinate space the model clicks in, and the tool does not rescale.
func TestImageResultContent_NeverReducesAComputerUseCapture(t *testing.T) {
	data := noisePNG(t, 400, 300)
	_, err := ImageResultContent("computer_use", imageResultJSON("image/png", data), ImagePolicy{MaxImageBase64Bytes: len(data) / 2})
	var tooLarge *ImageTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Cause != nil || !strings.Contains(err.Error(), "lower the display resolution") {
		t.Fatalf("err = %v, want a refusal with no reduction tried, advising a lower resolution", err)
	}
}

// A capture is a PNG the tool encodes: one whose bytes are anything else is
// refused from its header, never decoded.
func TestImageResultContent_ACaptureThatIsNotAPNGIsNotDecoded(t *testing.T) {
	data := jpegBase64(t, 600, 400)
	_, err := ImageResultContent("screenshot", imageResultJSON("image/png", data), ImagePolicy{MaxImageBase64Bytes: len(data) / 2})
	var tooLarge *ImageTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Cause == nil || !strings.Contains(tooLarge.Cause.Error(), "is not decoded") {
		t.Fatalf("err = %v, want a refusal from the header, saying the capture is not a PNG and is not decoded", err)
	}
}

// One global escape hatch valid for one route family never fails a
// generation of the other: the per-image bound is held to the per-request
// one where an image is shaped, naming both knobs.
func TestRouteImagePolicy_ConflictingKnobsNeverFailAGeneration(t *testing.T) {
	for spec, env := range map[string]map[string]string{
		"anthropic/claude-sonnet-4-6": {EnvImageMaxBase64Bytes: "30000000"},
		"openai/gpt-5":                {EnvRequestImageMaxBytes: "10000000"},
	} {
		if _, err := RouteImagePolicy(spec, func(k string) string { return env[k] }); err != nil {
			t.Errorf("%s with %v: %v, want the generation let through", spec, env, err)
		}
	}
	_, err := ImageResultContent("read_image", imageResultJSON("image/png", strings.Repeat("A", 700)), ImagePolicy{MaxImageBase64Bytes: 1000, MaxRequestImageBytes: 500})
	var tooLarge *ImageTooLargeError
	if !errors.As(err, &tooLarge) || !strings.Contains(err.Error(), EnvImageMaxBase64Bytes) || !strings.Contains(err.Error(), EnvRequestImageMaxBytes) {
		t.Fatalf("an image over the per-request budget, under the per-image bound: err = %v, want a refusal naming both knobs", err)
	}
}
