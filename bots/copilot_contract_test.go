package bots

import (
	"os"
	"strings"
	"testing"
)

func readCopilotContractFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func copilotContractSection(t *testing.T, source, start, end string) string {
	t.Helper()
	startAt := strings.Index(source, start)
	if startAt < 0 {
		t.Fatalf("could not find contract owner %q", start)
	}
	endAt := strings.Index(source[startAt+len(start):], end)
	if endAt < 0 {
		t.Fatalf("could not find end %q for contract owner %q", end, start)
	}
	return source[startAt : startAt+len(start)+endAt]
}

func normalizedCopilotContract(source string) string {
	return strings.Join(strings.Fields(source), " ")
}

func requireCopilotContract(t *testing.T, owner, source string, fragments ...string) {
	t.Helper()
	normalized := normalizedCopilotContract(source)
	for _, fragment := range fragments {
		if !strings.Contains(normalized, normalizedCopilotContract(fragment)) {
			t.Errorf("%s contract is missing %q", owner, fragment)
		}
	}
}

func TestCopilotProtocolLiteralsMatchTheirHostOwners(t *testing.T) {
	main := readCopilotContractFile(t, "copilot/main.bot")
	system := copilotContractSection(t, main, "prompt copi_system:", "prompt copi_user:")
	contextOwner := readCopilotContractFile(t, "../studio/src/lib/chatDock/contextMessage.ts")
	navigationOwner := readCopilotContractFile(t, "../studio/src/lib/chatDock/replyNavigation.ts")
	serverInfoOwner := readCopilotContractFile(t, "../pkg/server/server_info.go")
	watchOwner := readCopilotContractFile(t, "../pkg/server/assistant_run_watch.go")
	completedOwner := readCopilotContractFile(t, "../pkg/server/assistant_host_event.go")
	failedOwner := readCopilotContractFile(t, "../studio/src/components/ChatDock/AssistantActionOffer.tsx")

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
