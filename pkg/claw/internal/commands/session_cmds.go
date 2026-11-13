package commands

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SessionManager is the interface that session-related commands require.
type SessionManager interface {
	ListSessions() ([]string, error)
	ForkSession(name string) (string, error)
	LoadSession(id string) error
	DeleteSession(id string) error
}

// RegisterSessionCommands registers session-related slash commands.
func RegisterSessionCommands(r *Registry) {
	r.Register(Command{
		Name:         "session",
		Description:  "List, switch, fork, or delete managed local sessions",
		ArgumentHint: "[list|switch <session-id>|fork [branch-name]|delete <session-id> [--force]]",
		Category:     CategorySession,
		Handler: func(args string, loop interface{}) error {
			parts := strings.Fields(args)
			sub := "list"
			if len(parts) > 0 {
				sub = strings.ToLower(parts[0])
			}

			sm, ok := loop.(SessionManager)
			if !ok {
				fmt.Println("Session management not available in this context.")
				return nil
			}

			switch sub {
			case "list", "":
				sessions, err := sm.ListSessions()
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

			case "switch":
				if len(parts) < 2 {
					fmt.Println("Usage: /session switch <session-id>")
					return nil
				}
				if err := sm.LoadSession(parts[1]); err != nil {
					return fmt.Errorf("switch session: %w", err)
				}
				fmt.Printf("Switched to session %s\n", parts[1])

			case "fork":
				name := ""
				if len(parts) > 1 {
					name = parts[1]
				}
				id, err := sm.ForkSession(name)
				if err != nil {
					return fmt.Errorf("fork session: %w", err)
				}
				fmt.Printf("Forked session: %s\n", id)

			case "delete":
				if len(parts) < 2 {
					fmt.Println("Usage: /session delete <session-id>")
					return nil
				}
				if err := sm.DeleteSession(parts[1]); err != nil {
					return fmt.Errorf("delete session: %w", err)
				}
				fmt.Printf("Deleted session %s\n", parts[1])

			default:
				fmt.Printf("Unknown session subcommand: %s\nUsage: /session [list|switch|fork|delete]\n", sub)
			}

			return nil
		},
	})

	r.Register(Command{
		Name:         "resume",
		Description:  "Load a saved session into the REPL",
		ArgumentHint: "<session-path>",
		Category:     CategorySession,
		Handler: func(args string, loop interface{}) error {
			if args == "" {
				fmt.Println("Usage: /resume <session-path>")
				return nil
			}
			sm, ok := loop.(SessionManager)
			if !ok {
				fmt.Println("Session management not available in this context.")
				return nil
			}
			if err := sm.LoadSession(strings.TrimSpace(args)); err != nil {
				return fmt.Errorf("resume session: %w", err)
			}
			fmt.Printf("Resumed session from %s\n", args)
			return nil
		},
	})

	r.Register(Command{
		Name:         "rename",
		Description:  "Rename the current session",
		ArgumentHint: "<name>",
		Category:     CategorySession,
		Handler: func(args string, loop interface{}) error {
			if args == "" {
				fmt.Println("Usage: /rename <name>")
				return nil
			}
			type sessionRenamer interface {
				RenameSession(name string) error
			}
			if sr, ok := loop.(sessionRenamer); ok {
				if err := sr.RenameSession(strings.TrimSpace(args)); err != nil {
					return fmt.Errorf("rename: %w", err)
				}
				fmt.Printf("Session renamed to %s\n", args)
			} else {
				fmt.Println("Session rename not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "history",
		Description:     "Show conversation history",
		ArgumentHint:    "[count]",
		ResumeSupported: true,
		Category:        CategorySession,
		Handler: func(args string, loop interface{}) error {
			type historyViewer interface {
				ShowHistory(count int) ([]string, error)
			}
			hv, ok := loop.(historyViewer)
			if !ok {
				fmt.Println("History not available in this context.")
				return nil
			}
			count := 10
			if n := strings.TrimSpace(args); n != "" {
				if parsed, err := strconv.Atoi(n); err == nil && parsed > 0 {
					count = parsed
				}
			}
			entries, err := hv.ShowHistory(count)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Println("No history entries.")
				return nil
			}
			for _, e := range entries {
				fmt.Println(e)
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "workspace",
		Aliases:         []string{"ws"},
		Description:     "Show workspace information",
		ResumeSupported: true,
		Category:        CategorySession,
		Handler: func(args string, loop interface{}) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("workspace: %w", err)
			}
			fmt.Printf("Workspace: %s\n", cwd)
			// Show git info if available
			if gitAvailable() {
				branch, err := runGit("rev-parse", "--abbrev-ref", "HEAD")
				if err == nil {
					fmt.Printf("Branch:    %s\n", branch)
				}
				status, err := runGit("status", "--porcelain")
				if err == nil {
					changes := len(strings.Split(strings.TrimSpace(status), "\n"))
					if status == "" {
						changes = 0
					}
					fmt.Printf("Changes:   %d file(s)\n", changes)
				}
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "focus",
		Description:  "Focus context on specific files/directories",
		ArgumentHint: "<path>",
		Category:     CategorySession,
		Handler: func(args string, loop interface{}) error {
			type focusManager interface {
				AddFocus(path string) error
			}
			fm, ok := loop.(focusManager)
			if !ok {
				fmt.Println("Focus not available in this context.")
				return nil
			}
			path := strings.TrimSpace(args)
			if path == "" {
				fmt.Println("Usage: /focus <path>")
				return nil
			}
			if err := fm.AddFocus(path); err != nil {
				return err
			}
			fmt.Printf("Focused on: %s\n", path)
			return nil
		},
	})

	r.Register(Command{
		Name:         "unfocus",
		Description:  "Remove focus from a file/directory",
		ArgumentHint: "[path]",
		Category:     CategorySession,
		Handler: func(args string, loop interface{}) error {
			type focusManager interface {
				RemoveFocus(path string) error
			}
			fm, ok := loop.(focusManager)
			if !ok {
				fmt.Println("Unfocus not available in this context.")
				return nil
			}
			path := strings.TrimSpace(args)
			if err := fm.RemoveFocus(path); err != nil {
				return err
			}
			if path == "" {
				fmt.Println("All focus cleared.")
			} else {
				fmt.Printf("Unfocused: %s\n", path)
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "tag",
		Description:     "Tag the current conversation point",
		ArgumentHint:    "[label]",
		ResumeSupported: true,
		Category:        CategorySession,
		Handler: func(args string, loop interface{}) error {
			type tagger interface {
				TagConversation(label string) error
			}
			if t, ok := loop.(tagger); ok {
				label := strings.TrimSpace(args)
				if label == "" {
					fmt.Println("Usage: /tag [label]")
					return nil
				}
				if err := t.TagConversation(label); err != nil {
					return fmt.Errorf("tag: %w", err)
				}
				fmt.Printf("Tagged conversation point: %s\n", label)
			} else {
				fmt.Println("Tagging not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "summary",
		Description:     "Generate a summary of the conversation",
		ResumeSupported: true,
		Category:        CategorySession,
		Handler: func(args string, loop interface{}) error {
			type summarizer interface {
				SummarizeConversation() (string, error)
			}
			if s, ok := loop.(summarizer); ok {
				summary, err := s.SummarizeConversation()
				if err != nil {
					return fmt.Errorf("summary: %w", err)
				}
				fmt.Println(summary)
			} else {
				fmt.Println("Summary generation not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "pin",
		Description:  "Pin a message to persist across compaction",
		ArgumentHint: "[message-index]",
		Category:     CategorySession,
		Handler: func(args string, loop interface{}) error {
			type pinner interface {
				PinMessage(index int) error
			}
			if p, ok := loop.(pinner); ok {
				idx := strings.TrimSpace(args)
				if idx == "" {
					fmt.Println("Usage: /pin [message-index]")
					return nil
				}
				n, err := strconv.Atoi(idx)
				if err != nil || n < 0 {
					return fmt.Errorf("pin: invalid message index %q", idx)
				}
				if err := p.PinMessage(n); err != nil {
					return fmt.Errorf("pin: %w", err)
				}
				fmt.Printf("Pinned message %d.\n", n)
			} else {
				fmt.Println("Message pinning not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "unpin",
		Description:  "Unpin a previously pinned message",
		ArgumentHint: "[message-index]",
		Category:     CategorySession,
		Handler: func(args string, loop interface{}) error {
			type unpinner interface {
				UnpinMessage(index int) error
			}
			if u, ok := loop.(unpinner); ok {
				idx := strings.TrimSpace(args)
				if idx == "" {
					fmt.Println("Usage: /unpin [message-index]")
					return nil
				}
				n, err := strconv.Atoi(idx)
				if err != nil || n < 0 {
					return fmt.Errorf("unpin: invalid message index %q", idx)
				}
				if err := u.UnpinMessage(n); err != nil {
					return fmt.Errorf("unpin: %w", err)
				}
				fmt.Printf("Unpinned message %d.\n", n)
			} else {
				fmt.Println("Message unpinning not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "bookmarks",
		Description:     "List or manage conversation bookmarks",
		ArgumentHint:    "[add|remove|list]",
		ResumeSupported: true,
		Category:        CategorySession,
		Handler: func(args string, loop interface{}) error {
			type bookmarkManager interface {
				ListBookmarks() ([]string, error)
				AddBookmark(label string) error
				RemoveBookmark(label string) error
			}
			bm, ok := loop.(bookmarkManager)
			if !ok {
				fmt.Println("Bookmarks not available in this context.")
				return nil
			}
			parts := strings.Fields(args)
			sub := "list"
			if len(parts) > 0 {
				sub = strings.ToLower(parts[0])
			}
			switch sub {
			case "list", "":
				bookmarks, err := bm.ListBookmarks()
				if err != nil {
					return fmt.Errorf("bookmarks: %w", err)
				}
				if len(bookmarks) == 0 {
					fmt.Println("No bookmarks.")
					return nil
				}
				fmt.Println("Bookmarks:")
				for _, b := range bookmarks {
					fmt.Printf("  %s\n", b)
				}
			case "add":
				if len(parts) < 2 {
					fmt.Println("Usage: /bookmarks add <label>")
					return nil
				}
				if err := bm.AddBookmark(parts[1]); err != nil {
					return fmt.Errorf("bookmarks add: %w", err)
				}
				fmt.Printf("Added bookmark: %s\n", parts[1])
			case "remove":
				if len(parts) < 2 {
					fmt.Println("Usage: /bookmarks remove <label>")
					return nil
				}
				if err := bm.RemoveBookmark(parts[1]); err != nil {
					return fmt.Errorf("bookmarks remove: %w", err)
				}
				fmt.Printf("Removed bookmark: %s\n", parts[1])
			default:
				fmt.Println("Usage: /bookmarks [add|remove|list]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "add-dir",
		Description:  "Add an additional directory to the context",
		ArgumentHint: "<path>",
		Category:     CategorySession,
		Handler: func(args string, loop interface{}) error {
			type dirAdder interface {
				AddDirectory(path string) error
			}
			path := strings.TrimSpace(args)
			if path == "" {
				fmt.Println("Usage: /add-dir <path>")
				return nil
			}
			if da, ok := loop.(dirAdder); ok {
				if err := da.AddDirectory(path); err != nil {
					return fmt.Errorf("add-dir: %w", err)
				}
				fmt.Printf("Added directory to context: %s\n", path)
			} else {
				fmt.Println("Directory adding not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "export",
		Description:     "Export the current conversation to a file",
		ArgumentHint:    "[file]",
		ResumeSupported: true,
		Category:        CategorySession,
		Handler: func(args string, loop interface{}) error {
			type exporter interface {
				ExportConversation(path string) error
			}
			if e, ok := loop.(exporter); ok {
				path := strings.TrimSpace(args)
				if path == "" {
					path = "conversation.json"
				}
				if err := e.ExportConversation(path); err != nil {
					return fmt.Errorf("export: %w", err)
				}
				fmt.Printf("Exported conversation to %s\n", path)
			} else {
				fmt.Println("Export not available in this context.")
			}
			return nil
		},
	})
}
