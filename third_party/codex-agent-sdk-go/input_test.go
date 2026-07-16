package codexsdk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInputHelpers_BlockConstruction(t *testing.T) {
	content := Blocks(
		TextInput("Describe these inputs."),
		ImageInput("data:image/png;base64,AQID"),
		PathInput("/tmp/example.pdf"),
	)

	msg := NewUserMessage(content)
	data, err := json.Marshal(msg)
	require.NoError(t, err)

	assert.JSONEq(t, `{
		"type": "user",
		"message": {
			"role": "user",
			"content": [
				{"type": "text", "text": "Describe these inputs."},
				{"type": "image", "url": "data:image/png;base64,AQID"},
				{"type": "mention", "name": "example.pdf", "path": "/tmp/example.pdf"}
			]
		}
	}`, string(data))
}

func TestImageFileInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.png")
	require.NoError(t, os.WriteFile(path, []byte("pngdata"), 0o600))

	block, err := ImageFileInput(path)
	require.NoError(t, err)

	assert.Equal(t, BlockTypeImage, block.Type)
	assert.Equal(t, "data:image/png;base64,cG5nZGF0YQ==", block.URL)
}

func TestLocalImageInput(t *testing.T) {
	block := LocalImageInput("/tmp/example.png")
	assert.Equal(t, BlockTypeLocalImage, block.Type)
	assert.Equal(t, "/tmp/example.png", block.Path)
}

func TestPrepareExecContent(t *testing.T) {
	content := Blocks(
		TextInput("Explain these files."),
		PathInput("/tmp/example.pdf"),
		&InputLocalImageBlock{Type: BlockTypeLocalImage, Path: "/tmp/image.png"},
	)

	prompt, images, ok := prepareExecContent(content, &CodexAgentOptions{
		Images: []string{"/tmp/extra.png"},
	})
	require.True(t, ok)

	assert.Equal(t, "Explain these files.\n\n@/tmp/example.pdf", prompt)
	assert.Equal(t, []string{"/tmp/extra.png", "/tmp/image.png"}, images)
}

func TestPrepareExecContent_ImageURLRequiresAppServer(t *testing.T) {
	_, _, ok := prepareExecContent(Blocks(ImageInput("data:image/png;base64,AQID")), &CodexAgentOptions{})
	assert.False(t, ok)
}
