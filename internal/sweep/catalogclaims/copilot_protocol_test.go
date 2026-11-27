package catalogclaims

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

func TestCopilotProtocolLiteralsMatchTheirHostOwners(t *testing.T) {
	root := sweepwalk.ModuleRoot(t)
	at := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
	main := readCopilotContractFile(t, at("bots/copilot/main.bot"))
	system := copilotContractSection(t, main, "prompt copi_system:", "prompt copi_user:")
	contextOwner := readCopilotContractFile(t, at("studio/src/lib/chatDock/contextMessage.ts"))
	navigationOwner := readCopilotContractFile(t, at("studio/src/lib/chatDock/replyNavigation.ts"))
	serverInfoOwner := readCopilotContractFile(t, at("pkg/server/server_info.go"))
	watchOwner := readCopilotContractFile(t, at("pkg/server/assistant_run_watch.go"))
	completedOwner := readCopilotContractFile(t, at("pkg/server/assistant_host_event.go"))
	failedOwner := readCopilotContractFile(t, at("studio/src/components/ChatDock/AssistantActionOffer.tsx"))

	for _, contract := range []struct {
		literal string
		owner   string
	}{
		{"Opened the editor — go ahead.", navigationOwner},
		{"<visible-page-context>", contextOwner},
		{"<resolved-assistant-context>", contextOwner},
		{"<active-editor-document>", contextOwner},
		{"ITERION_ASSISTANT_EDITOR_MAX_SOURCE", serverInfoOwner},
		{"assistant-watch-event", watchOwner},
		{"action-completed", completedOwner},
		{"action-failed", failedOwner},
	} {
		if !strings.Contains(contract.owner, contract.literal) {
			t.Fatalf("authoritative host source lost protocol literal %q", contract.literal)
		}
		if !strings.Contains(system, contract.literal) {
			t.Errorf("Copi system contract drifted from host literal %q", contract.literal)
		}
	}
}
