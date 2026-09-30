package delegate

import (
	"strings"
	"testing"
)

// The resolved CLI invocation is logged because a silent `claude` exit is
// otherwise opaque — and on a cloud run that logger is teed into the run's
// PERSISTED log. `--mcp-config` carries the whole MCP catalog inline, each
// stdio server's `env` and each http server's `headers` included, and a
// plugin's config marked `secret` is resolved to its real value on the way
// there. Logging the raw argv therefore writes an operator credential into an
// artifact the run itself serves.
func TestTheArgvPreviewKeepsTheMCPConfigOutOfTheLog(t *testing.T) {
	const credential = "sk-operator-credential-canary"
	config := `{"mcpServers":{"firecrawl":{"command":"npx","env":{"FIRECRAWL_API_KEY":"` + credential +
		`"}},"github":{"url":"https://api.example","headers":{"Authorization":"Bearer ` + credential + `"}}}}`

	for _, args := range [][]string{
		{"--print", "--mcp-config", config, "--model", "opus"},
		// Both shapes reach these CLIs; redacting one and not the other
		// leaves the document printed by whichever the SDK happens to emit.
		{"--print", "--mcp-config=" + config, "--model", "opus"},
	} {
		got := strings.Join(redactedArgvPreview("/usr/bin/claude", args), " ")
		if strings.Contains(got, credential) {
			t.Fatalf("the credential reached the log line")
		}
		// What the log is FOR has to survive: which binary, which flags, and
		// which MCP servers were passed.
		for _, want := range []string{"/usr/bin/claude", "--model", "opus", "firecrawl", "github", "2 server(s)"} {
			if !strings.Contains(got, want) {
				t.Errorf("the preview lost %q, which is what the line is read for: %s", want, got)
			}
		}
	}
}

// A value that is not valid JSON is still redacted — an unparseable document
// is the case where guessing would be worst.
func TestAnUnreadableMCPConfigIsStillRedacted(t *testing.T) {
	got := strings.Join(redactedArgvPreview("claude", []string{"--mcp-config", "{not json: " + "secret-canary"}), " ")
	if strings.Contains(got, "secret-canary") {
		t.Error("an unparseable config must be redacted, not printed on the assumption it holds nothing")
	}
}

// Everything else is left exactly as it was: the preview is a redaction, not
// a rewrite, and an operator comparing it with a hand-run command must find
// the same flags.
func TestTheArgvPreviewLeavesOtherArgumentsAlone(t *testing.T) {
	args := []string{"--print", "--model", "opus", "--strict-mcp-config", "--permission-mode", "plan"}
	got := redactedArgvPreview("claude", args)
	if len(got) != len(args)+1 {
		t.Fatalf("the preview changed the argument count: %v", got)
	}
	for i, arg := range args {
		if got[i+1] != arg {
			t.Errorf("argument %d rewritten: %q → %q", i, arg, got[i+1])
		}
	}
}
