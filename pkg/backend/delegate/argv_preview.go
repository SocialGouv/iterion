package delegate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// argvValueRedactedFlags are the CLI flags whose VALUE is a document rather
// than a setting, and whose document carries credentials. Today one:
// `--mcp-config` is the whole MCP catalog inline as JSON, including each
// stdio server's `env` and each http server's `headers` — a plugin's
// resolved API key among them.
var argvValueRedactedFlags = map[string]bool{
	"--mcp-config": true,
}

// redactedArgvPreview renders a resolved CLI invocation for the log with the
// credential-bearing values replaced by a description of their shape.
//
// The invocation is worth logging: a silent `claude` exit is otherwise opaque
// even with stderr capture, and the concrete `docker exec` line is what makes
// it traceable. The raw argv is not worth logging, because on a cloud run
// this logger is teed into the run's persisted log, which the run's own
// artifacts and the studio then serve — so an operator-installed plugin's
// credentials would outlive the process that used them.
//
// What replaces a redacted value still answers the question the log is there
// for: how many MCP servers were passed, and under which names.
func redactedArgvPreview(path string, args []string) []string {
	preview := make([]string, 0, len(args)+1)
	preview = append(preview, path)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		// `--flag=value` and `--flag value` are both accepted by the CLIs;
		// redacting only one shape leaves the other printing the document.
		if flag, value, ok := strings.Cut(arg, "="); ok && argvValueRedactedFlags[flag] {
			preview = append(preview, flag+"="+describeRedactedArgvValue(flag, value))
			continue
		}
		if argvValueRedactedFlags[arg] && i+1 < len(args) {
			preview = append(preview, arg, describeRedactedArgvValue(arg, args[i+1]))
			i++
			continue
		}
		preview = append(preview, arg)
	}
	return preview
}

// describeRedactedArgvValue summarises a redacted value: for an MCP config,
// the server names it declares — data the operator needs and no secret.
func describeRedactedArgvValue(flag, value string) string {
	if flag != "--mcp-config" {
		return "<redacted>"
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(value), &doc); err != nil {
		return "<redacted mcp config>"
	}
	names := make([]string, 0, len(doc.MCPServers))
	for name := range doc.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("<redacted mcp config: %d server(s) %v>", len(names), names)
}
