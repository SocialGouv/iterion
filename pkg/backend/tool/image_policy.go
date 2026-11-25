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
// (ImageResultContent) and the request builder's per-request bound — so no
// call site re-derives it.
type ImagePolicy struct {
	// Route is the model spec the policy was resolved for, named in the
	// errors a refused image produces.
	Route string

	// Acceptance says whether the route takes images at all. The zero value
	// is ImagesUnknown, under which images are sent.
	Acceptance ImageAcceptance

	// MaxImageBase64Bytes bounds one image, in bytes of base64. An image
	// over it is reduced to fit (ImageResultContent) or refused with an
	// error the model reads. 0 means no per-image bound.
	MaxImageBase64Bytes int

	// MaxRequestImageBytes bounds the base64 bytes of every image one
	// request carries. Over it, the oldest tool images are replaced by a
	// marker saying so (BoundRequestImages). 0 means no request bound.
	MaxRequestImageBytes int

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

// The escape hatches of the route image bounds: a non-negative byte count,
// 0 for no bound. They override every route's default, for the routes whose
// real limits differ from the family default (a gateway configured for
// larger bodies, a provider that raised its cap).
const (
	EnvImageMaxBase64Bytes     = "ITERION_IMAGE_MAX_BASE64_BYTES"
	EnvRequestImageMaxBytes    = "ITERION_REQUEST_IMAGE_MAX_BYTES"
	anthropicImageBase64Bytes  = 5 << 20
	anthropicRequestImageBytes = 24 << 20
	openAIImageBase64Bytes     = 20 << 20
	openAIRequestImageBytes    = 40 << 20
)

// anthropicImageProviders are the routes Anthropic's Messages API limits
// apply to: 5 MiB of base64 per image (a 400 the conversation cannot recover
// from past it) and 32 MB per request.
var anthropicImageProviders = map[string]bool{
	"anthropic": true,
	"bedrock":   true,
	"vertex":    true,
}

// RouteImagePolicy resolves the image policy of a model spec.
//
// The Anthropic family (anthropic, bedrock, vertex, and a bare claude-* id,
// which the claw registry routes to anthropic) bounds an image at 5 MiB of
// base64 and a request's images at 24 MiB — the 32 MB request cap, less room
// for a full context of text. Every other route — openai, xai, foundry, an
// OpenAI-compatible gateway — bounds an image at 20 MiB and a request's
// images at 40 MiB, under the 50 MB request OpenAI documents. getenv reads
// the escape hatches (EnvImageMaxBase64Bytes, EnvRequestImageMaxBytes); a
// value that is not a non-negative integer is an error, never ignored.
func RouteImagePolicy(spec string, getenv func(string) string) (ImagePolicy, error) {
	policy := ImagePolicy{
		Route:                spec,
		MaxImageBase64Bytes:  openAIImageBase64Bytes,
		MaxRequestImageBytes: openAIRequestImageBytes,
	}
	route := modelroute.Parse(spec)
	if anthropicImageProviders[route.Provider] || (route.Provider == "" && strings.HasPrefix(route.Wire, "claude")) {
		policy.MaxImageBase64Bytes = anthropicImageBase64Bytes
		policy.MaxRequestImageBytes = anthropicRequestImageBytes
	}
	for _, o := range []struct {
		name string
		dst  *int
	}{
		{EnvImageMaxBase64Bytes, &policy.MaxImageBase64Bytes},
		{EnvRequestImageMaxBytes, &policy.MaxRequestImageBytes},
	} {
		raw := strings.TrimSpace(getenv(o.name))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return ImagePolicy{}, fmt.Errorf("%s=%q: want a non-negative byte count (0 for no bound)", o.name, raw)
		}
		*o.dst = n
	}
	return policy, nil
}

// ImagesRefusedError is a tool image a route that takes no image would
// receive. The model reads it as a recoverable tool error.
type ImagesRefusedError struct {
	Tool  string
	Route string
}

func (e *ImagesRefusedError) Error() string {
	return fmt.Sprintf("%s returned an image, and the model route %q does not accept images", e.Tool, e.Route)
}

// ImageTooLargeError is a tool image over the route's per-image bound that
// could not be reduced to fit it. Its message tells the model what its
// producer can do about it.
type ImageTooLargeError struct {
	Tool  string
	Block int
	Bytes int
	Max   int
	// Cause says why the image could not be reduced.
	Cause error
}

func (e *ImageTooLargeError) Error() string {
	var advice string
	switch e.Tool {
	case "read_image":
		advice = "convert the file to a smaller PNG or JPEG (fewer pixels) and read that file"
	case "screenshot", "computer_use":
		advice = "the capture is too large for this route: lower the display resolution before capturing again"
	default:
		advice = "ask the tool for a smaller image"
	}
	return fmt.Sprintf("image result block %d is %d bytes of base64, over the %d this model route accepts per image, and could not be reduced (%v) — %s, or raise %s if the route accepts larger images",
		e.Block, e.Bytes, e.Max, e.Cause, advice, EnvImageMaxBase64Bytes)
}

func (e *ImageTooLargeError) Unwrap() error { return e.Cause }

// TextOnlyImageMarker stands for an image in a tool result whose receiver
// reads text only.
const TextOnlyImageMarker = "[image omitted: a program receives text only — call the tool directly for the model to see the image]"
