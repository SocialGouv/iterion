//go:build integration

package protocol

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/stretchr/testify/require"
)

func generateSchemaDir(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("Codex CLI not installed")
	}

	dir := t.TempDir()
	cmd := exec.Command("codex", "app-server", "generate-json-schema", "--out", dir)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "generate schema: %s", string(output))

	return dir
}

func readSchemaFile(t *testing.T, dir string, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)

	return string(data)
}

func requestMethodToSubtype(method string) string {
	parts := strings.SplitN(method, "/", 2)
	if len(parts) == 2 {
		return parts[0] + "_" + parts[1]
	}

	return method
}

func TestSessionRegisterHandlers_CoversCurrentCodexServerRequests(t *testing.T) {
	schemaDir := generateSchemaDir(t)
	serverRequestJSON := readSchemaFile(t, schemaDir, "ServerRequest.json")

	controller := NewController(slog.Default(), newMockTransport())
	session := NewSession(slog.Default(), controller, &config.Options{}, nil)
	session.RegisterHandlers()

	liveMethods := []string{
		"item/tool/call",
		"item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/tool/requestUserInput",
		"account/chatgptAuthTokens/refresh",
		"applyPatchApproval",
		"execCommandApproval",
		"mcpServer/elicitation/request",
		"item/permissions/requestApproval",
	}

	// Guard against accidental whitespace-only schema reads.
	require.NotEmpty(t, strings.TrimSpace(serverRequestJSON))

	for _, method := range liveMethods {
		require.Contains(t, serverRequestJSON, `"`+method+`"`,
			"installed codex schema no longer contains %s; update this proof test", method)

		subtype := requestMethodToSubtype(method)
		_, ok := controller.handlers[subtype]
		require.True(t, ok, "no session handler registered for current codex server request %q (subtype %q)", method, subtype)
	}
}
