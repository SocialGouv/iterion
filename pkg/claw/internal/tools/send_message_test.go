package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendUserMessage(t *testing.T) {
	tests := []struct {
		name    string
		input   map[string]any
		wantErr string
		wantOut string
	}{
		{
			name: "happy path with message and status",
			input: map[string]any{
				"message": "Hello user",
				"status":  "proactive",
			},
			wantOut: "Hello user",
		},
		{
			name:    "missing message",
			input:   map[string]any{"status": "normal"},
			wantErr: "'message' is required",
		},
		{
			name:    "empty message",
			input:   map[string]any{"message": "", "status": "normal"},
			wantErr: "'message' is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ExecuteSendUserMessage(tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(out, tt.wantOut) {
				t.Fatalf("output %q does not contain %q", out, tt.wantOut)
			}
		})
	}
}

func TestSendUserMessage_Attachments(t *testing.T) {
	t.Run("valid file attachment", func(t *testing.T) {
		// Create a temp file to attach.
		dir := t.TempDir()
		tmpFile := filepath.Join(dir, "test.txt")
		os.WriteFile(tmpFile, []byte("hello"), 0o644)

		out, err := ExecuteSendUserMessage(map[string]any{
			"message":     "See attached",
			"status":      "normal",
			"attachments": []any{tmpFile},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)
		attachments, ok := parsed["attachments"].([]any)
		if !ok || len(attachments) != 1 {
			t.Fatalf("expected 1 attachment, got %v", parsed["attachments"])
		}
		att := attachments[0].(map[string]any)
		if att["size"].(float64) != 5 {
			t.Errorf("expected size=5, got %v", att["size"])
		}
		if att["isImage"].(bool) {
			t.Error("expected isImage=false for .txt")
		}
	})

	t.Run("image file attachment", func(t *testing.T) {
		dir := t.TempDir()
		imgFile := filepath.Join(dir, "photo.png")
		os.WriteFile(imgFile, []byte("PNG"), 0o644)

		out, err := ExecuteSendUserMessage(map[string]any{
			"message":     "Image",
			"status":      "normal",
			"attachments": []any{imgFile},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)
		attachments := parsed["attachments"].([]any)
		att := attachments[0].(map[string]any)
		if !att["isImage"].(bool) {
			t.Error("expected isImage=true for .png")
		}
	})

	t.Run("nonexistent file attachment", func(t *testing.T) {
		_, err := ExecuteSendUserMessage(map[string]any{
			"message":     "Oops",
			"status":      "normal",
			"attachments": []any{"/nonexistent/file.txt"},
		})
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
		if !strings.Contains(err.Error(), "attachment") {
			t.Errorf("expected error about attachment, got: %v", err)
		}
	})

	t.Run("empty attachments array", func(t *testing.T) {
		out, err := ExecuteSendUserMessage(map[string]any{
			"message":     "No attachments",
			"status":      "normal",
			"attachments": []any{},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)
		// Empty attachments input produces nil (no resolved attachments).
		// Rust: Option<Vec<Attachment>> with empty input = None = null.
		if parsed["attachments"] != nil {
			t.Errorf("expected null attachments for empty input array, got %v", parsed["attachments"])
		}
	})

	t.Run("no attachments field", func(t *testing.T) {
		out, err := ExecuteSendUserMessage(map[string]any{
			"message": "Plain",
			"status":  "normal",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)
		// Rust: Option<Vec<Attachment>> serializes as null when None.
		// attachments key should exist but be null.
		_, exists := parsed["attachments"]
		if !exists {
			t.Error("expected attachments key to exist in response")
		}
		if parsed["attachments"] != nil {
			t.Errorf("expected null attachments, got %v", parsed["attachments"])
		}
	})

	t.Run("relative path resolves to absolute", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "rel.txt"), []byte("x"), 0o644)
		origDir, _ := os.Getwd()
		os.Chdir(dir)
		defer os.Chdir(origDir)

		out, err := ExecuteSendUserMessage(map[string]any{
			"message":     "Rel",
			"status":      "normal",
			"attachments": []any{"rel.txt"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)
		att := parsed["attachments"].([]any)[0].(map[string]any)
		if !filepath.IsAbs(att["path"].(string)) {
			t.Errorf("expected absolute path, got %q", att["path"])
		}
	})
}
