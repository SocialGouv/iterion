package model

import "strings"

// previewURLDirective is the line prefix that tool nodes use to declare
// a URL the editor's Browser pane can render. Anything matching the
// shape `[iterion] preview_url=<url> [kind=<k>] [scope=<s>]` (one per
// line) is captured. Tool stdout that does not match is left alone.
const previewURLDirective = "[iterion] preview_url="

// previewScreenshotDirective is the companion line prefix tool nodes
// use to register a captured screenshot of the preview. Shape:
// `[iterion] preview_screenshot=<path> [url=<u>] [tool_call_id=<id>]`.
// `<path>` MUST be a host-readable absolute path to a PNG/JPEG file —
// the runtime will read it and persist it as a versioned attachment.
const previewScreenshotDirective = "[iterion] preview_screenshot="

// scanDirectiveLines walks tool stdout and yields the token list of
// each line that starts with directive. The first token is whatever
// comes after `directive=`; subsequent tokens are space-separated
// `key=value` pairs the caller can interpret. Lines that don't match
// (or whose first token is empty) are skipped silently — tools can
// freely interleave their own log output.
func scanDirectiveLines(output, directive string) [][]string {
	if output == "" || !strings.Contains(output, directive) {
		return nil
	}
	var found [][]string
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, directive) {
			continue
		}
		tokens := strings.Fields(line[len(directive):])
		if len(tokens) == 0 || tokens[0] == "" {
			continue
		}
		found = append(found, tokens)
	}
	return found
}

// scanPreviewURLs walks the captured stdout of a tool node and returns
// one event payload per directive line. Each payload is shaped to drop
// directly into a store.EventPreviewURLAvailable.Data map: required
// `url`, optional `kind` and `scope` (defaulting to "external"), plus
// `source: "tool-stdout"` so consumers can distinguish manual user
// entry from workflow-emitted URLs.
func scanPreviewURLs(output string) []map[string]interface{} {
	lines := scanDirectiveLines(output, previewURLDirective)
	if len(lines) == 0 {
		return nil
	}
	found := make([]map[string]interface{}, 0, len(lines))
	for _, tokens := range lines {
		data := map[string]interface{}{
			"url":    tokens[0],
			"source": "tool-stdout",
			"scope":  "external",
		}
		for _, kv := range tokens[1:] {
			eq := strings.IndexByte(kv, '=')
			if eq <= 0 {
				continue
			}
			k, v := kv[:eq], kv[eq+1:]
			switch k {
			case "kind", "scope":
				if v != "" {
					data[k] = v
				}
			}
		}
		found = append(found, data)
	}
	return found
}

// ScreenshotDirective is one parsed `[iterion] preview_screenshot=...`
// line. Path is host-absolute; URL and ToolCallID are optional.
type ScreenshotDirective struct {
	Path       string
	URL        string
	ToolCallID string
}

// scanPreviewScreenshots walks tool stdout and returns one
// ScreenshotDirective per matching line. The runtime hook is
// responsible for actually reading the file and persisting it.
func scanPreviewScreenshots(output string) []ScreenshotDirective {
	lines := scanDirectiveLines(output, previewScreenshotDirective)
	if len(lines) == 0 {
		return nil
	}
	found := make([]ScreenshotDirective, 0, len(lines))
	for _, tokens := range lines {
		dir := ScreenshotDirective{Path: tokens[0]}
		for _, kv := range tokens[1:] {
			eq := strings.IndexByte(kv, '=')
			if eq <= 0 {
				continue
			}
			k, v := kv[:eq], kv[eq+1:]
			switch k {
			case "url":
				dir.URL = v
			case "tool_call_id":
				dir.ToolCallID = v
			}
		}
		found = append(found, dir)
	}
	return found
}
