package tool

import (
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	clawtools "github.com/SocialGouv/iterion/pkg/claw/pkg/api/tools"
)

// reducibleImageProducers are the tools whose image the tool loop may decode
// to reduce it: a PNG the tool itself encodes from the display. Every other
// producer's bytes — read_image's are any workspace file — are never
// decoded: a crafted JPEG costs minutes of uncancellable CPU or gigabytes to
// decode under any pixel bound.
var reducibleImageProducers = map[string]bool{"screenshot": true, "computer_use": true}

// ImageResultContent decodes the output of a tool registered with an image
// result (read_image, screenshot, computer_use: clawImageResultAdapter's
// JSON) into the content of its tool_result — the description as a text
// block, then the image blocks, so a vision model reads the image itself
// rather than its base64 as text.
//
// policy is what the receiver accepts. A text-only receiver gets
// TextOnlyImageMarker in place of each image, unbounded, the image bytes
// dropped. Otherwise a route that refuses images makes any image an
// *ImagesRefusedError, and an image over one of the route's per-image
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
			var over, env string
			if max := policy.MaxImageBase64Bytes; max > 0 && len(src.Data) > max {
				over, env = fmt.Sprintf("is %d bytes of base64, over the %d this model route accepts per image", len(src.Data), max), EnvImageMaxBase64Bytes
			} else if side := policy.MaxImageSide; side > 0 {
				if w, h, _, ok := ImageHeader(src); ok && (w > side || h > side) {
					over, env = fmt.Sprintf("is %dx%d pixels, over the %d-pixel side this model route accepts", w, h, side), EnvImageMaxSide
				}
			}
			if over != "" {
				if !reducibleImageProducers[toolName] {
					return nil, &ImageTooLargeError{Tool: toolName, Block: i, Over: over, Env: env}
				}
				fitted, desc, err := fitImageBase64(src, policy.MaxImageBase64Bytes, policy.MaxImageSide, policy.MaxDecodePixels)
				if err != nil {
					return nil, &ImageTooLargeError{Tool: toolName, Block: i, Over: over, Env: env, Cause: err}
				}
				src = fitted
				note = fmt.Sprintf("[image %d was reduced to fit this model route's per-image limits: %s]", i, desc)
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
