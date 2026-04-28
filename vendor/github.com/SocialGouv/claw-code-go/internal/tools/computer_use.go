package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

// maxImageBytes caps both file and HTTP responses for read_image. 5 MB
// keeps a single image well under typical Anthropic vision payload limits
// (~30 MB for the whole request) while leaving headroom for prompt + tools.
const maxImageBytes = 5 * 1024 * 1024

// ReadImageTool returns the tool definition for loading an image into the
// conversation as an Anthropic vision content block.
func ReadImageTool() api.Tool {
	return api.Tool{
		Name: "read_image",
		Description: "Load an image (PNG, JPEG, GIF, WebP) from a local file or HTTPS URL " +
			"and return it as a base64-encoded vision content block the model can see.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"path": {
					Type:        "string",
					Description: "Local filesystem path to the image. Mutually exclusive with url.",
				},
				"url": {
					Type:        "string",
					Description: "HTTPS URL to fetch. Plain http, file, and data schemes are rejected. Mutually exclusive with path.",
				},
				"description": {
					Type:        "string",
					Description: "Optional caption for the image in the conversation. Defaults to the filename or URL.",
				},
			},
		},
	}
}

// ScreenshotTool returns the tool definition for capturing a screenshot of
// the host display. Currently a stub: no platform implementation exists.
func ScreenshotTool() api.Tool {
	return api.Tool{
		Name:        "screenshot",
		Description: "Capture a screenshot of the host display. Not yet implemented for any platform.",
		InputSchema: api.InputSchema{
			Type:       "object",
			Properties: map[string]api.Property{},
		},
	}
}

// ReadImageResult is what ExecuteReadImage produces. The Blocks field is
// shaped so callers can splice it directly into a tool_result content list
// destined for the next user-turn message.
type ReadImageResult struct {
	Description string             `json:"description"`
	Blocks      []api.ContentBlock `json:"blocks"`
}

// ExecuteReadImage validates input, loads bytes from path or url, sniffs
// the media type, and returns a single image ContentBlock encoded as
// base64. Exactly one of path / url must be provided.
func ExecuteReadImage(ctx context.Context, input map[string]any) (ReadImageResult, error) {
	pathStr, _ := input["path"].(string)
	urlStr, _ := input["url"].(string)
	caption, _ := input["description"].(string)

	pathStr = strings.TrimSpace(pathStr)
	urlStr = strings.TrimSpace(urlStr)

	if pathStr == "" && urlStr == "" {
		return ReadImageResult{}, fmt.Errorf("read_image: either 'path' or 'url' is required")
	}
	if pathStr != "" && urlStr != "" {
		return ReadImageResult{}, fmt.Errorf("read_image: 'path' and 'url' are mutually exclusive")
	}

	var (
		data      []byte
		mediaType string
		source    string
		err       error
	)

	if pathStr != "" {
		data, mediaType, err = loadImageFromPath(pathStr)
		source = filepath.Base(pathStr)
	} else {
		data, mediaType, err = loadImageFromURL(ctx, urlStr)
		source = urlStr
	}
	if err != nil {
		return ReadImageResult{}, err
	}

	if caption == "" {
		caption = source
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	block := api.ContentBlock{
		Type: "image",
		Source: &api.ImageSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      encoded,
		},
	}
	return ReadImageResult{Description: caption, Blocks: []api.ContentBlock{block}}, nil
}

// ExecuteScreenshot is a stub. The Anthropic vision content path is in
// place — what's missing is a cross-platform capture backend. Returning a
// typed APIError lets callers route it through the same retry/classifier
// machinery as upstream errors instead of leaking string-matched fallbacks.
func ExecuteScreenshot(ctx context.Context, input map[string]any) (ReadImageResult, error) {
	return ReadImageResult{}, &api.APIError{
		StatusCode: 501,
		Message:    "screenshot not yet implemented for this platform",
	}
}

func loadImageFromPath(path string) ([]byte, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("read_image: stat %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, "", fmt.Errorf("read_image: %s is a directory", path)
	}
	if info.Size() > maxImageBytes {
		return nil, "", fmt.Errorf("read_image: %s exceeds 5 MB limit (%d bytes)", path, info.Size())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read_image: %w", err)
	}
	mt, err := sniffImageMediaType(filepath.Ext(path), data)
	if err != nil {
		return nil, "", err
	}
	return data, mt, nil
}

// httpsOnlyClient is the package-default client used by loadImageFromURL.
// CheckRedirect rejects any hop that downgrades to http:// so a malicious
// origin can't redirect a starts-as-https URL to plaintext and exfiltrate
// the request through an attacker-controlled host.
var httpsOnlyClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("read_image: redirect to non-https URL rejected (%s)", req.URL.Scheme)
		}
		return nil
	},
}

func loadImageFromURL(ctx context.Context, raw string) ([]byte, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("read_image: invalid url: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, "", fmt.Errorf("read_image: only https URLs are accepted (got scheme %q)", u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", fmt.Errorf("read_image: build request: %w", err)
	}
	client := httpsOnlyClient
	// Tests may swap http.DefaultClient with an httptest TLS client. When
	// that happens we honour their override since CheckRedirect is the
	// only thing we care about and httptest doesn't redirect.
	if http.DefaultClient != nil && http.DefaultClient.Transport != nil {
		client = &http.Client{
			Transport:     http.DefaultClient.Transport,
			CheckRedirect: httpsOnlyClient.CheckRedirect,
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("read_image: fetch %s: %w", raw, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", &api.APIError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("read_image: fetch %s: HTTP %d", raw, resp.StatusCode),
		}
	}

	limited := io.LimitReader(resp.Body, maxImageBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Errorf("read_image: read body: %w", err)
	}
	if int64(len(data)) > maxImageBytes {
		return nil, "", fmt.Errorf("read_image: response from %s exceeds 5 MB limit", raw)
	}

	mt, err := sniffImageMediaType(extFromContentType(resp.Header.Get("Content-Type")), data)
	if err != nil {
		return nil, "", err
	}
	return data, mt, nil
}

// sniffImageMediaType picks the best media type using the extension hint
// first (cheaper, deterministic) and falling back to magic-byte sniffing
// when the hint is missing or ambiguous.
func sniffImageMediaType(extHint string, data []byte) (string, error) {
	switch strings.ToLower(strings.TrimPrefix(extHint, ".")) {
	case "png":
		return "image/png", nil
	case "jpg", "jpeg":
		return "image/jpeg", nil
	case "gif":
		return "image/gif", nil
	case "webp":
		return "image/webp", nil
	}

	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", nil
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg", nil
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif", nil
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp", nil
	}
	return "", fmt.Errorf("read_image: unsupported or unrecognized image format (allowed: png, jpeg, gif, webp)")
}

// extFromContentType maps an HTTP Content-Type header to a file extension
// hint suitable for sniffImageMediaType.
func extFromContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch ct {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ""
}
