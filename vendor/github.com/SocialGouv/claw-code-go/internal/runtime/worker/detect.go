package worker

import (
	"os"
	"path/filepath"
	"strings"
)

// PromptDeliveryObservation is the result of detectPromptMisdelivery.
type PromptDeliveryObservation struct {
	Target      WorkerPromptTarget
	ObservedCwd *string
}

// ---------------------------------------------------------------------------
// Trust prompt detection
// ---------------------------------------------------------------------------

var trustPromptPatterns = []string{
	"do you trust the files in this folder",
	"trust the files in this folder",
	"trust this folder",
	"allow and continue",
	"yes, proceed",
}

func detectTrustPrompt(lowered string) bool {
	for _, p := range trustPromptPatterns {
		if strings.Contains(lowered, p) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Prompt misdelivery detection
// ---------------------------------------------------------------------------

var shellErrorPatterns = []string{
	"command not found",
	"syntax error near unexpected token",
	"no such file or directory",
	"parse error near",
	"unknown command",
	"permission denied",
	"is not recognized as",
	"not an internal or external command",
}

func detectPromptMisdelivery(screenText, lowered string, prompt *string, expectedCwd string) *PromptDeliveryObservation {
	if prompt == nil || *prompt == "" {
		return nil
	}

	// Extract first non-empty line of prompt, lowered.
	promptLowered := strings.ToLower(*prompt)
	firstLine := ""
	for _, line := range strings.Split(promptLowered, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			firstLine = trimmed
			break
		}
	}

	if firstLine == "" {
		return nil
	}

	// Check if prompt text is visible on screen.
	if !strings.Contains(lowered, firstLine) {
		return nil
	}

	// Detect wrong target via CWD mismatch.
	observedCwd := detectObservedShellCwd(screenText)
	if observedCwd != "" && expectedCwd != "" && !cwdMatchesObservedTarget(expectedCwd, observedCwd) {
		cwd := observedCwd
		return &PromptDeliveryObservation{
			Target:      TargetWrongTarget,
			ObservedCwd: &cwd,
		}
	}

	// Detect shell error patterns.
	for _, pattern := range shellErrorPatterns {
		if strings.Contains(lowered, pattern) {
			if observedCwd != "" {
				cwd := observedCwd
				return &PromptDeliveryObservation{
					Target:      TargetShell,
					ObservedCwd: &cwd,
				}
			}
			return &PromptDeliveryObservation{
				Target: TargetShell,
			}
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Ready-for-prompt detection
// ---------------------------------------------------------------------------

var readyMarkers = []string{
	"ready for input",
	"ready for your input",
	"ready for prompt",
	"send a message",
}

var agentPromptChars = []string{">", "\u203a", "\u276f"} // >, ›, ❯

// boxDrawingPromptPatterns matches box-drawing prompt patterns like "│ >" used by some CLI agents.
var boxDrawingPromptPatterns = []string{
	"\u2502 >",      // │ >
	"\u2502 \u203a", // │ ›
	"\u2502 \u276f", // │ ❯
}

func detectReadyForPrompt(screenText, lowered string) bool {
	// Check explicit markers.
	for _, marker := range readyMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}

	// Check last non-empty line for agent prompt characters.
	lines := strings.Split(screenText, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		// Reject shell prompts.
		if isShellPrompt(trimmed) {
			return false
		}
		// Check for box-drawing prompt patterns.
		for _, pat := range boxDrawingPromptPatterns {
			if strings.Contains(trimmed, pat) {
				return true
			}
		}
		// Check for agent prompt chars (suffix, prefix, or starts-with pattern like "> ").
		for _, ch := range agentPromptChars {
			if strings.HasSuffix(trimmed, ch) || strings.HasPrefix(trimmed, ch+" ") || strings.HasPrefix(trimmed, ch) {
				return true
			}
		}
		break
	}

	return false
}

// ---------------------------------------------------------------------------
// Running cue detection
// ---------------------------------------------------------------------------

var runningCues = []string{
	"thinking",
	"working",
	"running tests",
	"inspecting",
	"analyzing",
}

func detectRunningCue(lowered string) bool {
	for _, cue := range runningCues {
		if strings.Contains(lowered, cue) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Shell CWD detection
// ---------------------------------------------------------------------------

var shellPromptTokens = []string{"$", "%", "#", ">", "\u203a", "\u276f"}

func detectObservedShellCwd(screenText string) string {
	lines := strings.Split(screenText, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		for _, token := range shellPromptTokens {
			idx := strings.Index(trimmed, token)
			if idx <= 0 {
				continue
			}
			// Extract the token preceding the shell prompt character.
			before := strings.TrimSpace(trimmed[:idx])
			parts := strings.Fields(before)
			if len(parts) == 0 {
				continue
			}
			candidate := parts[len(parts)-1]
			if looksLikeCwdLabel(candidate) {
				return candidate
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Path normalization
// ---------------------------------------------------------------------------

// NormalizePath normalizes a path for comparison: expand ~ to $HOME, clean, remove trailing slash.
func NormalizePath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			p = filepath.Join(home, p[2:])
		}
	} else if p == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			p = home
		}
	}
	p = filepath.Clean(p)
	// Remove trailing separator unless root.
	if len(p) > 1 && strings.HasSuffix(p, string(filepath.Separator)) {
		p = p[:len(p)-1]
	}
	return p
}

// ---------------------------------------------------------------------------
// Path matching
// ---------------------------------------------------------------------------

func pathMatchesAllowlist(cwd, trustedRoot string) bool {
	cwd = filepath.Clean(cwd)
	trustedRoot = filepath.Clean(trustedRoot)
	if cwd == trustedRoot {
		return true
	}
	return strings.HasPrefix(cwd, trustedRoot+string(filepath.Separator))
}

// ---------------------------------------------------------------------------
// Utility functions
// ---------------------------------------------------------------------------

func promptPreview(prompt string) string {
	trimmed := strings.TrimSpace(prompt)
	runes := []rune(trimmed)
	if len(runes) <= 48 {
		return trimmed
	}
	preview := strings.TrimRight(string(runes[:48]), " \t\n\r")
	return preview + "\u2026"
}

func promptMisdeliveryDetail(target WorkerPromptTarget) string {
	switch target {
	case TargetShell:
		return "shell misdelivery detected"
	case TargetWrongTarget:
		return "prompt landed in wrong target"
	default:
		return "prompt delivery failure detected"
	}
}

func looksLikeCwdLabel(candidate string) bool {
	if strings.HasPrefix(candidate, "/") {
		return true
	}
	if strings.HasPrefix(candidate, "~") {
		return true
	}
	if strings.HasPrefix(candidate, ".") {
		return true
	}
	if strings.Contains(candidate, "/") {
		return true
	}
	return false
}

func isShellPrompt(trimmed string) bool {
	if strings.HasSuffix(trimmed, "$") || strings.HasSuffix(trimmed, "%") || strings.HasSuffix(trimmed, "#") {
		return true
	}
	if strings.HasPrefix(trimmed, "$") || strings.HasPrefix(trimmed, "%") || strings.HasPrefix(trimmed, "#") {
		return true
	}
	return false
}

func cwdMatchesObservedTarget(expectedCwd, observedCwd string) bool {
	expected := NormalizePath(expectedCwd)
	observed := NormalizePath(observedCwd)

	if expected == observed {
		return true
	}
	// ends_with check: expected ends with observed or vice versa.
	if strings.HasSuffix(expected, observed) || strings.HasSuffix(observed, expected) {
		return true
	}
	// starts_with check.
	if strings.HasPrefix(expected, observed) || strings.HasPrefix(observed, expected) {
		return true
	}
	// Basename match.
	if filepath.Base(expected) == filepath.Base(observed) {
		return true
	}
	return false
}
