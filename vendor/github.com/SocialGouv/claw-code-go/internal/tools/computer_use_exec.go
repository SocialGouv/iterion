package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

// ErrComputerUseUnavailable is returned when the host environment cannot
// support computer-use actions: no display server, missing required binaries,
// or unsupported OS. Callers can wrap or unwrap with errors.As.
var ErrComputerUseUnavailable = errors.New("computer_use: not available on this host")

// computerUseRunner is the seam unit tests use to avoid invoking real
// xdotool / ImageMagick during the test run. The default implementation
// shells out via os/exec; tests swap in a stub that records commands.
type computerUseRunner interface {
	// lookPath checks whether a binary exists on PATH. Returning ""
	// signals it's missing (treated as unavailable).
	lookPath(name string) string
	// run executes name with args, returns stdout bytes. Stderr is
	// folded into the returned error message on failure.
	run(ctx context.Context, name string, args ...string) ([]byte, error)
	// hasDisplay reports whether a graphical session is reachable
	// (X11 DISPLAY or Wayland WAYLAND_DISPLAY env var present).
	hasDisplay() bool
}

// defaultRunner is the production runner. Replaced via runnerOverride in tests.
type defaultRunner struct{}

func (defaultRunner) lookPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

func (defaultRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return out, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func (defaultRunner) hasDisplay() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// runnerOverride lets tests inject a fake runner. nil means use defaultRunner.
var runnerOverride computerUseRunner

func currentRunner() computerUseRunner {
	if runnerOverride != nil {
		return runnerOverride
	}
	return defaultRunner{}
}

// ComputerUseAction enumerates the action verbs accepted by the computer_use
// tool. The set mirrors Anthropic's official computer_use_20241022 spec, minus
// scroll variants which depend on toolkit-specific keysyms.
type ComputerUseAction string

const (
	ActionScreenshot      ComputerUseAction = "screenshot"
	ActionLeftClick       ComputerUseAction = "left_click"
	ActionRightClick      ComputerUseAction = "right_click"
	ActionMiddleClick     ComputerUseAction = "middle_click"
	ActionDoubleClick     ComputerUseAction = "double_click"
	ActionType            ComputerUseAction = "type"
	ActionKey             ComputerUseAction = "key"
	ActionMouseMove       ComputerUseAction = "mouse_move"
	ActionCursorPosition  ComputerUseAction = "cursor_position"
	ActionLeftClickDrag   ComputerUseAction = "left_click_drag"
)

// ComputerUseTool returns the tool definition for the computer_use tool. The
// schema mirrors the Anthropic computer-use-20241022 contract closely enough
// that prompt-side guidance written for Claude Code transfers verbatim, while
// remaining a generic api.Tool (no provider-specific cache_control plumbing).
func ComputerUseTool() api.Tool {
	return api.Tool{
		Name: "computer_use",
		Description: "Interact with the host display: capture screenshots, move " +
			"the mouse, click, type, and press keys. Returns image content blocks " +
			"for screenshot actions and structured text for input actions. Requires " +
			"an X11 display with xdotool and ImageMagick `import` installed; returns " +
			"a clear error otherwise.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"action": {
					Type: "string",
					Description: "One of: screenshot, left_click, right_click, middle_click, " +
						"double_click, type, key, mouse_move, cursor_position, left_click_drag.",
				},
				"coordinate": {
					Type: "array",
					Description: "Two-element [x, y] integer pixel coordinate. Required for " +
						"clicks, mouse_move, and as the end-point for left_click_drag.",
				},
				"text": {
					Type: "string",
					Description: "For action=type: literal text to type. For action=key: an " +
						"xdotool keysym (e.g. \"Return\", \"ctrl+c\", \"shift+Tab\").",
				},
				"start_coordinate": {
					Type:        "array",
					Description: "Two-element [x, y] start point for left_click_drag.",
				},
			},
		},
	}
}

// ScreenshotTool is preserved for backward compatibility. It returns the
// computer_use tool definition rebranded with a screenshot-only description so
// existing callers that only ever want screenshots keep working.
func ScreenshotTool() api.Tool {
	return api.Tool{
		Name: "screenshot",
		Description: "Capture a screenshot of the host display and return it as " +
			"a base64-encoded image content block. Requires ImageMagick `import` on " +
			"PATH and an active display.",
		InputSchema: api.InputSchema{
			Type:       "object",
			Properties: map[string]api.Property{},
		},
	}
}

// ExecuteScreenshot is a thin shim around the computer_use screenshot action,
// kept to preserve the legacy API. New code should call ExecuteComputerUse
// with action="screenshot".
func ExecuteScreenshot(ctx context.Context, input map[string]any) (ReadImageResult, error) {
	return executeScreenshotAction(ctx, currentRunner())
}

// ComputerUseResult is what ExecuteComputerUse produces. For screenshot
// actions, Blocks holds a single image ContentBlock and Description carries a
// caption. For input actions, Blocks is empty and Description carries a short
// success summary (e.g. "clicked at (120, 240)").
type ComputerUseResult = ReadImageResult

// ExecuteComputerUse dispatches an action verb to its handler. Validates
// inputs, then either captures a screenshot or invokes xdotool. Always returns
// ErrComputerUseUnavailable (wrapped) when prerequisites are missing.
func ExecuteComputerUse(ctx context.Context, input map[string]any) (ComputerUseResult, error) {
	rawAction, _ := input["action"].(string)
	action := ComputerUseAction(strings.TrimSpace(rawAction))
	if action == "" {
		return ComputerUseResult{}, fmt.Errorf("computer_use: 'action' is required")
	}

	r := currentRunner()

	switch action {
	case ActionScreenshot:
		return executeScreenshotAction(ctx, r)
	case ActionLeftClick, ActionRightClick, ActionMiddleClick, ActionDoubleClick:
		return executeClickAction(ctx, r, action, input)
	case ActionType:
		return executeTypeAction(ctx, r, input)
	case ActionKey:
		return executeKeyAction(ctx, r, input)
	case ActionMouseMove:
		return executeMouseMoveAction(ctx, r, input)
	case ActionCursorPosition:
		return executeCursorPositionAction(ctx, r)
	case ActionLeftClickDrag:
		return executeDragAction(ctx, r, input)
	default:
		return ComputerUseResult{}, fmt.Errorf("computer_use: unknown action %q", action)
	}
}

// requireDisplay asserts that a graphical session is reachable. Returns a
// wrapped ErrComputerUseUnavailable so callers can errors.Is / errors.As.
func requireDisplay(r computerUseRunner) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("%w: only linux is supported (got %s)", ErrComputerUseUnavailable, runtime.GOOS)
	}
	if !r.hasDisplay() {
		return fmt.Errorf("%w: no DISPLAY or WAYLAND_DISPLAY env var set", ErrComputerUseUnavailable)
	}
	return nil
}

// requireBinary asserts that the named binary exists on PATH. Returns wrapped
// ErrComputerUseUnavailable so the caller can route the error through the
// usual unavailability handling without string matching.
func requireBinary(r computerUseRunner, name string) error {
	if r.lookPath(name) == "" {
		return fmt.Errorf("%w: %s not found on PATH", ErrComputerUseUnavailable, name)
	}
	return nil
}

// executeScreenshotAction captures the root window via ImageMagick `import`,
// reads the resulting PNG, and emits it as a base64 image ContentBlock.
func executeScreenshotAction(ctx context.Context, r computerUseRunner) (ComputerUseResult, error) {
	if err := requireDisplay(r); err != nil {
		return ComputerUseResult{}, err
	}
	if err := requireBinary(r, "import"); err != nil {
		return ComputerUseResult{}, err
	}

	// `import -window root png:-` writes PNG bytes to stdout. We use the
	// "png:" coder prefix so ImageMagick infers the format even though the
	// destination is a pipe with no extension to sniff.
	out, err := r.run(ctx, "import", "-window", "root", "png:-")
	if err != nil {
		return ComputerUseResult{}, fmt.Errorf("computer_use: screenshot capture failed: %w", err)
	}
	if len(out) == 0 {
		return ComputerUseResult{}, fmt.Errorf("computer_use: screenshot capture returned no data")
	}

	encoded := base64.StdEncoding.EncodeToString(out)
	block := api.ContentBlock{
		Type: "image",
		Source: &api.ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      encoded,
		},
	}
	return ComputerUseResult{
		Description: "screenshot",
		Blocks:      []api.ContentBlock{block},
	}, nil
}

func executeClickAction(ctx context.Context, r computerUseRunner, action ComputerUseAction, input map[string]any) (ComputerUseResult, error) {
	if err := requireDisplay(r); err != nil {
		return ComputerUseResult{}, err
	}
	if err := requireBinary(r, "xdotool"); err != nil {
		return ComputerUseResult{}, err
	}

	x, y, err := parseCoordinate(input, "coordinate")
	if err != nil {
		return ComputerUseResult{}, err
	}

	button := "1"
	repeat := "1"
	switch action {
	case ActionLeftClick:
		button = "1"
	case ActionMiddleClick:
		button = "2"
	case ActionRightClick:
		button = "3"
	case ActionDoubleClick:
		button = "1"
		repeat = "2"
	}

	args := []string{"mousemove", "--sync", strconv.Itoa(x), strconv.Itoa(y), "click", "--repeat", repeat, button}
	if _, err := r.run(ctx, "xdotool", args...); err != nil {
		return ComputerUseResult{}, fmt.Errorf("computer_use: %s failed: %w", action, err)
	}
	return ComputerUseResult{
		Description: fmt.Sprintf("%s at (%d, %d)", action, x, y),
	}, nil
}

func executeTypeAction(ctx context.Context, r computerUseRunner, input map[string]any) (ComputerUseResult, error) {
	if err := requireDisplay(r); err != nil {
		return ComputerUseResult{}, err
	}
	if err := requireBinary(r, "xdotool"); err != nil {
		return ComputerUseResult{}, err
	}

	text, _ := input["text"].(string)
	if text == "" {
		return ComputerUseResult{}, fmt.Errorf("computer_use: 'text' is required for action=type")
	}

	// xdotool type -- "literal" passes the text as a single argv entry. The
	// "--" terminator prevents any leading dash in user input from being
	// parsed as a flag, which is the only injection vector for an exec.Cmd
	// argv that's already split at the Go layer (no shell involved).
	if _, err := r.run(ctx, "xdotool", "type", "--delay", "0", "--", text); err != nil {
		return ComputerUseResult{}, fmt.Errorf("computer_use: type failed: %w", err)
	}
	return ComputerUseResult{
		Description: fmt.Sprintf("typed %d character(s)", len([]rune(text))),
	}, nil
}

func executeKeyAction(ctx context.Context, r computerUseRunner, input map[string]any) (ComputerUseResult, error) {
	if err := requireDisplay(r); err != nil {
		return ComputerUseResult{}, err
	}
	if err := requireBinary(r, "xdotool"); err != nil {
		return ComputerUseResult{}, err
	}

	key, _ := input["text"].(string)
	key = strings.TrimSpace(key)
	if key == "" {
		return ComputerUseResult{}, fmt.Errorf("computer_use: 'text' is required for action=key")
	}
	// Reject obvious shell metacharacters even though we never go through a
	// shell: a stray ; or | in a "key" value is almost certainly a model
	// hallucination (a real xdotool keysym is alphanumeric + underscore + plus).
	if strings.ContainsAny(key, ";|&`$<>\n\r") {
		return ComputerUseResult{}, fmt.Errorf("computer_use: 'text' contains invalid characters for a key spec")
	}

	if _, err := r.run(ctx, "xdotool", "key", "--", key); err != nil {
		return ComputerUseResult{}, fmt.Errorf("computer_use: key failed: %w", err)
	}
	return ComputerUseResult{
		Description: fmt.Sprintf("pressed key %q", key),
	}, nil
}

func executeMouseMoveAction(ctx context.Context, r computerUseRunner, input map[string]any) (ComputerUseResult, error) {
	if err := requireDisplay(r); err != nil {
		return ComputerUseResult{}, err
	}
	if err := requireBinary(r, "xdotool"); err != nil {
		return ComputerUseResult{}, err
	}
	x, y, err := parseCoordinate(input, "coordinate")
	if err != nil {
		return ComputerUseResult{}, err
	}
	if _, err := r.run(ctx, "xdotool", "mousemove", "--sync", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
		return ComputerUseResult{}, fmt.Errorf("computer_use: mouse_move failed: %w", err)
	}
	return ComputerUseResult{Description: fmt.Sprintf("moved cursor to (%d, %d)", x, y)}, nil
}

func executeCursorPositionAction(ctx context.Context, r computerUseRunner) (ComputerUseResult, error) {
	if err := requireDisplay(r); err != nil {
		return ComputerUseResult{}, err
	}
	if err := requireBinary(r, "xdotool"); err != nil {
		return ComputerUseResult{}, err
	}
	out, err := r.run(ctx, "xdotool", "getmouselocation", "--shell")
	if err != nil {
		return ComputerUseResult{}, fmt.Errorf("computer_use: cursor_position failed: %w", err)
	}
	x, y, err := parseGetMouseLocation(string(out))
	if err != nil {
		return ComputerUseResult{}, fmt.Errorf("computer_use: parse cursor position: %w", err)
	}
	return ComputerUseResult{Description: fmt.Sprintf("cursor at (%d, %d)", x, y)}, nil
}

func executeDragAction(ctx context.Context, r computerUseRunner, input map[string]any) (ComputerUseResult, error) {
	if err := requireDisplay(r); err != nil {
		return ComputerUseResult{}, err
	}
	if err := requireBinary(r, "xdotool"); err != nil {
		return ComputerUseResult{}, err
	}
	sx, sy, err := parseCoordinate(input, "start_coordinate")
	if err != nil {
		return ComputerUseResult{}, err
	}
	ex, ey, err := parseCoordinate(input, "coordinate")
	if err != nil {
		return ComputerUseResult{}, err
	}
	args := []string{
		"mousemove", "--sync", strconv.Itoa(sx), strconv.Itoa(sy),
		"mousedown", "1",
		"mousemove", "--sync", strconv.Itoa(ex), strconv.Itoa(ey),
		"mouseup", "1",
	}
	if _, err := r.run(ctx, "xdotool", args...); err != nil {
		return ComputerUseResult{}, fmt.Errorf("computer_use: left_click_drag failed: %w", err)
	}
	return ComputerUseResult{Description: fmt.Sprintf("dragged from (%d, %d) to (%d, %d)", sx, sy, ex, ey)}, nil
}

// parseCoordinate extracts an [x, y] integer pair from input[key]. It accepts
// JSON-decoded shapes: []any{float64, float64}, []any{int, int}, or []int{...}.
func parseCoordinate(input map[string]any, key string) (int, int, error) {
	raw, ok := input[key]
	if !ok || raw == nil {
		return 0, 0, fmt.Errorf("computer_use: %q is required", key)
	}
	switch v := raw.(type) {
	case []any:
		if len(v) != 2 {
			return 0, 0, fmt.Errorf("computer_use: %q must have exactly 2 elements", key)
		}
		x, err := toInt(v[0])
		if err != nil {
			return 0, 0, fmt.Errorf("computer_use: %q[0]: %w", key, err)
		}
		y, err := toInt(v[1])
		if err != nil {
			return 0, 0, fmt.Errorf("computer_use: %q[1]: %w", key, err)
		}
		return x, y, nil
	case []int:
		if len(v) != 2 {
			return 0, 0, fmt.Errorf("computer_use: %q must have exactly 2 elements", key)
		}
		return v[0], v[1], nil
	case []float64:
		if len(v) != 2 {
			return 0, 0, fmt.Errorf("computer_use: %q must have exactly 2 elements", key)
		}
		return int(v[0]), int(v[1]), nil
	default:
		return 0, 0, fmt.Errorf("computer_use: %q must be a [x, y] array", key)
	}
}

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, fmt.Errorf("not an integer: %q", n)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("not an integer: %T", v)
	}
}

// parseGetMouseLocation parses the shell-style output of
// `xdotool getmouselocation --shell` which looks like:
//
//	X=512
//	Y=384
//	SCREEN=0
//	WINDOW=12345678
func parseGetMouseLocation(out string) (int, int, error) {
	var x, y *int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "X":
			i, err := strconv.Atoi(v)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid X=%q", v)
			}
			x = &i
		case "Y":
			i, err := strconv.Atoi(v)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid Y=%q", v)
			}
			y = &i
		}
	}
	if x == nil || y == nil {
		return 0, 0, fmt.Errorf("missing X or Y in output")
	}
	return *x, *y, nil
}
