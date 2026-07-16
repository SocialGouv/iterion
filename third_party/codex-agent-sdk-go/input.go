package codexsdk

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Text creates text-only user content.
func Text(text string) UserMessageContent {
	return NewUserMessageContent(text)
}

// TextInput creates a text block for block-based user content.
func TextInput(text string) *TextBlock {
	return &TextBlock{
		Type: BlockTypeText,
		Text: text,
	}
}

// Blocks creates block-based user content.
func Blocks(blocks ...ContentBlock) UserMessageContent {
	return NewUserMessageContentBlocks(blocks)
}

// ImageInput creates an app-server image block from a URL or data URL.
func ImageInput(url string) *InputImageBlock {
	url = strings.TrimSpace(url)

	return &InputImageBlock{
		Type: BlockTypeImage,
		URL:  url,
	}
}

// PathInput creates a generic local path mention block.
func PathInput(path string) *InputMentionBlock {
	return &InputMentionBlock{
		Type: BlockTypeMention,
		Name: filepath.Base(path),
		Path: path,
	}
}

// LocalImageInput creates a local image-path input block.
func LocalImageInput(path string) *InputLocalImageBlock {
	return &InputLocalImageBlock{
		Type: BlockTypeLocalImage,
		Path: path,
	}
}

// ImageFileInput reads a local image file and creates an inline data-URL image block.
func ImageFileInput(path string) (*InputImageBlock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read image file: %w", err)
	}

	mediaType := detectImageMediaType(path, data)
	if !strings.HasPrefix(mediaType, "image/") {
		return nil, fmt.Errorf("unsupported image media type %q", mediaType)
	}

	return ImageInput(
		fmt.Sprintf("data:%s;base64,%s", mediaType, base64.StdEncoding.EncodeToString(data)),
	), nil
}

func detectImageMediaType(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return http.DetectContentType(data)
	}
}

func formatPathMention(path string) string {
	if path == "" {
		return "@"
	}

	return "@" + path
}
