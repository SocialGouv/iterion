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

// scanPreviewURLs walks the captured stdout of a tool node and returns
// one event payload per directive line. Each payload is shaped to drop
// directly into a store.EventPreviewURLAvailable.Data map: required
// `url`, optional `kind` and `scope` (defaulting to "external"), plus
// `source: "tool-stdout"` so consumers can distinguish manual user
// entry from workflow-emitted URLs.
//
// The scanner ignores lines that don't start with the prefix, so a
// tool can mix the directive with its own logging output without
// extra escaping.
func scanPreviewURLs(output string) []map[string]interface{} {
	if output == "" || !strings.Contains(output, previewURLDirective) {
		return nil
	}

	var found []map[string]interface{}
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, previewURLDirective) {
			continue
		}

		tokens := strings.Fields(line[len(previewURLDirective):])
		if len(tokens) == 0 {
			continue
		}
		url := tokens[0]
		if url == "" {
			continue
		}

		data := map[string]interface{}{
			"url":    url,
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
	if output == "" || !strings.Contains(output, previewScreenshotDirective) {
		return nil
	}
	var found []ScreenshotDirective
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, previewScreenshotDirective) {
			continue
		}
		tokens := strings.Fields(line[len(previewScreenshotDirective):])
		if len(tokens) == 0 {
			continue
		}
		path := tokens[0]
		if path == "" {
			continue
		}
		dir := ScreenshotDirective{Path: path}
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
