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
func parseSDKOutput(resultText *string, structuredOutput any, outputSchema json.RawMessage) (output map[string]any, rawLen int, fallback bool) {
	// Priority 1: structured output from SDK — only when non-empty.
	// claude-code's stream-json emits `structured_output: {}` for
	// tool-using sessions where no second-pass formatter ran (i.e.
	// the sandboxed two-pass case where formatOutput can't resume the
	// in-container session). Treating an empty map as a valid result
	// wedged the runtime into "raw_output_len=0, parse_fallback=false"
	// with all required fields missing — fall through to the
	// resultText path so the assistant's final markdown JSON block
	// can be extracted instead.
	if structuredOutput != nil {
		if obj, ok := structuredOutput.(map[string]any); ok {
			if len(obj) > 0 {
				return obj, 0, false
			}
		} else {
			// Non-map types: round-trip via JSON.
			b, err := json.Marshal(structuredOutput)
			if err == nil {
				var obj map[string]any
				if json.Unmarshal(b, &obj) == nil && len(obj) > 0 {
					return obj, len(b), false
				}
			}
		}
	}

	// Priority 2: parse result text.
	if resultText != nil && *resultText != "" {
		text := *resultText
		rawLen = len(text)

		// Try direct JSON object parse.
		var obj map[string]any
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
		output = map[string]any{"text": text}
		fb := len(outputSchema) > 0
		return output, rawLen, fb
	}

	return map[string]any{}, 0, false
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
//
// The block scanner treats the opening fence as `\`\`\`<lang?>\n` — i.e. the
// language tag, if present, runs from the fence to the first newline and
// is dropped. A fenced block missing the newline (`\`\`\`json{...}\`\`\“)
// or with no language tag (`\`\`\`{...}\`\`\“) is recognised: the body
// is whatever sits between the opening and the next `\`\`\“ after the
// language line is consumed. Previously, the language-tag skip pinned
// to an unconditional IndexByte('\n') and a one-line fenced block
// silently lost its body to the outer loop's advance.
func extractJSONFromMarkdown(text string) string {
	const fence = "```"
	result := ""
	for {
		start := strings.Index(text, fence)
		if start == -1 {
			break
		}
		inner := text[start+len(fence):]
		// A leading "{" means there was no language tag and no newline;
		// jump straight to the body. Otherwise advance past the language
		// tag if a newline appears before the closing fence.
		if nl := strings.IndexByte(inner, '\n'); nl != -1 {
			fenceIdx := strings.Index(inner, fence)
			if fenceIdx == -1 || nl < fenceIdx {
				inner = inner[nl+1:]
			}
		}
		end := strings.Index(inner, fence)
		if end == -1 {
			break
		}
		block := strings.TrimSpace(inner[:end])
		if len(block) > 0 && block[0] == '{' {
			result = block
		}
		text = inner[end+len(fence):]
	}
	return result
}
