package bundle

import (
	"strings"
	"testing"
)

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
			chat := &ChatSurface{Launcher: tc.launcher}
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
