package bundle

import (
	"strings"
	"testing"
)

func TestValidateChatSurface_NodeRules(t *testing.T) {
	tests := []struct {
		name    string
		chat    *ChatSurface
		wantErr string
	}{
		{
			name: "unknown kind",
			chat: &ChatSurface{Nodes: map[string]ChatNode{
				"chat": {Kind: ChatNodeKind("wizard")},
			}},
			wantErr: "expected banner, human or silent",
		},
		{
			name: "non-human answer field",
			chat: &ChatSurface{Nodes: map[string]ChatNode{
				"work": {Kind: ChatNodeBanner, TextField: "message"},
			}},
			wantErr: "only a human node collects one",
		},
		{
			name: "human without answer field",
			chat: &ChatSurface{Nodes: map[string]ChatNode{
				"chat": {Kind: ChatNodeHuman},
			}},
			wantErr: "neither text_field nor approved_field",
		},
		{
			name: "no human node",
			chat: &ChatSurface{Nodes: map[string]ChatNode{
				"work": {Kind: ChatNodeBanner},
			}},
			wantErr: "no node is the operator's turn",
		},
		{
			name:    "no nodes",
			chat:    &ChatSurface{SeedVar: "initial_message"},
			wantErr: "no node is the operator's turn",
		},
		{
			name: "valid text conversation",
			chat: &ChatSurface{Nodes: map[string]ChatNode{
				"work": {Kind: ChatNodeBanner},
				"chat": {Kind: ChatNodeHuman, TextField: "message"},
			}},
		},
		{
			name: "valid approval conversation",
			chat: &ChatSurface{Nodes: map[string]ChatNode{
				"chat": {Kind: ChatNodeHuman, ApprovedField: "approved"},
			}},
		},
		{
			name: "valid hybrid conversation",
			chat: &ChatSurface{Nodes: map[string]ChatNode{
				"chat": {Kind: ChatNodeHuman, TextField: "feedback", ApprovedField: "accepted"},
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateChatSurface(tc.chat)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateChatSurface() unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateChatSurface() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestChatSurfaceNormalized(t *testing.T) {
	var nilChat *ChatSurface
	if got := nilChat.normalized(); got != nil {
		t.Fatalf("nil normalized = %#v, want nil", got)
	}
	if got := (&ChatSurface{Label: "  "}).normalized(); got != nil {
		t.Fatalf("empty normalized = %#v, want nil", got)
	}

	got := (&ChatSurface{
		Label:   "  Copi  ",
		SeedVar: " initial_message ",
		Launcher: &ChatLauncher{
			Presets: []ChatSeedPreset{{Value: "  Help me  ", Label: " Help "}},
		},
	}).normalized()
	if got == nil || got.Label != "Copi" || got.SeedVar != "initial_message" {
		t.Fatalf("normalized identity = %#v", got)
	}
	if got.Launcher == nil || got.Launcher.AllowOther == nil || !*got.Launcher.AllowOther {
		t.Fatalf("launcher AllowOther = %#v, want default true", got.Launcher)
	}
	if got.Launcher.Presets[0].Value != "Help me" {
		t.Fatalf("preset value = %q, want trimmed", got.Launcher.Presets[0].Value)
	}

	allowOther := false
	got = (&ChatSurface{
		SeedVar: "initial_message",
		Launcher: &ChatLauncher{
			Prompt:     "Choose",
			AllowOther: &allowOther,
		},
	}).normalized()
	if got == nil || got.Launcher == nil || got.Launcher.AllowOther == nil || *got.Launcher.AllowOther {
		t.Fatalf("explicit false AllowOther was not preserved: %#v", got)
	}
}

func TestValidateChatSurface_LauncherRequiresSeedVar(t *testing.T) {
	for _, tc := range []struct {
		name     string
		launcher *ChatLauncher
	}{
		{name: "free text launcher", launcher: &ChatLauncher{}},
		{
			name:     "preset launcher",
			launcher: &ChatLauncher{Presets: []ChatSeedPreset{{Value: "Help me"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := &ChatSurface{
				Launcher: tc.launcher,
				Nodes: map[string]ChatNode{
					"chat": {Kind: ChatNodeHuman, TextField: "message"},
				},
			}
			err := validateChatSurface(chat)
			if err == nil || !strings.Contains(err.Error(), "seed_var is empty") {
				t.Fatalf("validateChatSurface() error = %v, want missing seed_var", err)
			}

			chat.SeedVar = "initial_message"
			if err := validateChatSurface(chat); err != nil {
				t.Fatalf("validateChatSurface() with seed_var: %v", err)
			}
		})
	}
}

func TestValidateChatSurface_EditorProposalRequiresContext(t *testing.T) {
	chat := &ChatSurface{
		Nodes: map[string]ChatNode{
			"chat": {Kind: ChatNodeHuman, TextField: "message"},
		},
		Editor: &ChatEditorSurface{Proposals: true},
	}
	if err := validateChatSurface(chat); err == nil || !strings.Contains(err.Error(), "require editor context") {
		t.Fatalf("validateChatSurface() error = %v, want editor-context dependency", err)
	}
	chat.Editor.Context = true
	if err := validateChatSurface(chat); err != nil {
		t.Fatalf("validateChatSurface() with context: %v", err)
	}
}
