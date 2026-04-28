package tools

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func SendUserMessageTool() api.Tool {
	return api.Tool{
		Name:        "send_user_message",
		Description: "Send a message to the user with optional attachments.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"message":     {Type: "string", Description: "The message text to send."},
				"status":      {Type: "string", Description: "Message status: normal or proactive."},
				"attachments": {Type: "array", Description: "File paths to attach to the message."},
			},
			Required: []string{"message", "status"},
		},
	}
}

// ResolvedAttachment holds metadata for a resolved file attachment.
type ResolvedAttachment struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	IsImage bool   `json:"isImage"`
}

func ExecuteSendUserMessage(input map[string]any) (string, error) {
	message, ok := input["message"].(string)
	if !ok || strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("send_user_message: 'message' is required and must not be empty")
	}
	var attachments []ResolvedAttachment
	if paths, ok := input["attachments"].([]any); ok {
		for _, p := range paths {
			pathStr, ok := p.(string)
			if !ok {
				continue
			}
			resolved, err := resolveAttachment(pathStr)
			if err != nil {
				return "", fmt.Errorf("send_user_message: attachment %q: %w", pathStr, err)
			}
			attachments = append(attachments, *resolved)
		}
	}

	// Match Rust's BriefOutput: {message, attachments, sentAt} — status is NOT echoed.
	// attachments is nil when no files were resolved (including empty input array).
	// nil serializes as JSON null, matching Rust's Option<Vec<Attachment>> = None.
	result := map[string]any{
		"message":     message,
		"sentAt":      time.Now().UTC().Format(time.RFC3339),
		"attachments": attachments,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// resolveAttachment canonicalizes a file path and returns attachment metadata.
// filepath.Abs is required before EvalSymlinks because EvalSymlinks does not
// guarantee an absolute result for relative inputs (unlike Rust's canonicalize).
func resolveAttachment(path string) (*ResolvedAttachment, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	return &ResolvedAttachment{
		Path:    resolved,
		Size:    info.Size(),
		IsImage: isImagePath(resolved),
	}, nil
}

// isImagePath checks if a file path has an image extension.
func isImagePath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg":
		return true
	default:
		return false
	}
}
