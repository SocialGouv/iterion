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
// structuredObject is THE definition of a structured answer, shared by
// parseSDKOutput (which ships it) and the render guards (which exempt it):
// the SDK's structured object when non-empty — claude-code's stream-json
// emits `structured_output: {}` for tool-using sessions where no second-pass
// formatter ran, and treating that as a result once wedged the runtime into
// "raw_output_len=0, parse_fallback=false" with every required field
// missing — a non-map structured value that round-trips to a non-empty
// object, or a JSON object in the result text, direct or in a fenced block.
// found is false when there is none; rawLen is the text's length when the
// object came from the text.
func structuredObject(resultText *string, structuredOutput any) (obj map[string]any, rawLen int, found bool) {
	if structuredOutput != nil {
		if m, ok := structuredOutput.(map[string]any); ok {
			if len(m) > 0 {
				return m, 0, true
			}
		} else if b, err := json.Marshal(structuredOutput); err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil && len(m) > 0 {
				return m, len(b), true
			}
		}
	}
	if resultText != nil && *resultText != "" {
		text := *resultText
		var m map[string]any
		if json.Unmarshal([]byte(text), &m) == nil {
			return m, len(text), true
		}
		if extracted := extractJSONFromMarkdown(text); extracted != "" {
			if json.Unmarshal([]byte(extracted), &m) == nil {
				return m, len(text), true
			}
		}
	}
	return nil, 0, false
}

func parseSDKOutput(resultText *string, structuredOutput any, outputSchema json.RawMessage) (output map[string]any, rawLen int, fallback bool) {
	// Priority 1 and 2: a structured answer, from the SDK object or the text.
	if obj, n, found := structuredObject(resultText, structuredOutput); found {
		return obj, n, false
	}

	// Priority 3: the result text as text.
	if resultText != nil && *resultText != "" {
		text := *resultText
		rawLen = len(text)

		// Fallback: wrap raw text. If the output schema expects exactly one
		// required field of type "string" (e.g. shell_result {result}), a
		// text-only backend — kimi, or any CLI agent that cannot emit a
		// JSON-schema-shaped result like claude_code/codex do — satisfies it
		// by placing its final text in that field. Treat this as a VALID
		// result (fallback=false), not a validation failure. Multi-field or
		// non-string schemas still require real JSON and keep the {"text":…}
		// fallback so the retry path can ask the model for structured output.
		if field := singleRequiredStringField(outputSchema); field != "" {
			return map[string]any{field: text}, rawLen, false
		}
		output = map[string]any{"text": text}
		fb := len(outputSchema) > 0
		return output, rawLen, fb
	}

	return map[string]any{}, 0, false
}

// singleRequiredStringField returns the name of the schema's sole required
// field when that field is of type "string", or "" otherwise. Lets a
// text-output backend satisfy a single-string-field schema (e.g. shell_result)
// by wrapping its final text under that field.
func singleRequiredStringField(outputSchema json.RawMessage) string {
	if len(outputSchema) == 0 {
		return ""
	}
	var s struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(outputSchema, &s) != nil || len(s.Required) != 1 {
		return ""
	}
	if prop, ok := s.Properties[s.Required[0]]; ok && prop.Type == "string" {
		return s.Required[0]
	}
	return ""
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
