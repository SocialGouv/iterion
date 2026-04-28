package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// ----- read_image -----

// ReadImageTool returns the tool definition for loading an image into the
// conversation as a vision content block.
func ReadImageTool() api.Tool { return intl.ReadImageTool() }

// ReadImageResult is the typed payload of ExecuteReadImage. Blocks contains
// a single vision ContentBlock that callers can splice into a tool_result
// content list bound for the next user-turn message.
type ReadImageResult = intl.ReadImageResult

// ExecuteReadImage loads an image from input["path"] (local file) or
// input["url"] (HTTPS only) and returns it as a base64 vision block.
// Maximum size is 5 MB.
func ExecuteReadImage(ctx context.Context, input map[string]any) (ReadImageResult, error) {
	return intl.ExecuteReadImage(ctx, input)
}

// ----- screenshot (stub) -----

// ScreenshotTool returns the tool definition for capturing a screenshot.
// The underlying executor is a stub: ExecuteScreenshot always returns a
// 501 *api.APIError until a platform backend lands.
func ScreenshotTool() api.Tool { return intl.ScreenshotTool() }

// ExecuteScreenshot is currently a stub returning *api.APIError{StatusCode:
// 501}. The vision pipeline is ready end-to-end; only the capture backend
// is missing.
func ExecuteScreenshot(ctx context.Context, input map[string]any) (ReadImageResult, error) {
	return intl.ExecuteScreenshot(ctx, input)
}
