package commands

import (
	"fmt"
	"strings"
)

// CommandCategory groups related commands for help display and organization.
type CommandCategory string

const (
	CategorySession       CommandCategory = "session"
	CategoryStatus        CommandCategory = "status"
	CategoryConfig        CommandCategory = "config"
	CategoryDiagnostics   CommandCategory = "diagnostics"
	CategoryBuiltin       CommandCategory = "builtin"
	CategoryPlugin        CommandCategory = "plugin"
	CategoryCode          CommandCategory = "code"
	CategoryUX            CommandCategory = "ux"
	CategoryContext       CommandCategory = "context"
	CategoryAuth          CommandCategory = "auth"
	CategoryInteraction   CommandCategory = "interaction"
	CategoryUncategorized CommandCategory = ""
)

// Command represents a slash command.
type Command struct {
	Name            string
	Aliases         []string
	Description     string
	ArgumentHint    string
	ResumeSupported bool
	Category        CommandCategory
	Handler         func(args string, loop interface{}) error
}

// Registry holds all registered slash commands.
type Registry struct {
	commands map[string]Command
}

// NewRegistry creates a new command registry with built-in commands.
func NewRegistry() *Registry {
	r := &Registry{
		commands: make(map[string]Command),
	}

	r.registerBuiltins()
	return r
}

// Register adds a command to the registry, including any aliases.
func (r *Registry) Register(cmd Command) {
	name := strings.TrimPrefix(cmd.Name, "/")
	r.commands[name] = cmd
	for _, alias := range cmd.Aliases {
		a := strings.TrimPrefix(alias, "/")
		r.commands[a] = cmd
	}
}

// Execute processes an input line. Returns (true, nil) if the command was handled,
// (false, nil) if not a command, or (true, err) if a command errored.
func (r *Registry) Execute(input string, loop interface{}) (bool, error) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return false, nil
	}

	// Split into command name and args
	parts := strings.SplitN(input[1:], " ", 2)
	name := strings.ToLower(parts[0])
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	cmd, ok := r.commands[name]
	if !ok {
		fmt.Printf("Unknown command: /%s. Type /help for available commands.\n", name)
		return true, nil
	}

	return true, cmd.Handler(args, loop)
}

// List returns all registered commands, deduplicated (aliases excluded).
func (r *Registry) List() []Command {
	seen := make(map[string]bool)
	cmds := make([]Command, 0, len(r.commands))
	for name, cmd := range r.commands {
		primary := strings.TrimPrefix(cmd.Name, "/")
		if name != primary {
			continue // skip alias entries
		}
		if seen[primary] {
			continue
		}
		seen[primary] = true
		cmds = append(cmds, cmd)
	}
	return cmds
}

// Lookup finds a command by name (including aliases). Returns the command and
// whether it was found.
func (r *Registry) Lookup(name string) (Command, bool) {
	name = strings.TrimPrefix(name, "/")
	cmd, ok := r.commands[strings.ToLower(name)]
	return cmd, ok
}

// Count returns the number of unique (non-alias) commands registered.
func (r *Registry) Count() int {
	return len(r.List())
}

// SuggestCommands returns up to limit command names that have prefix as a prefix.
// This powers tab-completion in the TUI.
func (r *Registry) SuggestCommands(prefix string, limit int) []string {
	prefix = strings.TrimPrefix(strings.ToLower(prefix), "/")
	seen := make(map[string]bool)
	var suggestions []string
	for name := range r.commands {
		if seen[name] || !strings.HasPrefix(name, prefix) {
			continue
		}
		seen[name] = true
		suggestions = append(suggestions, "/"+name)
		if len(suggestions) >= limit {
			break
		}
	}
	return suggestions
}

// CommandsByCategory returns all unique commands in the given category.
func (r *Registry) CommandsByCategory(category CommandCategory) []Command {
	var result []Command
	seen := make(map[string]bool)
	for name, cmd := range r.commands {
		primary := strings.TrimPrefix(cmd.Name, "/")
		if name != primary || seen[primary] || cmd.Category != category {
			continue
		}
		seen[primary] = true
		result = append(result, cmd)
	}
	return result
}

// Categories returns the set of categories that have registered commands.
func (r *Registry) Categories() []CommandCategory {
	cats := make(map[CommandCategory]bool)
	for _, cmd := range r.commands {
		if cmd.Category != CategoryUncategorized {
			cats[cmd.Category] = true
		}
	}
	result := make([]CommandCategory, 0, len(cats))
	for c := range cats {
		result = append(result, c)
	}
	return result
}

// ResumeSupportedCommands returns commands where ResumeSupported is true.
func (r *Registry) ResumeSupportedCommands() []Command {
	var result []Command
	seen := make(map[string]bool)
	for name, cmd := range r.commands {
		primary := strings.TrimPrefix(cmd.Name, "/")
		if name != primary || seen[primary] || !cmd.ResumeSupported {
			continue
		}
		seen[primary] = true
		result = append(result, cmd)
	}
	return result
}

// NewFullRegistry creates a registry with all command categories registered.
// This is the standard way to create a fully-initialized command registry.
func NewFullRegistry() *Registry {
	r := NewRegistry()
	RegisterSessionCommands(r)
	RegisterSessionTimelineCommands(r)
	RegisterStatusCommands(r)
	RegisterConfigCommands(r)
	RegisterDiagnosticCommands(r)
	RegisterAuthCommands(r)
	RegisterMCPCommand(r)
	RegisterPluginCommands(r)
	RegisterPluginMarketplaceCommands(r)
	RegisterCodeCommands(r)
	RegisterUXCommands(r)
	RegisterContextCommands(r)
	RegisterGitCommands(r)
	RegisterInteractionCommands(r)
	return r
}

// InitializeAll registers all command categories into an existing registry.
func InitializeAll(r *Registry) {
	RegisterSessionCommands(r)
	RegisterSessionTimelineCommands(r)
	RegisterStatusCommands(r)
	RegisterConfigCommands(r)
	RegisterDiagnosticCommands(r)
	RegisterAuthCommands(r)
	RegisterMCPCommand(r)
	RegisterPluginCommands(r)
	RegisterPluginMarketplaceCommands(r)
	RegisterCodeCommands(r)
	RegisterUXCommands(r)
	RegisterContextCommands(r)
	RegisterGitCommands(r)
	RegisterInteractionCommands(r)
}

// ErrExit is returned by /exit and /quit to signal the REPL should stop.
var ErrExit = fmt.Errorf("exit")

// registerBuiltins registers the built-in slash commands.
func (r *Registry) registerBuiltins() {
	r.Register(Command{
		Name:            "help",
		Description:     "Show available commands",
		ArgumentHint:    "[command]",
		ResumeSupported: true,
		Category:        CategoryBuiltin,
		Handler: func(args string, loop interface{}) error {
			if query := strings.TrimSpace(args); query != "" {
				// Show detail for a specific command.
				query = strings.TrimPrefix(query, "/")
				cmd, ok := r.Lookup(query)
				if !ok {
					fmt.Printf("Unknown command: /%s\n", query)
					return nil
				}
				name := strings.TrimPrefix(cmd.Name, "/")
				fmt.Printf("/%s — %s\n", name, cmd.Description)
				if cmd.ArgumentHint != "" {
					fmt.Printf("  Usage: /%s %s\n", name, cmd.ArgumentHint)
				}
				if len(cmd.Aliases) > 0 {
					fmt.Printf("  Aliases: %s\n", strings.Join(cmd.Aliases, ", "))
				}
				return nil
			}

			fmt.Println("Available commands:")
			// Deduplicate aliases — only show primary names.
			seen := make(map[string]bool)
			for name, cmd := range r.commands {
				if name != strings.TrimPrefix(cmd.Name, "/") {
					continue // skip alias entries
				}
				if seen[name] {
					continue
				}
				seen[name] = true
				hint := ""
				if cmd.ArgumentHint != "" {
					hint = " " + cmd.ArgumentHint
				}
				aliases := ""
				if len(cmd.Aliases) > 0 {
					aliases = fmt.Sprintf(" (aliases: %s)", strings.Join(cmd.Aliases, ", "))
				}
				fmt.Printf("  /%s%s — %s%s\n", name, hint, cmd.Description, aliases)
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "exit",
		Description: "Exit the REPL",
		Category:    CategoryBuiltin,
		Handler: func(args string, loop interface{}) error {
			return ErrExit
		},
	})

	r.Register(Command{
		Name:        "quit",
		Description: "Exit the REPL",
		Category:    CategoryBuiltin,
		Handler: func(args string, loop interface{}) error {
			return ErrExit
		},
	})

	r.Register(Command{
		Name:        "clear",
		Description: "Clear the conversation history",
		Category:    CategorySession,
		Handler: func(args string, loop interface{}) error {
			// Type assertion to access the conversation loop
			type sessionHolder interface {
				ClearSession()
			}
			if sh, ok := loop.(sessionHolder); ok {
				sh.ClearSession()
				fmt.Println("Conversation history cleared.")
			} else {
				fmt.Println("Cannot clear session: incompatible loop type.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "session-list",
		Description: "List saved sessions",
		Category:    CategorySession,
		Handler: func(args string, loop interface{}) error {
			type sessionLister interface {
				ListSessions() ([]string, error)
			}
			if sl, ok := loop.(sessionLister); ok {
				sessions, err := sl.ListSessions()
				if err != nil {
					return fmt.Errorf("list sessions: %w", err)
				}
				if len(sessions) == 0 {
					fmt.Println("No saved sessions.")
					return nil
				}
				fmt.Println("Saved sessions:")
				for _, s := range sessions {
					fmt.Printf("  %s\n", s)
				}
			} else {
				fmt.Println("Cannot list sessions: incompatible loop type.")
			}
			return nil
		},
	})
}
