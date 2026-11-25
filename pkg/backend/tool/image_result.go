package tool

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	clawtools "github.com/SocialGouv/iterion/pkg/claw/pkg/api/tools"
)

// reducibleImageProducers are the tools whose image the tool loop may decode
// to reduce it: screenshot, a PNG the tool itself encodes from the display.
// Every other producer's bytes — read_image's are any workspace file — are
// never decoded: a crafted JPEG costs minutes of uncancellable CPU or
// gigabytes to decode under any pixel bound. computer_use is not reduced
// either: the model clicks in the coordinate space of the capture it sees,
// and the tool executes its coordinates on the display unscaled.
var reducibleImageProducers = map[string]bool{"screenshot": true}

// reductionNote is the text block that follows a reduced image; the request
// builder drops it with the image (IsReductionNote).
const reductionNote = "[image %d was reduced to fit this model route's per-image limits: %s]"

// IsReductionNote reports whether text is the note ImageResultContent puts
// after a reduced image.
func IsReductionNote(text string) bool {
	return strings.HasPrefix(text, "[image ") && strings.Contains(text, " was reduced to fit this model route's per-image limits: ")
}

// ImageResultContent decodes the output of a tool registered with an image
// result (read_image, screenshot, computer_use: clawImageResultAdapter's
// JSON) into the content of its tool_result — the description as a text
// block, then the image blocks, so a vision model reads the image itself
// rather than its base64 as text.
//
// policy is what the receiver accepts. A text-only receiver gets
// TextOnlyImageMarker in place of each image, unbounded, the image bytes
// dropped. Otherwise a route that refuses images makes any image an
// *ImagesRefusedError. An image's media type is the one its header declares
// (ImageHeader), the producer's kept where the header is unreadable. An
// image over one of the route's per-image
// bounds — its base64 size, or its longer side read from the header alone
// (ImageHeader) — is reduced to fit when toolName is a producer the tool
// loop controls (reducibleImageProducers), a text block saying so following
// it; any other producer's image, or one a reduction cannot fit, is an
// *ImageTooLargeError advising toolName's producer.
//
// A computer_use action other than a capture returns its description alone.
// The output is the tool's own encoding, so a decode failure, a block that
// is neither text nor image, or an empty result is a defect of the
// producer, reported as an error rather than passed on as text.
func ImageResultContent(toolName, output string, policy ImagePolicy) ([]api.ContentBlock, error) {
	var res clawtools.ReadImageResult
	if err := json.Unmarshal([]byte(output), &res); err != nil {
		return nil, fmt.Errorf("decode image result: %w", err)
	}
	content := make([]api.ContentBlock, 0, len(res.Blocks)+1)
	if res.Description != "" {
		content = append(content, api.ContentBlock{Type: "text", Text: res.Description})
	}
	for i, b := range res.Blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				content = append(content, api.ContentBlock{Type: "text", Text: b.Text})
			}
		case "image":
			if b.Source == nil {
				return nil, fmt.Errorf("image result block %d has no source", i)
			}
			if policy.TextOnly {
				content = append(content, api.ContentBlock{Type: "text", Text: TextOnlyImageMarker})
				continue
			}
			if policy.Acceptance == ImagesRefused {
				return nil, &ImagesRefusedError{Tool: toolName, Route: policy.Route}
			}
			src, note := b.Source, ""
			w, h, format, headerOK := ImageHeader(src)
			if mt := "image/" + format; format != "" && src.MediaType != mt {
				fixed := *src
				fixed.MediaType = mt
				src = &fixed
			}
			var over, env string
			if max, knobs := policy.EffectiveMaxImageBytes(); max > 0 && len(src.Data) > max {
				over, env = fmt.Sprintf("is %d bytes of base64, over the %d this model route accepts per image", len(src.Data), max), knobs
			} else if side := policy.MaxImageSide; side > 0 && headerOK && (w > side || h > side) {
				over, env = fmt.Sprintf("is %dx%d pixels, over the %d-pixel side this model route accepts", w, h, side), EnvImageMaxSide
			} else if side > 0 && !headerOK && format != "" {
				over, env = fmt.Sprintf("is a %s whose size its header does not give within %d bytes, so it cannot be held to the %d-pixel side this model route accepts", format, headerPrefixBytes, side), EnvImageMaxSide
			}
			if over != "" {
				if !reducibleImageProducers[toolName] {
					return nil, &ImageTooLargeError{Tool: toolName, Block: i, Over: over, Env: env}
				}
				max, _ := policy.EffectiveMaxImageBytes()
				fitted, desc, err := fitImageBase64(src, max, policy.MaxImageSide, policy.MaxDecodePixels)
				if err != nil {
					return nil, &ImageTooLargeError{Tool: toolName, Block: i, Over: over, Env: env, Cause: err}
				}
				src = fitted
				note = fmt.Sprintf(reductionNote, i, desc)
			}
			content = append(content, api.ContentBlock{Type: "image", Source: src})
			if note != "" {
				content = append(content, api.ContentBlock{Type: "text", Text: note})
			}
		default:
			return nil, fmt.Errorf("image result block %d has type %q, want text or image", i, b.Type)
		}
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("image result carries neither a description nor a block")
	}
	return content, nil
}
