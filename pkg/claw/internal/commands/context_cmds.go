package commands

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// contextLoop is the interface for commands that inspect conversation context.
type contextLoop interface {
	ListContextFiles() []string
	ContextTokenBreakdown() map[string]int
	ClearContext()
}

// searchLoop is the interface for workspace search commands.
type searchLoop interface {
	SearchFiles(query string) ([]string, error)
}

// rewindLoop is the interface for conversation rewind commands.
type rewindLoop interface {
	RewindSteps(n int) error
}

// clipboardLoop is the interface for clipboard copy commands.
type clipboardLoop interface {
	GetLastOutput() string
	GetFullConversation() string
}

// clipboardCmd returns the platform-specific clipboard command and arguments.
func clipboardCmd() (string, []string, error) {
	switch runtime.GOOS {
	case "darwin":
		return "pbcopy", nil, nil
	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			return "xclip", []string{"-selection", "clipboard"}, nil
		}
		if _, err := exec.LookPath("xsel"); err == nil {
			return "xsel", []string{"--clipboard", "--input"}, nil
		}
		return "", nil, fmt.Errorf("clipboard: install xclip or xsel")
	case "windows":
		return "clip.exe", nil, nil
	default:
		return "", nil, fmt.Errorf("clipboard: unsupported platform %s", runtime.GOOS)
	}
}

// isValidBranchName validates a git branch name for safety.
func isValidBranchName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '/' || r == '.') {
			return false
		}
	}
	return !strings.Contains(name, "..") && !strings.HasSuffix(name, ".lock")
}

// RegisterContextCommands registers context and workspace slash commands.
func RegisterContextCommands(r *Registry) {
	r.Register(Command{
		Name:            "files",
		Description:     "List files in context",
		ResumeSupported: true,
		Category:        CategoryContext,
		Handler: func(args string, loop interface{}) error {
			cl, ok := loop.(contextLoop)
			if !ok {
				fmt.Println("Context listing not available in this context.")
				return nil
			}
			files := cl.ListContextFiles()
			if len(files) == 0 {
				fmt.Println("No files in context.")
				return nil
			}
			fmt.Println("Files in context:")
			for _, f := range files {
				fmt.Printf("  %s\n", f)
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "context",
		Description:     "Show/clear token breakdown",
		ArgumentHint:    "[show|clear]",
		ResumeSupported: true,
		Category:        CategoryContext,
		Handler: func(args string, loop interface{}) error {
			cl, ok := loop.(contextLoop)
			if !ok {
				fmt.Println("Context management not available in this context.")
				return nil
			}
			sub := strings.TrimSpace(strings.ToLower(args))
			switch sub {
			case "clear":
				cl.ClearContext()
				fmt.Println("Context cleared.")
			case "show", "":
				breakdown := cl.ContextTokenBreakdown()
				if len(breakdown) == 0 {
					fmt.Println("Context is empty.")
					return nil
				}
				total := 0
				fmt.Println("Token breakdown:")
				for source, count := range breakdown {
					fmt.Printf("  %-20s %d tokens\n", source, count)
					total += count
				}
				fmt.Printf("  %-20s %d tokens\n", "Total", total)
			default:
				fmt.Println("Usage: /context [show|clear]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "hooks",
		Description:     "List/run hooks",
		ArgumentHint:    "[list|run]",
		ResumeSupported: true,
		Category:        CategoryContext,
		Handler: func(args string, loop interface{}) error {
			sub := strings.TrimSpace(strings.ToLower(args))
			switch sub {
			case "list", "":
				fmt.Println("Hooks: (none configured)")
				fmt.Println("Configure hooks in settings.json.")
			case "run":
				fmt.Println("No hooks to run.")
			default:
				fmt.Println("Usage: /hooks [list|run]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "search",
		Description:  "Search workspace",
		ArgumentHint: "<query>",
		Category:     CategoryContext,
		Handler: func(args string, loop interface{}) error {
			query := strings.TrimSpace(args)
			if query == "" {
				fmt.Println("Usage: /search <query>")
				return nil
			}
			sl, ok := loop.(searchLoop)
			if !ok {
				fmt.Println("Search not available in this context.")
				return nil
			}
			results, err := sl.SearchFiles(query)
			if err != nil {
				return fmt.Errorf("search: %w", err)
			}
			if len(results) == 0 {
				fmt.Println("No results found.")
				return nil
			}
			fmt.Printf("Found %d result(s):\n", len(results))
			for _, r := range results {
				fmt.Printf("  %s\n", r)
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "copy",
		Description:  "Copy to clipboard",
		ArgumentHint: "[last|all]",
		Category:     CategoryContext,
		Handler: func(args string, loop interface{}) error {
			cl, ok := loop.(clipboardLoop)
			if !ok {
				fmt.Println("Clipboard not available in this context.")
				return nil
			}

			what := strings.TrimSpace(strings.ToLower(args))
			if what == "" {
				what = "last"
			}

			var content string
			switch what {
			case "last":
				content = cl.GetLastOutput()
			case "all":
				content = cl.GetFullConversation()
			default:
				fmt.Println("Usage: /copy [last|all]")
				return nil
			}

			if content == "" {
				fmt.Println("Nothing to copy.")
				return nil
			}

			bin, binArgs, err := clipboardCmd()
			if err != nil {
				return err
			}

			cmd := exec.Command(bin, binArgs...)
			cmd.Stdin = strings.NewReader(content)
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("copy: %w", err)
			}
			fmt.Println("Copied to clipboard.")
			return nil
		},
	})

	r.Register(Command{
		Name:         "rewind",
		Description:  "Rewind conversation",
		ArgumentHint: "[steps]",
		Category:     CategoryContext,
		Handler: func(args string, loop interface{}) error {
			rl, ok := loop.(rewindLoop)
			if !ok {
				fmt.Println("Rewind not available in this context.")
				return nil
			}
			n := 1
			if s := strings.TrimSpace(args); s != "" {
				parsed, err := strconv.Atoi(s)
				if err != nil || parsed < 1 {
					return fmt.Errorf("rewind: invalid step count %q", s)
				}
				n = parsed
			}
			if err := rl.RewindSteps(n); err != nil {
				return fmt.Errorf("rewind: %w", err)
			}
			fmt.Printf("Rewound %d step(s).\n", n)
			return nil
		},
	})

	r.Register(Command{
		Name:            "tool-details",
		Description:     "Show detailed info about a specific tool",
		ArgumentHint:    "<tool-name>",
		ResumeSupported: true,
		Category:        CategoryContext,
		Handler: func(args string, loop interface{}) error {
			name := strings.TrimSpace(args)
			if name == "" {
				fmt.Println("Usage: /tool-details <tool-name>")
				return nil
			}
			type toolDetailer interface {
				GetToolDetails(name string) (string, error)
			}
			if td, ok := loop.(toolDetailer); ok {
				details, err := td.GetToolDetails(name)
				if err != nil {
					return fmt.Errorf("tool-details: %w", err)
				}
				fmt.Println(details)
			} else {
				fmt.Println("Tool details not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "map",
		Description:     "Show a visual map of the codebase structure",
		ArgumentHint:    "[depth]",
		ResumeSupported: true,
		Category:        CategoryContext,
		Handler: func(args string, loop interface{}) error {
			type codeMapper interface {
				ShowCodeMap(depth int) (string, error)
			}
			depth := 3
			if s := strings.TrimSpace(args); s != "" {
				parsed, err := strconv.Atoi(s)
				if err != nil || parsed < 1 {
					return fmt.Errorf("map: invalid depth %q", s)
				}
				depth = parsed
			}
			if cm, ok := loop.(codeMapper); ok {
				result, err := cm.ShowCodeMap(depth)
				if err != nil {
					return fmt.Errorf("map: %w", err)
				}
				fmt.Println(result)
			} else {
				fmt.Println("Code map not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "symbols",
		Description:     "List symbols in a file",
		ArgumentHint:    "<path>",
		ResumeSupported: true,
		Category:        CategoryContext,
		Handler: func(args string, loop interface{}) error {
			path := strings.TrimSpace(args)
			if path == "" {
				fmt.Println("Usage: /symbols <path>")
				return nil
			}
			type symbolLister interface {
				ListSymbols(path string) ([]string, error)
			}
			if sl, ok := loop.(symbolLister); ok {
				symbols, err := sl.ListSymbols(path)
				if err != nil {
					return fmt.Errorf("symbols: %w", err)
				}
				if len(symbols) == 0 {
					fmt.Println("No symbols found.")
					return nil
				}
				fmt.Println("Symbols:")
				for _, s := range symbols {
					fmt.Printf("  %s\n", s)
				}
			} else {
				fmt.Println("Symbol listing not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "references",
		Description:  "Find all references to a symbol",
		ArgumentHint: "<symbol>",
		Category:     CategoryContext,
		Handler: func(args string, loop interface{}) error {
			symbol := strings.TrimSpace(args)
			if symbol == "" {
				fmt.Println("Usage: /references <symbol>")
				return nil
			}
			type referenceFinder interface {
				FindReferences(symbol string) ([]string, error)
			}
			if rf, ok := loop.(referenceFinder); ok {
				refs, err := rf.FindReferences(symbol)
				if err != nil {
					return fmt.Errorf("references: %w", err)
				}
				if len(refs) == 0 {
					fmt.Printf("No references found for %s\n", symbol)
					return nil
				}
				fmt.Printf("References to %s:\n", symbol)
				for _, r := range refs {
					fmt.Printf("  %s\n", r)
				}
			} else {
				fmt.Println("Reference finding not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "definition",
		Description:  "Go to the definition of a symbol",
		ArgumentHint: "<symbol>",
		Category:     CategoryContext,
		Handler: func(args string, loop interface{}) error {
			symbol := strings.TrimSpace(args)
			if symbol == "" {
				fmt.Println("Usage: /definition <symbol>")
				return nil
			}
			type definitionFinder interface {
				FindDefinition(symbol string) (string, error)
			}
			if df, ok := loop.(definitionFinder); ok {
				location, err := df.FindDefinition(symbol)
				if err != nil {
					return fmt.Errorf("definition: %w", err)
				}
				fmt.Println(location)
			} else {
				fmt.Println("Definition lookup not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "hover",
		Description:     "Show hover information for a symbol",
		ArgumentHint:    "<symbol>",
		ResumeSupported: true,
		Category:        CategoryContext,
		Handler: func(args string, loop interface{}) error {
			symbol := strings.TrimSpace(args)
			if symbol == "" {
				fmt.Println("Usage: /hover <symbol>")
				return nil
			}
			type hoverProvider interface {
				GetHoverInfo(symbol string) (string, error)
			}
			if hp, ok := loop.(hoverProvider); ok {
				info, err := hp.GetHoverInfo(symbol)
				if err != nil {
					return fmt.Errorf("hover: %w", err)
				}
				fmt.Println(info)
			} else {
				fmt.Println("Hover information not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "diagnostics",
		Description:     "Show LSP diagnostics for a file",
		ArgumentHint:    "[path]",
		ResumeSupported: true,
		Category:        CategoryContext,
		Handler: func(args string, loop interface{}) error {
			type diagnosticsProvider interface {
				GetDiagnostics(path string) ([]string, error)
			}
			path := strings.TrimSpace(args)
			if dp, ok := loop.(diagnosticsProvider); ok {
				diags, err := dp.GetDiagnostics(path)
				if err != nil {
					return fmt.Errorf("diagnostics: %w", err)
				}
				if len(diags) == 0 {
					fmt.Println("No diagnostics.")
					return nil
				}
				fmt.Println("Diagnostics:")
				for _, d := range diags {
					fmt.Printf("  %s\n", d)
				}
			} else {
				fmt.Println("LSP diagnostics not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "branch",
		Description:  "Create/switch git branches",
		ArgumentHint: "<branch-name>",
		Category:     CategoryContext,
		Handler: func(args string, loop interface{}) error {
			name := strings.TrimSpace(args)
			if name == "" {
				fmt.Println("Usage: /branch <branch-name>")
				return nil
			}
			if !isValidBranchName(name) {
				return fmt.Errorf("branch: invalid branch name %q (use alphanumeric, dash, underscore, slash, dot)", name)
			}
			cmd := exec.Command("git", "checkout", "-b", name)
			output, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("branch: %s", strings.TrimSpace(string(output)))
			}
			fmt.Printf("Created and switched to branch %s\n", name)
			return nil
		},
	})
}
