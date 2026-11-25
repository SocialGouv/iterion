package tool

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // registers the GIF decoder image.Decode reads
	"image/jpeg"
	_ "image/png" // registers the PNG decoder image.Decode reads

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// fitJPEGQuality is the quality an oversized image is re-encoded at: a
// photographic QHD screenshot lands near 1 MB, text stays legible.
const fitJPEGQuality = 85

// fitMinSide is the smallest side the reduction halves an image down to.
// Below it the image no longer carries what the model was asked to read.
const fitMinSide = 64

// fitImageBase64 reduces a base64 image to at most max bytes of base64: a
// JPEG re-encode at full size first (transparency flattened onto white),
// then the image halved, box-filtered, until it fits. It returns the reduced
// source and a description of what was done. An image Go cannot decode
// (PNG, JPEG and GIF are read), or one that does not fit before a side falls
// under fitMinSide, is an error.
func fitImageBase64(src *api.ImageSource, max int) (*api.ImageSource, string, error) {
	if src.Type != "base64" {
		return nil, "", fmt.Errorf("source %q is not base64 data", src.Type)
	}
	raw, err := base64.StdEncoding.DecodeString(src.Data)
	if err != nil {
		return nil, "", fmt.Errorf("decode base64: %w", err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("decode %s: %w", src.MediaType, err)
	}
	b := decoded.Bounds()
	img := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(img, img.Bounds(), decoded, b.Min, draw.Over)
	for {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: fitJPEGQuality}); err != nil {
			return nil, "", fmt.Errorf("encode jpeg: %w", err)
		}
		if n := base64.StdEncoding.EncodedLen(buf.Len()); n <= max {
			desc := fmt.Sprintf("%dx%d %s → %dx%d JPEG, %d bytes of base64", b.Dx(), b.Dy(), format, img.Bounds().Dx(), img.Bounds().Dy(), n)
			return &api.ImageSource{Type: "base64", MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(buf.Bytes())}, desc, nil
		}
		w, h := img.Bounds().Dx()/2, img.Bounds().Dy()/2
		if w < fitMinSide || h < fitMinSide {
			return nil, "", fmt.Errorf("still over the bound at %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
		}
		img = halve(img, w, h)
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
