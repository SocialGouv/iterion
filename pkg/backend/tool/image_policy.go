package tool

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
)

// ImagePolicy is what the receiver of a tool's images accepts: the model
// route a generation talks to, or a codemode program reading text only. It
// is resolved once per generation from the route (RouteImagePolicy) and
// carried to every place that hands images on — the tool-result shaping
// (ImageResultContent) and the request builder's per-request bounds — so no
// call site re-derives it. Every bound reads 0 as no bound.
type ImagePolicy struct {
	// Route is the model spec the policy was resolved for, named in the
	// errors a refused image produces.
	Route string

	// Acceptance says whether the route takes images at all. The zero value
	// is ImagesUnknown, under which images are sent.
	Acceptance ImageAcceptance

	// MaxImageBase64Bytes bounds one image, in bytes of base64.
	MaxImageBase64Bytes int

	// MaxImageSide bounds the longer side of one image, in pixels, read from
	// the image header (PNG, JPEG, GIF; a format Go cannot read the header
	// of is not measured).
	MaxImageSide int

	// MaxDecodePixels bounds the pixels a reduction decodes, read from the
	// header before any decode.
	MaxDecodePixels int64

	// MaxRequestImageBytes bounds the base64 bytes of every image one
	// request carries; MaxRequestImages their count. Over either, the oldest
	// tool images are replaced by a marker saying so (the request builder).
	MaxRequestImageBytes int
	MaxRequestImages     int

	// ManyImagesThreshold and ManyImagesMaxSide: a request carrying more
	// than ManyImagesThreshold images holds each to ManyImagesMaxSide pixels
	// a side (Anthropic refuses the request otherwise, on every retry).
	ManyImagesThreshold int
	ManyImagesMaxSide   int

	// TextOnly says the receiver reads text only — a codemode program. An
	// image becomes TextOnlyImageMarker: no bound applies, and the image
	// bytes are never kept.
	TextOnly bool
}

// ImageAcceptance is a route's verdict on images.
type ImageAcceptance int

const (
	// ImagesUnknown: nothing says the route refuses images; they are sent.
	ImagesUnknown ImageAcceptance = iota
	// ImagesAccepted: the route is known to accept images.
	ImagesAccepted
	// ImagesRefused: the route is known not to accept images. A tool image
	// becomes an ImagesRefusedError the model reads, never a request the
	// provider rejects.
	ImagesRefused
)

// The escape hatches of the route image bounds: a non-negative integer, 0
// for no bound. They override every route's default, for the routes whose
// real limits differ from the family default (a gateway configured for
// larger bodies, a provider that raised its cap).
const (
	EnvImageMaxBase64Bytes        = "ITERION_IMAGE_MAX_BASE64_BYTES"
	EnvImageMaxSide               = "ITERION_IMAGE_MAX_SIDE"
	EnvImageMaxDecodePixels       = "ITERION_IMAGE_MAX_DECODE_PIXELS"
	EnvRequestImageMaxBytes       = "ITERION_REQUEST_IMAGE_MAX_BYTES"
	EnvRequestImageMaxCount       = "ITERION_REQUEST_IMAGE_MAX_COUNT"
	EnvRequestManyImagesThreshold = "ITERION_REQUEST_MANY_IMAGES_THRESHOLD"
	EnvRequestManyImagesMaxSide   = "ITERION_REQUEST_MANY_IMAGES_MAX_SIDE"
	anthropicImageBase64          = 5 << 20
	anthropicImageSide            = 8000
	anthropicRequestImageB64      = 24 << 20
	anthropicRequestImages        = 100
	anthropicManyImages           = 20
	anthropicManyImagesSide       = 2000
	openAIImageBase64             = 20 << 20
	openAIRequestImageB64         = 40 << 20
	// defaultDecodePixels bounds a reduction's decode: a few-byte PNG can
	// declare 100000×100000, whose RGBA buffer alone is 40 GB. 50 megapixels
	// covers an 8K capture (7680×4320 ≈ 33 MP) with room, at 200 MB of RGBA.
	defaultDecodePixels = 50_000_000
)

// ImagePolicyEnvVars are the escape hatches the image policy reads
// (RouteImagePolicy's bounds, ImageAcceptanceFor's override), in one list
// for the reader and the sandbox crossing alike.
var ImagePolicyEnvVars = []string{
	EnvImageMaxBase64Bytes,
	EnvImageMaxSide,
	EnvImageMaxDecodePixels,
	EnvRequestImageMaxBytes,
	EnvRequestImageMaxCount,
	EnvRequestManyImagesThreshold,
	EnvRequestManyImagesMaxSide,
	EnvImageAcceptance,
}

// EnvImageAcceptance overrides what a route's catalog states about image
// input (ImageAcceptanceFor): "auto" (or empty) follows the catalog, "send"
// sends images whatever the catalog states — a gateway whose catalog row
// says text-only while the model it serves takes images — and "refuse"
// refuses them on every route. It applies to every route of the process.
const EnvImageAcceptance = "ITERION_IMAGE_ACCEPTANCE"

// ImageAcceptanceFor turns what a route's catalog states about image input
// — nil unstated, else whether the model takes images — into the policy's
// Acceptance, under the EnvImageAcceptance hatch getenv reads. Unstated is
// ImagesUnknown: images are sent, since a refusal on a guess would break a
// working vision route. A hatch value other than auto, send or refuse is an
// error, never ignored.
func ImageAcceptanceFor(stated *bool, getenv func(string) string) (ImageAcceptance, error) {
	switch raw := strings.ToLower(strings.TrimSpace(getenv(EnvImageAcceptance))); raw {
	case "", "auto":
	case "send":
		return ImagesAccepted, nil
	case "refuse":
		return ImagesRefused, nil
	default:
		return ImagesUnknown, fmt.Errorf("%s=%q: want auto, send or refuse", EnvImageAcceptance, raw)
	}
	switch {
	case stated == nil:
		return ImagesUnknown, nil
	case *stated:
		return ImagesAccepted, nil
	}
	return ImagesRefused, nil
}

// anthropicImageProviders are the routing prefixes Anthropic's Messages API
// serves: 5 MiB of base64 and 8000 pixels a side per image (a 400 the
// conversation cannot recover from past either), 100 images and 32 MB per
// request, and 2000 pixels a side for every image of a request of more than
// 20.
var anthropicImageProviders = map[string]bool{
	"anthropic": true,
	"bedrock":   true,
	"vertex":    true,
}

// RouteImagePolicy resolves the image policy of a model spec.
//
// The Anthropic family — an anthropic, bedrock or vertex route, or any
// route whose model id names a Claude model (a bare claude-*, a gateway
// serving one) — bounds an image at 5 MiB of base64 and 8000 pixels a side,
// a request's images at 24 MiB (the 32 MB request cap, less room for a full
// context of text) and 100 images, and every image of a request of more
// than 20 at 2000 pixels a side. Every other route — openai, xai,
// foundry, an OpenAI-compatible gateway — bounds an image at 20 MiB and a
// request's images at 40 MiB, under the 50 MB request OpenAI documents, with
// no side or count bound. A reduction decodes at most 50 megapixels.
//
// getenv reads the escape hatches; a value that is not a non-negative
// integer is an error, never ignored. A per-image bound over the per-request
// one is not: the knobs are global and each family has its own defaults, so
// the smaller of the two holds where an image is bounded
// (EffectiveMaxImageBytes).
func RouteImagePolicy(spec string, getenv func(string) string) (ImagePolicy, error) {
	policy := ImagePolicy{
		Route:                spec,
		MaxImageBase64Bytes:  openAIImageBase64,
		MaxDecodePixels:      defaultDecodePixels,
		MaxRequestImageBytes: openAIRequestImageB64,
	}
	route := modelroute.Parse(spec)
	if anthropicImageProviders[route.Provider] || strings.Contains(strings.ToLower(route.Wire), "claude") {
		policy.MaxImageBase64Bytes = anthropicImageBase64
		policy.MaxImageSide = anthropicImageSide
		policy.MaxRequestImageBytes = anthropicRequestImageB64
		policy.MaxRequestImages = anthropicRequestImages
		policy.ManyImagesThreshold = anthropicManyImages
		policy.ManyImagesMaxSide = anthropicManyImagesSide
	}
	var decodePixels = int(policy.MaxDecodePixels)
	for _, o := range []struct {
		name string
		dst  *int
	}{
		{EnvImageMaxBase64Bytes, &policy.MaxImageBase64Bytes},
		{EnvImageMaxSide, &policy.MaxImageSide},
		{EnvImageMaxDecodePixels, &decodePixels},
		{EnvRequestImageMaxBytes, &policy.MaxRequestImageBytes},
		{EnvRequestImageMaxCount, &policy.MaxRequestImages},
		{EnvRequestManyImagesThreshold, &policy.ManyImagesThreshold},
		{EnvRequestManyImagesMaxSide, &policy.ManyImagesMaxSide},
	} {
		raw := strings.TrimSpace(getenv(o.name))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return ImagePolicy{}, fmt.Errorf("%s=%q: want a non-negative integer (0 for no bound)", o.name, raw)
		}
		*o.dst = n
	}
	policy.MaxDecodePixels = int64(decodePixels)
	return policy, nil
}

// EffectiveMaxImageBytes is the byte bound one image is held to: the
// per-image bound, or the per-request one where it is smaller — an image
// over it could never be sent. 0 is no bound. knobs names the escape
// hatches of the bound that holds.
func (p ImagePolicy) EffectiveMaxImageBytes() (max int, knobs string) {
	per, req := p.MaxImageBase64Bytes, p.MaxRequestImageBytes
	switch {
	case req > 0 && (per <= 0 || req < per):
		return req, EnvRequestImageMaxBytes + " (the per-request budget, under " + EnvImageMaxBase64Bytes + ")"
	case per > 0:
		return per, EnvImageMaxBase64Bytes
	}
	return 0, ""
}

// ImagesRefusedError is a tool image a route that takes no image would
// receive. The model reads it as a recoverable tool error, never sent: it
// says calling again will not help and how to get the content another way.
type ImagesRefusedError struct {
	Tool  string
	Route string
	// Description is the tool's own text about the image (read_image's
	// caller-given description, else the file's base name or the URL), kept
	// so the model still reads what it asked for.
	Description string
}

func (e *ImagesRefusedError) Error() string {
	var advice string
	switch e.Tool {
	case "read_image":
		advice = "get what you need from the file another way — read it as text if it is a text format (SVG, a document), or use a tool that extracts or describes its content"
	case "screenshot", "computer_use":
		advice = "get what you need from the screen another way — read the page or the application's state as text"
	default:
		advice = "get what you need another way, as text"
	}
	described := ""
	if e.Description != "" {
		described = fmt.Sprintf(" The tool described the image: %s", e.Description)
	}
	return fmt.Sprintf("%s returned an image, and model route %q does not accept images: calling %s again will not help — %s (operators: %s=send overrides the route's catalog).%s",
		e.Tool, e.Route, e.Tool, advice, EnvImageAcceptance, described)
}

// ImageTooLargeError is a tool image over one of the route's per-image
// bounds. Its message tells the model what its producer can do about it.
type ImageTooLargeError struct {
	Tool  string
	Block int
	// Over says which bound the image is over ("is N bytes of base64, over
	// the M …"), and Env names that bound's escape hatch.
	Over string
	Env  string
	// Cause, when set, says why a reduction failed; nil when none was tried
	// (a producer whose bytes are never decoded).
	Cause error
}

func (e *ImageTooLargeError) Error() string {
	var advice string
	switch e.Tool {
	case "read_image":
		advice = "convert or downscale the file to a smaller PNG or JPEG (fewer pixels) and read that file"
	case "screenshot":
		advice = "the capture is too large for this route: lower the display resolution before capturing again"
	case "computer_use":
		advice = "the capture is too large for this route and computer_use does not reduce it (its clicks are in the capture's coordinates): call screenshot, whose capture is reduced to fit, to see the screen, or lower the display resolution"
	default:
		advice = "ask the tool for a smaller image"
	}
	reduced := ""
	if e.Cause != nil {
		reduced = fmt.Sprintf(", and could not be reduced (%v)", e.Cause)
	}
	return fmt.Sprintf("image result block %d %s%s — %s, or raise %s if the route accepts larger images", e.Block, e.Over, reduced, advice, e.Env)
}

func (e *ImageTooLargeError) Unwrap() error { return e.Cause }

// TextOnlyImageMarker stands for an image in a tool result whose receiver
// reads text only.
const TextOnlyImageMarker = "[image omitted: a program receives text only — call the tool directly for the model to see the image]"
