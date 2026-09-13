package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestValidateLaunchRunSource(t *testing.T) {
	got, err := validateLaunchRunSource(&launchRunSource{
		Kind:           store.RunSourceKindStudioChat,
		ClientID:       "client-123",
		ConversationID: "conversation:456",
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.Kind != store.RunSourceKindStudioChat || got.ClientID != "client-123" || got.ConversationID != "conversation:456" {
		t.Fatalf("source = %+v", got)
	}

	for name, input := range map[string]*launchRunSource{
		"forged schedule": {Kind: store.RunSourceKindSchedule, ClientID: "c", ConversationID: "x"},
		"missing client":  {Kind: store.RunSourceKindStudioChat, ConversationID: "x"},
		"missing convo":   {Kind: store.RunSourceKindStudioChat, ClientID: "c"},
		"unsafe client":   {Kind: store.RunSourceKindStudioChat, ClientID: "../c", ConversationID: "x"},
		"too long":        {Kind: store.RunSourceKindStudioChat, ClientID: strings.Repeat("a", studioChatSourceIDMax+1), ConversationID: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateLaunchRunSource(input); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
