package tool

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"strings"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// fitJPEGQuality is the quality an oversized image is re-encoded at: a
// photographic QHD screenshot lands near 1 MB, text stays legible.
const fitJPEGQuality = 85

// fitMinSide is the smallest side the reduction halves an image down to.
// Below it the image no longer carries what the model was asked to read.
const fitMinSide = 64

// headerPrefixBytes bounds the bytes ImageHeader reads: enough for a
// JPEG's frame header behind the usual EXIF and ICC segments, and nothing
// a crafted file can amplify.
const headerPrefixBytes = 768 << 10

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// ImageHeader reads an image's format and dimensions from its header alone,
// never a pixel: a PNG's from its IHDR (the first chunk, by the format's
// rule) without walking the chunks after it, a WebP's from its first chunk
// (VP8X, VP8L or VP8), a JPEG's through its DecodeConfig and a GIF's from its
// logical screen and first frame (the larger of the two), over a stream of
// at most headerPrefixBytes. format names the format the magic bytes
// declare even when the size cannot be read; ok says the size was read. A
// caller treats a known format of unreadable size as over any side bound:
// the image is one the route may refuse.
func ImageHeader(src *api.ImageSource) (width, height int, format string, ok bool) {
	if src == nil || src.Type != "base64" {
		return 0, 0, "", false
	}
	r := bufio.NewReaderSize(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(src.Data)), headerPrefixBytes), 64)
	head, _ := r.Peek(30)
	le24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
	switch {
	case len(head) >= 8 && bytes.Equal(head[:8], pngSignature):
		if len(head) < 24 || string(head[12:16]) != "IHDR" || binary.BigEndian.Uint32(head[8:12]) != 13 {
			return 0, 0, "png", false
		}
		w, h := binary.BigEndian.Uint32(head[16:20]), binary.BigEndian.Uint32(head[20:24])
		if w > 1<<31-1 || h > 1<<31-1 {
			return 0, 0, "png", false
		}
		return int(w), int(h), "png", true
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		if len(head) < 30 {
			return 0, 0, "webp", false
		}
		switch string(head[12:16]) {
		case "VP8X":
			return 1 + le24(head[24:27]), 1 + le24(head[27:30]), "webp", true
		case "VP8L":
			if head[20] != 0x2f {
				return 0, 0, "webp", false
			}
			bits := binary.LittleEndian.Uint32(head[21:25])
			return 1 + int(bits&0x3fff), 1 + int(bits>>14&0x3fff), "webp", true
		case "VP8 ":
			if head[23] != 0x9d || head[24] != 0x01 || head[25] != 0x2a {
				return 0, 0, "webp", false
			}
			return int(binary.LittleEndian.Uint16(head[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(head[28:30]) & 0x3fff), "webp", true
		}
		return 0, 0, "webp", false
	case len(head) >= 2 && head[0] == 0xff && head[1] == 0xd8:
		cfg, err := jpeg.DecodeConfig(r)
		if err != nil {
			return 0, 0, "jpeg", false
		}
		return cfg.Width, cfg.Height, "jpeg", true
	case len(head) >= 6 && (string(head[:6]) == "GIF87a" || string(head[:6]) == "GIF89a"):
		w, h, ok := gifHeader(r)
		return w, h, "gif", ok
	}
	return 0, 0, "", false
}

// gifHeader reads a GIF's logical screen and its first frame's size, and
// returns the larger of each: a frame may exceed the screen it is declared
// on, and a decoder draws the frame.
func gifHeader(r *bufio.Reader) (width, height int, ok bool) {
	var lsd [13]byte
	if _, err := io.ReadFull(r, lsd[:]); err != nil {
		return 0, 0, false
	}
	width, height = int(binary.LittleEndian.Uint16(lsd[6:8])), int(binary.LittleEndian.Uint16(lsd[8:10]))
	if lsd[10]&0x80 != 0 {
		if _, err := r.Discard(3 << (int(lsd[10]&7) + 1)); err != nil {
			return 0, 0, false
		}
	}
	for {
		intro, err := r.ReadByte()
		if err != nil {
			return 0, 0, false
		}
		switch intro {
		case 0x2c: // image descriptor
			var d [9]byte
			if _, err := io.ReadFull(r, d[:]); err != nil {
				return 0, 0, false
			}
			fw := int(binary.LittleEndian.Uint16(d[0:2])) + int(binary.LittleEndian.Uint16(d[4:6]))
			fh := int(binary.LittleEndian.Uint16(d[2:4])) + int(binary.LittleEndian.Uint16(d[6:8]))
			return max(width, fw), max(height, fh), true
		case 0x21: // extension: a label, then data sub-blocks
			if _, err := r.ReadByte(); err != nil {
				return 0, 0, false
			}
			for {
				n, err := r.ReadByte()
				if err != nil {
					return 0, 0, false
				}
				if n == 0 {
					break
				}
				if _, err := r.Discard(int(n)); err != nil {
					return 0, 0, false
				}
			}
		default:
			return 0, 0, false
		}
	}
}

// fitImageBase64 reduces a capture the tool loop produced itself — a PNG,
// never untrusted bytes — until its longer side is at most maxSide and its
// base64 at most maxBytes (0: no bound): halved, box-filtered, to the side,
// then re-encoded as JPEG (transparency flattened onto white) and halved
// again until it fits. The header is read first: bytes that are not a PNG,
// non-positive dimensions, or more than maxPixels (0: no bound) refuse the
// image before any decode. It returns the reduced source and a description
// of what was done. One that does not fit before a side falls under
// fitMinSide is an error.
func fitImageBase64(src *api.ImageSource, maxBytes, maxSide int, maxPixels int64) (*api.ImageSource, string, error) {
	w, h, format, ok := ImageHeader(src)
	if !ok || format != "png" {
		return nil, "", fmt.Errorf("the capture is not a PNG (header %q, readable=%t): it is not decoded", format, ok)
	}
	if w <= 0 || h <= 0 {
		return nil, "", fmt.Errorf("header declares %dx%d pixels", w, h)
	}
	if px := int64(w) * int64(h); maxPixels > 0 && px > maxPixels {
		return nil, "", fmt.Errorf("header declares %dx%d = %d pixels, over the %d this reduction decodes (%s)", w, h, px, maxPixels, EnvImageMaxDecodePixels)
	}
	raw, err := base64.StdEncoding.DecodeString(src.Data)
	if err != nil {
		return nil, "", fmt.Errorf("decode base64: %w", err)
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("decode png: %w", err)
	}
	b := decoded.Bounds()
	opaque := false
	if o, ok := decoded.(interface{ Opaque() bool }); ok {
		opaque = o.Opaque()
	}
	img := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(img, img.Bounds(), decoded, b.Min, draw.Over)
	halveOnce := func() error {
		w, h := img.Bounds().Dx()/2, img.Bounds().Dy()/2
		if w < fitMinSide || h < fitMinSide {
			return fmt.Errorf("the reduction stopped at the %d-pixel minimum side, at %dx%d, still over the route's bound", fitMinSide, img.Bounds().Dx(), img.Bounds().Dy())
		}
		img = halve(img, w, h)
		return nil
	}
	for maxSide > 0 && max(img.Bounds().Dx(), img.Bounds().Dy()) > maxSide {
		if err := halveOnce(); err != nil {
			return nil, "", err
		}
	}
	for {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: fitJPEGQuality}); err != nil {
			return nil, "", fmt.Errorf("encode jpeg: %w", err)
		}
		if n := base64.StdEncoding.EncodedLen(buf.Len()); maxBytes <= 0 || n <= maxBytes {
			desc := []string{fmt.Sprintf("%dx%d %s → %dx%d JPEG, %d bytes of base64", b.Dx(), b.Dy(), "png", img.Bounds().Dx(), img.Bounds().Dy(), n)}
			if !opaque {
				desc = append(desc, "transparency flattened onto white")
			}
			return &api.ImageSource{Type: "base64", MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(buf.Bytes())}, strings.Join(desc, "; "), nil
		}
		if err := halveOnce(); err != nil {
			return nil, "", err
		}
	}
}

// halve box-filters img down to w×h, each output pixel the mean of the 2×2
// block it covers.
func halve(img *image.RGBA, w, h int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, bl, a uint32
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					c := img.RGBAAt(2*x+dx, 2*y+dy)
					r, g, bl, a = r+uint32(c.R), g+uint32(c.G), bl+uint32(c.B), a+uint32(c.A)
				}
			}
			out.SetRGBA(x, y, color.RGBA{R: uint8(r / 4), G: uint8(g / 4), B: uint8(bl / 4), A: uint8(a / 4)})
		}
	}
	return out
}
