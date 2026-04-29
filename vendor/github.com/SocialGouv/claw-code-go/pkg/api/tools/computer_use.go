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

// ----- screenshot -----

// ScreenshotTool returns the tool definition for capturing a screenshot.
// Backed by ImageMagick `import` on Linux/X11; returns
// ErrComputerUseUnavailable when prerequisites are missing.
func ScreenshotTool() api.Tool { return intl.ScreenshotTool() }

// ExecuteScreenshot captures the host display and returns it as a
// base64-encoded image content block. Equivalent to ExecuteComputerUse with
// action="screenshot".
func ExecuteScreenshot(ctx context.Context, input map[string]any) (ReadImageResult, error) {
	return intl.ExecuteScreenshot(ctx, input)
}

// ----- computer_use (full action surface) -----

// ComputerUseResult is the typed payload of ExecuteComputerUse. For
// screenshot actions, Blocks holds a single image ContentBlock and
// Description is "screenshot". For input actions (click / type / key /
// mouse_move / cursor_position / left_click_drag), Blocks is empty and
// Description holds a short success summary.
type ComputerUseResult = intl.ComputerUseResult

// ErrComputerUseUnavailable is returned (wrapped) when the host environment
// cannot support computer-use actions: no display server, missing required
// binaries (xdotool, ImageMagick `import`), or unsupported OS. Use
// errors.Is to detect.
var ErrComputerUseUnavailable = intl.ErrComputerUseUnavailable

// ComputerUseTool returns the tool definition for the computer_use tool.
// Action verbs mirror Anthropic's computer_use_20241022 spec: screenshot,
// left_click, right_click, middle_click, double_click, type, key,
// mouse_move, cursor_position, left_click_drag.
func ComputerUseTool() api.Tool { return intl.ComputerUseTool() }

// ExecuteComputerUse dispatches the action verb in input["action"] to the
// matching backend handler. See ComputerUseTool for the schema.
func ExecuteComputerUse(ctx context.Context, input map[string]any) (ComputerUseResult, error) {
	return intl.ExecuteComputerUse(ctx, input)
}
