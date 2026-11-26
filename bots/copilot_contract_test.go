package bots

import (
	"strings"
	"testing"
)

func readCopilotContractFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := botUnitSource(path)
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
