package tool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clawapi "github.com/SocialGouv/claw-code-go/pkg/api"
)

func hasTool(r *Registry, name string) bool {
	_, err := r.Resolve(name)
	return err == nil
}

func TestRegisterClawBuiltins_RegistersStandardSet(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawBuiltins(r, ""); err != nil {
		t.Fatalf("RegisterClawBuiltins: %v", err)
	}
	want := []string{"read_file", "write_file", "glob", "grep", "file_edit", "web_fetch", "bash"}
	for _, name := range want {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered", name)
		}
	}
}

func TestRegisterClawBuiltins_DoesNotRegisterComputerUse(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawBuiltins(r, ""); err != nil {
		t.Fatalf("RegisterClawBuiltins: %v", err)
	}
	for _, name := range []string{"read_image", "screenshot"} {
		if hasTool(r, name) {
			t.Errorf("expected %q NOT registered by default; vision tools are opt-in", name)
		}
	}
}

func TestRegisterClawComputerUse_RegistersBoth(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}
	for _, name := range []string{"read_image", "screenshot"} {
		if !hasTool(r, name) {
			t.Errorf("expected %q registered after opt-in", name)
		}
	}
}

func TestRegisterClawComputerUse_ReadImageRoundTrip(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}

	// Tiny 1x1 transparent PNG.
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.png")
	if err := os.WriteFile(path, pngBytes, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tool, err := r.Resolve("read_image")
	if err != nil {
		t.Fatalf("read_image not in registry: %v", err)
	}
	input, _ := json.Marshal(map[string]any{"path": path})
	out, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute read_image: %v", err)
	}

	// Output is JSON-encoded ReadImageResult.
	var decoded struct {
		Description string                `json:"description"`
		Blocks      []clawapi.ContentBlock `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(decoded.Blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(decoded.Blocks))
	}
	block := decoded.Blocks[0]
	if block.Type != "image" || block.Source == nil || block.Source.Type != "base64" {
		t.Errorf("expected base64 image block, got %+v", block)
	}
	if block.Source.MediaType != "image/png" {
		t.Errorf("expected image/png, got %q", block.Source.MediaType)
	}
	if block.Source.Data == "" {
		t.Errorf("expected non-empty base64 data")
	}
}

func TestRegisterClawComputerUse_ReadImagePropagatesError(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}
	tool, _ := r.Resolve("read_image")

	// Missing both path and url → underlying tool errors. The
	// adapter must surface that, not swallow it.
	input, _ := json.Marshal(map[string]any{})
	if _, err := tool.Execute(context.Background(), input); err == nil {
		t.Fatal("expected error when neither path nor url given")
	}
}

func TestRegisterClawComputerUse_ScreenshotReturnsAPIError(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatalf("RegisterClawComputerUse: %v", err)
	}
	tool, _ := r.Resolve("screenshot")

	input, _ := json.Marshal(map[string]any{})
	_, err := tool.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected stub error from screenshot")
	}
	var apiErr *clawapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *api.APIError to propagate through adapter, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 501 {
		t.Errorf("expected status 501, got %d", apiErr.StatusCode)
	}
}

func TestRegisterClawComputerUse_ReadImageRejectsHTTPRedirect(t *testing.T) {
	// Defense-in-depth: the iterion-level adapter must inherit the
	// underlying tool's HTTPS-only check, including redirects.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("plain http should never be reached; redirect must abort")
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/img.png", http.StatusFound)
	}))
	defer tls.Close()

	prev := http.DefaultClient
	http.DefaultClient = tls.Client()
	t.Cleanup(func() { http.DefaultClient = prev })

	r := NewRegistry()
	if err := RegisterClawComputerUse(r); err != nil {
		t.Fatal(err)
	}
	tool, _ := r.Resolve("read_image")
	input, _ := json.Marshal(map[string]any{"url": tls.URL + "/start"})
	_, err := tool.Execute(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Errorf("expected redirect-to-non-https error, got %v", err)
	}
}
