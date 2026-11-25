package tool

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // registers the GIF header and decoder image.DecodeConfig reads
	"image/jpeg"
	_ "image/png" // registers the PNG header and decoder image.DecodeConfig reads
	"strings"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// fitJPEGQuality is the quality an oversized image is re-encoded at: a
// photographic QHD screenshot lands near 1 MB, text stays legible.
const fitJPEGQuality = 85

// fitMinSide is the smallest side the reduction halves an image down to.
// Below it the image no longer carries what the model was asked to read.
const fitMinSide = 64

// headerPrefixBase64 is how much of an image's base64 ImageHeader decodes:
// enough for the header of a PNG or GIF, and for a JPEG's frame header
// behind the usual EXIF and ICC segments.
const headerPrefixBase64 = 1 << 20

// ImageHeader reads an image's dimensions from its header alone — a
// bounded base64 prefix, image.DecodeConfig, no pixel decoded. ok is false
// for a source that is not base64 data, or a header Go cannot read (a
// format other than PNG, JPEG and GIF, or a JPEG frame header past the
// prefix).
func ImageHeader(src *api.ImageSource) (width, height int, format string, ok bool) {
	if src == nil || src.Type != "base64" {
		return 0, 0, "", false
	}
	data := src.Data
	if len(data) > headerPrefixBase64 {
		data = data[:headerPrefixBase64]
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return 0, 0, "", false
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0, "", false
	}
	return cfg.Width, cfg.Height, format, true
}

// fitImageBase64 reduces a base64 image the tool loop produced itself (a
// capture, never untrusted bytes) until its longer side is at most maxSide
// and its base64 at most maxBytes (0: no bound): halved, box-filtered, to
// the side, then re-encoded as JPEG (transparency flattened onto white) and
// halved again until it fits. The header is read first: non-positive
// dimensions, or more than maxPixels (0: no bound), refuse the image before
// any decode. It returns the reduced source and a description of what was
// done. An image Go cannot decode (PNG, JPEG and GIF are read), or one that
// does not fit before a side falls under fitMinSide, is an error.
func fitImageBase64(src *api.ImageSource, maxBytes, maxSide int, maxPixels int64) (*api.ImageSource, string, error) {
	if src.Type != "base64" {
		return nil, "", fmt.Errorf("source %q is not base64 data", src.Type)
	}
	raw, err := base64.StdEncoding.DecodeString(src.Data)
	if err != nil {
		return nil, "", fmt.Errorf("decode base64: %w", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("decode %s header: %w", src.MediaType, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", fmt.Errorf("header declares %dx%d pixels", cfg.Width, cfg.Height)
	}
	if px := int64(cfg.Width) * int64(cfg.Height); maxPixels > 0 && px > maxPixels {
		return nil, "", fmt.Errorf("header declares %dx%d = %d pixels, over the %d this reduction decodes (%s)", cfg.Width, cfg.Height, px, maxPixels, EnvImageMaxDecodePixels)
	}
	decoded, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("decode %s: %w", src.MediaType, err)
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
			return fmt.Errorf("still over the bound at %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
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
			desc := []string{fmt.Sprintf("%dx%d %s → %dx%d JPEG, %d bytes of base64", b.Dx(), b.Dy(), format, img.Bounds().Dx(), img.Bounds().Dy(), n)}
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
