package commands

import (
	"fmt"
	"sort"
	"strings"
)

// CommandDeps holds optional dependencies that command handlers can use.
// Fields are typed as interface{} so the commands package does not import
// concrete implementation packages; callers populate whichever fields they
// have available.
type CommandDeps struct {
	PluginManager  interface{} // *plugin.PluginManager when available
	MCPRegistry    interface{} // *mcp.Registry when available
	WorkerRegistry interface{} // *worker.WorkerRegistry when available
	Config         interface{} // config provider when available
	Session        interface{} // session provider when available
	GitRunner      interface{} // git command runner when available
}

// validateNoArgs returns an error if args is non-empty.
func validateNoArgs(cmdName, args string) error {
	if strings.TrimSpace(args) != "" {
		return fmt.Errorf("/%s takes no arguments", cmdName)
	}
	return nil
}

// requireArg returns the trimmed args string or an error when it is empty.
// hint describes what the command expects (shown in the error message).
func requireArg(cmdName, args, hint string) (string, error) {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return "", fmt.Errorf("/%s requires an argument: %s", cmdName, hint)
	}
	return trimmed, nil
}

// splitSubcommand splits "sub rest of args" into ("sub", "rest of args").
// If args is empty or blank, sub is "" and rest is "".
func splitSubcommand(args string) (sub string, rest string) {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return "", ""
	}
	parts := strings.SplitN(trimmed, " ", 2)
	sub = parts[0]
	if len(parts) > 1 {
		rest = parts[1]
	}
	return sub, rest
}

// AllCategories returns a sorted list of every CommandCategory constant
// (excluding CategoryUncategorized).
func AllCategories() []CommandCategory {
	cats := []CommandCategory{
		CategorySession,
		CategoryStatus,
		CategoryConfig,
		CategoryDiagnostics,
		CategoryBuiltin,
		CategoryPlugin,
		CategoryCode,
		CategoryUX,
		CategoryContext,
		CategoryAuth,
		CategoryInteraction,
	}
	sort.Slice(cats, func(i, j int) bool {
		return string(cats[i]) < string(cats[j])
	})
	return cats
}
