package tool

import (
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	clawtools "github.com/SocialGouv/iterion/pkg/claw/pkg/api/tools"
)

// ImageResultContent decodes the output of a tool registered with an image
// result (read_image, screenshot, computer_use: clawImageResultAdapter's
// JSON) into the content of its tool_result — the description as a text
// block, then the image blocks, so a vision model reads the image itself
// rather than its base64 as text.
//
// A computer_use action other than a capture returns its description alone.
// The output is the tool's own encoding, so a decode failure, a block that
// is neither text nor image, or an empty result is a defect of the
// producer, reported as an error rather than passed on as text.
func ImageResultContent(output string) ([]api.ContentBlock, error) {
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
			content = append(content, api.ContentBlock{Type: "image", Source: b.Source})
		default:
			return nil, fmt.Errorf("image result block %d has type %q, want text or image", i, b.Type)
		}
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("image result carries neither a description nor a block")
	}
	return content, nil
}
