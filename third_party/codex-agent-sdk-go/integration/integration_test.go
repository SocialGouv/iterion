//go:build integration

package integration

import (
	"errors"
	"strings"
	"testing"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// skipIfCLINotInstalled skips the test if err indicates the Codex CLI binary
// is not found. Call this immediately after receiving the first non-nil error
// from Query or Client.Start.
func skipIfCLINotInstalled(t *testing.T, err error) {
	t.Helper()

	if _, ok := errors.AsType[*codexsdk.CLINotFoundError](err); ok {
		t.Skip("Codex CLI not installed")
	}
}

// contains42 checks if a string mentions the number 42.
func contains42(s string) bool {
	lower := strings.ToLower(s)

	return strings.Contains(lower, "42") ||
		strings.Contains(lower, "forty-two") ||
		strings.Contains(lower, "forty two")
}

// mapKeys returns sorted keys of a map for diagnostic logging.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	return keys
}
