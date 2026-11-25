package tool

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

func webpSource(chunk string, payload []byte) *api.ImageSource {
	var b bytes.Buffer
	b.WriteString("RIFF\x00\x00\x00\x00WEBP" + chunk)
	b.Write([]byte{byte(len(payload)), 0, 0, 0})
	b.Write(payload)
	return &api.ImageSource{Type: "base64", MediaType: "image/webp", Data: base64.StdEncoding.EncodeToString(b.Bytes())}
}

// The lossless and lossy WebP headers read their 2500×1500 canvas.
func TestImageHeader_ReadsLosslessAndLossyWebP(t *testing.T) {
	var lossless [5]byte
	lossless[0] = 0x2f
	binary.LittleEndian.PutUint32(lossless[1:], uint32(2500-1)|uint32(1500-1)<<14)
	lossy := []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(lossy[6:], 2500)
	binary.LittleEndian.PutUint16(lossy[8:], 1500)
	for name, src := range map[string]*api.ImageSource{
		"VP8L": webpSource("VP8L", append(lossless[:], make([]byte, 8)...)),
		"VP8":  webpSource("VP8 ", append(lossy, make([]byte, 8)...)),
	} {
		w, h, format, ok := ImageHeader(src)
		if !ok || w != 2500 || h != 1500 || format != "webp" {
			t.Errorf("%s: header = %dx%d %q ok=%t, want 2500x1500 webp", name, w, h, format, ok)
		}
	}
}

// lateFrameJPEG is an 8100×64 JPEG whose frame header sits past the bytes
// a header read takes, behind APP15 padding segments.
func lateFrameJPEG(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 8100, 64)), nil); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	var out bytes.Buffer
	out.Write(raw[:2])
	pad := make([]byte, 65000)
	for out.Len() < headerPrefixBytes+4096 {
		jpegSegment(&out, 0xef, pad)
	}
	out.Write(raw[2:])
	return base64.StdEncoding.EncodeToString(out.Bytes())
}

// A known format whose size cannot be read counts as over the side bound:
// read_image is refused rather than sending an image the route may refuse.
func TestImageResultContent_AnUnreadableSizeIsOverTheSide(t *testing.T) {
	data := lateFrameJPEG(t)
	if _, _, format, ok := ImageHeader(&api.ImageSource{Type: "base64", Data: data}); ok || format != "jpeg" {
		t.Fatalf("header ok=%t format=%q, want a JPEG whose size is unreadable", ok, format)
	}
	_, err := ImageResultContent("read_image", imageResultJSON("image/jpeg", data), ImagePolicy{MaxImageBase64Bytes: 20 << 20, MaxImageSide: 8000})
	var tooLarge *ImageTooLargeError
	if !errors.As(err, &tooLarge) || !strings.Contains(err.Error(), "read that file") {
		t.Fatalf("err = %v, want a refusal advising to convert the file", err)
	}
	if _, err := ImageResultContent("read_image", imageResultJSON("image/jpeg", data), ImagePolicy{MaxImageBase64Bytes: 20 << 20}); err != nil {
		t.Errorf("with no side bound the image was refused: %v", err)
	}
}

// The computer_use refusal names screenshot, the capture path that reduces;
// a screenshot reduction stopped at the minimum side says so.
func TestImageResultContent_LargeCaptureErrorsNameTheWayOut(t *testing.T) {
	data := noisePNG(t, 400, 300)
	_, err := ImageResultContent("computer_use", imageResultJSON("image/png", data), ImagePolicy{MaxImageBase64Bytes: len(data) / 2})
	if err == nil || !strings.Contains(err.Error(), "screenshot") {
		t.Errorf("computer_use refusal = %v, want it to name screenshot", err)
	}
	_, err = ImageResultContent("screenshot", imageResultJSON("image/png", grayPNG(t, 8100, 100)), ImagePolicy{MaxImageSide: 8000})
	var tooLarge *ImageTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Cause == nil || !strings.Contains(tooLarge.Cause.Error(), "minimum side") {
		t.Errorf("8100x100 screenshot: err = %v, want the cause to say the reduction stopped at the minimum side", err)
	}
}

// A GIF frame larger than its logical screen is what a decoder draws: the
// header read reports the frame.
func TestImageHeader_AGIFFrameLargerThanItsScreen(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("GIF89a")
	b.Write([]byte{100, 0, 100, 0, 0, 0, 0})
	b.Write([]byte{0x21, 0xf9, 4, 0, 0, 0, 0, 0}) // a graphic control extension
	b.Write([]byte{0x2c, 0, 0, 0, 0, 0x28, 0x23, 0x28, 0x23, 0})
	w, h, format, ok := ImageHeader(&api.ImageSource{Type: "base64", Data: base64.StdEncoding.EncodeToString(b.Bytes())})
	if !ok || w != 9000 || h != 9000 || format != "gif" {
		t.Fatalf("header = %dx%d %q ok=%t, want the 9000x9000 frame", w, h, format, ok)
	}
}
