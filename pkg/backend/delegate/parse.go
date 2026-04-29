package delegate

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// parseSDKOutput converts SDK result fields into a delegate Result.Output map.
// It prioritizes structuredOutput over resultText, falling back to JSON extraction
// from markdown and finally plain text wrapping.
func parseSDKOutput(resultText *string, structuredOutput any, outputSchema json.RawMessage) (output map[string]interface{}, rawLen int, fallback bool) {
	// Priority 1: structured output from SDK.
	if structuredOutput != nil {
		if obj, ok := structuredOutput.(map[string]interface{}); ok {
			return obj, 0, false
		}
		// Round-trip via JSON for non-map types.
		b, err := json.Marshal(structuredOutput)
		if err == nil {
			var obj map[string]interface{}
			if json.Unmarshal(b, &obj) == nil {
				return obj, len(b), false
			}
		}
	}

	// Priority 2: parse result text.
	if resultText != nil && *resultText != "" {
		text := *resultText
		rawLen = len(text)

		// Try direct JSON object parse.
		var obj map[string]interface{}
		if json.Unmarshal([]byte(text), &obj) == nil {
			return obj, rawLen, false
		}

		// Try extracting JSON from markdown code blocks.
		if extracted := extractJSONFromMarkdown(text); extracted != "" {
			if json.Unmarshal([]byte(extracted), &obj) == nil {
				return obj, rawLen, false
			}
		}

		// Fallback: wrap raw text.
		output = map[string]interface{}{"text": text}
		fb := len(outputSchema) > 0
		return output, rawLen, fb
	}

	return map[string]interface{}{}, 0, false
}

// validateWorkDir checks that workDir resolves to a path within baseDir.
// If baseDir is empty, no validation is performed.
// Symlinks are resolved to prevent directory traversal bypasses.
func validateWorkDir(workDir, baseDir string) error {
	if baseDir == "" {
		return nil
	}

	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("delegate: invalid WorkDir %q: %w", workDir, err)
	}
	absWork, err = filepath.EvalSymlinks(absWork)
	if err != nil {
		return fmt.Errorf("delegate: resolve WorkDir %q: %w", workDir, err)
	}

	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return fmt.Errorf("delegate: invalid BaseDir %q: %w", baseDir, err)
	}
	absBase, err = filepath.EvalSymlinks(absBase)
	if err != nil {
		return fmt.Errorf("delegate: resolve BaseDir %q: %w", baseDir, err)
	}

	if absWork != absBase && !strings.HasPrefix(absWork, absBase+string(filepath.Separator)) {
		return fmt.Errorf("delegate: WorkDir %q is outside allowed BaseDir %q", workDir, baseDir)
	}

	return nil
}

// extractJSONFromMarkdown extracts the last JSON object from markdown code blocks.
// It looks for ```json ... ``` or ``` ... ``` blocks and returns the last one
// that contains valid JSON.
func extractJSONFromMarkdown(text string) string {
	const fence = "```"
	result := ""
	for {
		start := strings.Index(text, fence)
		if start == -1 {
			break
		}
		// Skip the opening fence and optional language tag.
		inner := text[start+len(fence):]
		// Skip language tag (e.g., "json").
		if nl := strings.IndexByte(inner, '\n'); nl != -1 {
			inner = inner[nl+1:]
		}
		end := strings.Index(inner, fence)
		if end == -1 {
			break
		}
		block := strings.TrimSpace(inner[:end])
		// Check if it looks like a JSON object.
		if len(block) > 0 && block[0] == '{' {
			result = block
		}
		text = inner[end+len(fence):]
	}
	return result
}
