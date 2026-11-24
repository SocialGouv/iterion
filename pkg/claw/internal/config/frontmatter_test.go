package config

import (
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantModel    *string
		wantPermMode *string
		wantTools    []string
		wantBody     string
		wantErr      bool
	}{
		{
			name:      "valid frontmatter with model override",
			input:     "---\nmodel: claude-sonnet-4-6\n---\n# Hello\nBody here.",
			wantModel: strPtr("claude-sonnet-4-6"),
			wantBody:  "# Hello\nBody here.",
		},
		{
			name:     "no frontmatter passthrough",
			input:    "# Just a heading\nSome content.",
			wantBody: "# Just a heading\nSome content.",
		},
		{
			name:     "empty frontmatter",
			input:    "---\n---\nBody after empty.",
			wantBody: "Body after empty.",
		},
		{
			name:         "multiple fields",
			input:        "---\nmodel: opus\npermissionMode: auto\n---\nContent.",
			wantModel:    strPtr("opus"),
			wantPermMode: strPtr("auto"),
			wantBody:     "Content.",
		},
		{
			name:      "allowedTools as list",
			input:     "---\nallowedTools:\n- bash\n- read\n- write\n---\nBody.",
			wantTools: []string{"bash", "read", "write"},
			wantBody:  "Body.",
		},
		{
			name:    "malformed no closing delimiter",
			input:   "---\nmodel: opus\nno closing here",
			wantErr: true,
		},
		{
			name:      "body preserved correctly",
			input:     "---\nmodel: test\n---\nLine 1\nLine 2\nLine 3",
			wantModel: strPtr("test"),
			wantBody:  "Line 1\nLine 2\nLine 3",
		},
		{
			name:      "closing delimiter at EOF without trailing newline",
			input:     "---\nmodel: eof-test\n---",
			wantModel: strPtr("eof-test"),
			wantBody:  "",
		},
		{
			name:         "all fields combined",
			input:        "---\nmodel: claude-opus-4-6\npermissionMode: manual\nallowedTools:\n- edit\n- bash\n---\nReal content.",
			wantModel:    strPtr("claude-opus-4-6"),
			wantPermMode: strPtr("manual"),
			wantTools:    []string{"edit", "bash"},
			wantBody:     "Real content.",
		},
		{
			name:     "horizontal rule not on line 1 is not frontmatter",
			input:    "# Heading\n\n---\n\nSome text after a horizontal rule.",
			wantBody: "# Heading\n\n---\n\nSome text after a horizontal rule.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, body, err := ParseFrontmatter([]byte(tt.input))

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Check model
			if tt.wantModel == nil {
				if cfg.Model != nil {
					t.Errorf("Model: got %q, want nil", *cfg.Model)
				}
			} else {
				if cfg.Model == nil {
					t.Fatalf("Model: got nil, want %q", *tt.wantModel)
				}
				if *cfg.Model != *tt.wantModel {
					t.Errorf("Model: got %q, want %q", *cfg.Model, *tt.wantModel)
				}
			}

			// Check permissionMode
			if tt.wantPermMode == nil {
				if cfg.PermissionMode != nil {
					t.Errorf("PermissionMode: got %q, want nil", *cfg.PermissionMode)
				}
			} else {
				if cfg.PermissionMode == nil {
					t.Fatalf("PermissionMode: got nil, want %q", *tt.wantPermMode)
				}
				if *cfg.PermissionMode != *tt.wantPermMode {
					t.Errorf("PermissionMode: got %q, want %q", *cfg.PermissionMode, *tt.wantPermMode)
				}
			}

			// Check allowedTools
			if len(tt.wantTools) == 0 {
				if len(cfg.AllowedTools) != 0 {
					t.Errorf("AllowedTools: got %v, want empty", cfg.AllowedTools)
				}
			} else {
				if len(cfg.AllowedTools) != len(tt.wantTools) {
					t.Fatalf("AllowedTools: got %v, want %v", cfg.AllowedTools, tt.wantTools)
				}
				for i, want := range tt.wantTools {
					if cfg.AllowedTools[i] != want {
						t.Errorf("AllowedTools[%d]: got %q, want %q", i, cfg.AllowedTools[i], want)
					}
				}
			}

			// Check body
			if string(body) != tt.wantBody {
				t.Errorf("body:\ngot:  %q\nwant: %q", string(body), tt.wantBody)
			}
		})
	}
}

func strPtr(s string) *string {
	return &s
}
