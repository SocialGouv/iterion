package commands

import (
	"fmt"
	"os"
	"strings"
)

// RegisterGitCommands registers git and project management slash commands.
func RegisterGitCommands(r *Registry) {
	r.Register(Command{
		Name:         "stash",
		Description:  "Git stash operations",
		ArgumentHint: "[list|pop|push|drop]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			if !requireGit() {
				return nil
			}
			sub := strings.TrimSpace(args)
			if sub == "" {
				sub = "list"
			}
			switch sub {
			case "list":
				out, err := runGit("stash", "list")
				if err != nil {
					return err
				}
				if out == "" {
					fmt.Println("No stashes.")
				} else {
					fmt.Println(out)
				}
			case "push":
				_, err := runGit("stash", "push")
				if err != nil {
					return err
				}
				fmt.Println("Changes stashed.")
			case "pop":
				_, err := runGit("stash", "pop")
				if err != nil {
					return err
				}
				fmt.Println("Stash applied and removed.")
			case "drop":
				_, err := runGit("stash", "drop")
				if err != nil {
					return err
				}
				fmt.Println("Top stash dropped.")
			default:
				fmt.Printf("Unknown stash subcommand: %s\n", sub)
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "blame",
		Description:  "Show git blame for a file",
		ArgumentHint: "<file>",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			if !requireGit() {
				return nil
			}
			file := strings.TrimSpace(args)
			if file == "" {
				fmt.Println("Usage: /blame <file>")
				return nil
			}
			out, err := runGit("blame", "--porcelain", file)
			if err != nil {
				return fmt.Errorf("blame: %w", err)
			}
			fmt.Println(out)
			return nil
		},
	})

	r.Register(Command{
		Name:            "log",
		Description:     "Show recent git commits",
		ArgumentHint:    "[count]",
		ResumeSupported: true,
		Category:        CategoryCode,
		Handler: func(args string, loop interface{}) error {
			if !requireGit() {
				return nil
			}
			count := "10"
			if n := strings.TrimSpace(args); n != "" {
				count = n
			}
			out, err := runGit("log", "--oneline", "-"+count)
			if err != nil {
				return err
			}
			fmt.Println(out)
			return nil
		},
	})

	r.Register(Command{
		Name:         "git",
		Description:  "Run a git command",
		ArgumentHint: "<subcommand> [args...]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			if !requireGit() {
				return nil
			}
			parts := strings.Fields(args)
			if len(parts) == 0 {
				fmt.Println("Usage: /git <subcommand> [args...]")
				return nil
			}
			out, err := runGit(parts...)
			if err != nil {
				return fmt.Errorf("git: %w\n%s", err, out)
			}
			if out != "" {
				fmt.Println(out)
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "project",
		Description:     "Show project information",
		ResumeSupported: true,
		Category:        CategoryDiagnostics,
		Handler: func(args string, loop interface{}) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("project: %w", err)
			}
			fmt.Printf("Project root: %s\n", cwd)

			// Check for common project files
			projectFiles := []string{"package.json", "Cargo.toml", "go.mod", "pyproject.toml", "Makefile", "pom.xml", "build.gradle"}
			for _, f := range projectFiles {
				if _, err := os.Stat(f); err == nil {
					fmt.Printf("  Found: %s\n", f)
				}
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "env",
		Description:     "Show relevant environment variables",
		ResumeSupported: true,
		Category:        CategoryDiagnostics,
		Handler: func(args string, loop interface{}) error {
			envVars := []string{
				"ANTHROPIC_API_KEY",
				"ANTHROPIC_MODEL",
				"ANTHROPIC_BASE_URL",
				"CLAUDE_CODE_USE_BEDROCK",
				"CLAUDE_CODE_USE_VERTEX",
				"CLAUDE_CODE_USE_FOUNDRY",
				"CLAUDE_CODE_TELEMETRY_DIR",
				"HOME",
				"SHELL",
				"TERM",
			}
			for _, env := range envVars {
				val := os.Getenv(env)
				if val == "" {
					val = "(not set)"
				} else if strings.Contains(strings.ToLower(env), "key") || strings.Contains(strings.ToLower(env), "token") {
					// Mask sensitive values
					if len(val) > 8 {
						val = val[:4] + "..." + val[len(val)-4:]
					} else {
						val = "****"
					}
				}
				fmt.Printf("  %s = %s\n", env, val)
			}
			return nil
		},
	})

	// Note: /sandbox is registered by RegisterPluginCommands (plugin_cmds.go).

	r.Register(Command{
		Name:        "reset",
		Description: "Reset session state (clear history, tokens, focus)",
		Category:    CategorySession,
		Handler: func(args string, loop interface{}) error {
			type sessionResetter interface {
				ResetSession() error
			}
			sr, ok := loop.(sessionResetter)
			if !ok {
				fmt.Println("Session reset not available in this context.")
				return nil
			}
			if err := sr.ResetSession(); err != nil {
				return err
			}
			fmt.Println("Session state reset.")
			return nil
		},
	})

	r.Register(Command{
		Name:        "migrate",
		Description: "Run pending data migrations",
		Category:    CategoryCode,
		Handler: func(args string, loop interface{}) error {
			type migrator interface {
				RunMigrations() (string, error)
			}
			if m, ok := loop.(migrator); ok {
				result, err := m.RunMigrations()
				if err != nil {
					return fmt.Errorf("migrate: %w", err)
				}
				fmt.Println(result)
			} else {
				fmt.Println("Migration not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "benchmark",
		Description:  "Run performance benchmarks",
		ArgumentHint: "[suite]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			type benchmarker interface {
				RunBenchmark(suite string) (string, error)
			}
			suite := strings.TrimSpace(args)
			if bm, ok := loop.(benchmarker); ok {
				result, err := bm.RunBenchmark(suite)
				if err != nil {
					return fmt.Errorf("benchmark: %w", err)
				}
				fmt.Println(result)
			} else {
				fmt.Println("Benchmarks not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "terminal-setup",
		Aliases:         []string{"term-setup"},
		Description:     "Show terminal setup information and recommendations",
		ResumeSupported: true,
		Category:        CategoryDiagnostics,
		Handler: func(args string, loop interface{}) error {
			fmt.Println("Terminal Setup:")
			fmt.Printf("  TERM:      %s\n", os.Getenv("TERM"))
			fmt.Printf("  SHELL:     %s\n", os.Getenv("SHELL"))
			fmt.Printf("  LANG:      %s\n", os.Getenv("LANG"))
			fmt.Printf("  COLORTERM: %s\n", os.Getenv("COLORTERM"))

			// Check for common terminal features
			term := os.Getenv("TERM")
			if strings.Contains(term, "256color") || os.Getenv("COLORTERM") == "truecolor" {
				fmt.Println("  Colors:    256-color or truecolor supported")
			} else {
				fmt.Println("  Colors:    limited color support")
				fmt.Println("  Tip:       Set TERM=xterm-256color for better rendering")
			}
			return nil
		},
	})
}
