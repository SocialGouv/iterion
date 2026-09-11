package server

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

const studioChatSourceIDMax = 128

var studioChatSourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// launchRunSource is the deliberately narrow public launch provenance shape.
// Do not embed store.RunSource here: accepting its dispatcher/schedule fields
// would let a browser pollute overlap gates and source filters.
type launchRunSource struct {
	Kind           string `json:"kind"`
	ClientID       string `json:"client_id"`
	ConversationID string `json:"conversation_id"`
}

func validateLaunchRunSource(in *launchRunSource) (*store.RunSource, error) {
	if in == nil {
		return nil, nil
	}
	if strings.TrimSpace(in.Kind) != store.RunSourceKindStudioChat {
		return nil, fmt.Errorf("kind must be %q", store.RunSourceKindStudioChat)
	}
	if err := validateStudioChatSourceID("client_id", in.ClientID); err != nil {
		return nil, err
	}
	if err := validateStudioChatSourceID("conversation_id", in.ConversationID); err != nil {
		return nil, err
	}
	return &store.RunSource{
		Kind:           store.RunSourceKindStudioChat,
		ClientID:       in.ClientID,
		ConversationID: in.ConversationID,
	}, nil
}

func validateStudioChatSourceID(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > studioChatSourceIDMax || !studioChatSourceID.MatchString(value) {
		return fmt.Errorf("%s must be 1-%d safe characters", name, studioChatSourceIDMax)
	}
	return nil
}
