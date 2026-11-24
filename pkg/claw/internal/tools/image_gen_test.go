package tools

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

type testImageGenerator struct {
	data   []byte
	err    error
	called int
}

func (g *testImageGenerator) GenerateImage(_ context.Context, _ string) (api.GeneratedImage, error) {
	g.called++
	return api.GeneratedImage{Data: g.data}, g.err
}

func toolTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestExecuteImageGenSavesVerifiedPNG(t *testing.T) {
	root := t.TempDir()
	gen := &testImageGenerator{data: toolTestPNG(t, 2, 3)}
	result, err := ExecuteImageGen(context.Background(), map[string]any{"prompt": "red square"}, gen, root)
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(strings.Split(result, " (2 x 3)")[0], "Generated PNG saved to ")
	if path == result || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		t.Fatalf("result has no safe absolute path: %q", result)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, gen.data) {
		t.Fatalf("saved PNG = %d bytes, %v", len(data), err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v", info.Mode())
	}
	if gen.called != 1 {
		t.Fatalf("calls = %d", gen.called)
	}
}

func TestExecuteImageGenRejectsBadInputWithoutArtifact(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"invalid PNG", []byte("not an image")},
		{"oversized dimensions", toolTestPNG(t, maxGeneratedImageDimension+1, 1)},
		{"oversized bytes", make([]byte, maxGeneratedImageBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			_, err := ExecuteImageGen(context.Background(), map[string]any{"prompt": "a"}, &testImageGenerator{data: tc.data}, root)
			if err == nil {
				t.Fatal("invalid image was saved")
			}
			files, readErr := os.ReadDir(root)
			if readErr != nil || len(files) != 0 {
				t.Fatalf("partial files = %v, %v", files, readErr)
			}
		})
	}
	gen := &testImageGenerator{data: toolTestPNG(t, 1, 1)}
	_, err := ExecuteImageGen(context.Background(), map[string]any{}, gen, t.TempDir())
	if err == nil || gen.called != 0 {
		t.Fatalf("missing prompt: %v, calls=%d", err, gen.called)
	}
	_, err = ExecuteImageGen(context.Background(), map[string]any{"prompt": "a"}, nil, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("unsupported provider: %v", err)
	}
}

func TestImageGenToolSchema(t *testing.T) {
	tool := ImageGenTool()
	if tool.Name != "image_gen" || fmt.Sprint(tool.InputSchema.Required) != "[prompt]" {
		t.Fatalf("tool = %+v", tool)
	}
}
