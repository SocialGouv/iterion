package commands

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// promptInjector is the loop interface for commands that inject a prompt
// back into the conversation rather than printing directly.
type promptInjector interface {
	InjectPrompt(prompt string) error
}

// injectOrPrint tries to inject prompt into the conversation loop.
// Falls back to printing the prompt to stdout.
func injectOrPrint(loop interface{}, prompt string) error {
	if pi, ok := loop.(promptInjector); ok {
		return pi.InjectPrompt(prompt)
	}
	fmt.Println(prompt)
	return nil
}

// promptCommandHandler returns a handler for simple prompt-injection commands
// like /explain, /refactor, /docs, /fix, /perf. basePrompt is used when no
// target argument is provided; targetFmt is a format string with one %s for
// the target.
func promptCommandHandler(basePrompt, targetFmt string) func(args string, loop interface{}) error {
	return func(args string, loop interface{}) error {
		target := strings.TrimSpace(args)
		prompt := basePrompt
		if target != "" {
			prompt = fmt.Sprintf(targetFmt, target)
		}
		return injectOrPrint(loop, prompt)
	}
}

// toolSpec describes a project marker file and the tool command to run.
type toolSpec struct {
	marker  string
	command string
	args    []string
}

// detectAndRunTool finds the first matching project tool and runs it.
// Returns true if a tool was found and executed.
func detectAndRunTool(specs []toolSpec, label string) (bool, error) {
	for _, s := range specs {
		if _, err := os.Stat(s.marker); err != nil {
			continue
		}
		if _, err := exec.LookPath(s.command); err != nil {
			continue
		}
		fmt.Printf("%s with %s...\n", label, s.command)
		cmd := exec.Command(s.command, s.args...)
		out, err := cmd.CombinedOutput()
		result := strings.TrimSpace(string(out))
		if err != nil {
			return true, fmt.Errorf("%s (%s) failed:\n%s\n%w", label, s.command, result, err)
		}
		if result != "" {
			fmt.Println(result)
		}
		return true, nil
	}
	return false, nil
}

// runGit executes a git command with separate arguments (never shell-interpolated)
// and returns the trimmed combined output.
func runGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitAvailable returns true if the git binary is on PATH.
func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// requireGit prints a message and returns false if git is not available.
// Use at the start of command handlers: if !requireGit() { return nil }
func requireGit() bool {
	if gitAvailable() {
		return true
	}
	fmt.Println("git is not available on PATH.")
	return false
}

// RegisterCodeCommands registers code-operations slash commands.
func RegisterCodeCommands(r *Registry) {
	r.Register(Command{
		Name:        "commit",
		Description: "Generate commit message from git diff",
		Category:    CategoryCode,
		Handler: func(args string, loop interface{}) error {
			if !requireGit() {
				return nil
			}

			fullDiff, err := runGit("diff", "--cached")
			if err != nil {
				return fmt.Errorf("commit: %w", err)
			}
			if fullDiff == "" {
				fmt.Println("No staged changes. Use `git add` to stage files first.")
				return nil
			}

			stat, _ := runGit("diff", "--cached", "--stat")
			prompt := fmt.Sprintf("Based on the following staged changes, generate a concise git commit message following conventional commit format.\n\n```diff\n%s\n```\n\nStat summary:\n%s", fullDiff, stat)

			return injectOrPrint(loop, prompt)
		},
	})

	r.Register(Command{
		Name:         "pr",
		Description:  "Draft/create pull request",
		ArgumentHint: "[title]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			if !requireGit() {
				return nil
			}

			branch, err := runGit("rev-parse", "--abbrev-ref", "HEAD")
			if err != nil {
				return fmt.Errorf("pr: %w", err)
			}

			log, err := runGit("log", "--oneline", "-20")
			if err != nil {
				return fmt.Errorf("pr: %w", err)
			}

			diff, err := runGit("diff", "HEAD~5..HEAD", "--stat")
			if err != nil {
				// Non-fatal: repo might have fewer than 5 commits.
				diff = "(diff unavailable)"
			}

			title := strings.TrimSpace(args)
			if title == "" {
				title = branch
			}

			prompt := fmt.Sprintf("Create a pull request for branch '%s' with the title '%s'.\n\nRecent commits:\n%s\n\nDiff summary:\n%s\n\nGenerate a PR title and description with a ## Summary section and ## Test plan checklist.", branch, title, log, diff)

			return injectOrPrint(loop, prompt)
		},
	})

	r.Register(Command{
		Name:         "issue",
		Description:  "Draft GitHub issue from conversation context",
		ArgumentHint: "[title]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			title := strings.TrimSpace(args)
			if title == "" {
				title = "New Issue"
			}
			fmt.Printf("## GitHub Issue: %s\n\n", title)
			fmt.Println("### Description")
			fmt.Println()
			fmt.Println("### Steps to Reproduce")
			fmt.Println()
			fmt.Println("### Expected Behavior")
			fmt.Println()
			fmt.Println("### Actual Behavior")
			fmt.Println()
			fmt.Println("\nFill in the sections above based on the conversation context.")
			return nil
		},
	})

	r.Register(Command{
		Name:         "bughunter",
		Description:  "Scope-aware bug hunting prompt",
		ArgumentHint: "[scope]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			scope := strings.TrimSpace(args)
			if scope == "" {
				scope = "the current codebase"
			}
			fmt.Printf("Bug Hunting Scope: %s\n\n", scope)
			fmt.Println("Analyze the code in the given scope for:")
			fmt.Println("  1. Logic errors and off-by-one mistakes")
			fmt.Println("  2. Nil/null pointer dereferences")
			fmt.Println("  3. Resource leaks (unclosed files, connections)")
			fmt.Println("  4. Race conditions and concurrency issues")
			fmt.Println("  5. Error handling gaps (swallowed errors, missing checks)")
			fmt.Println("  6. Edge cases in input validation")
			fmt.Printf("\nFocus on: %s\n", scope)
			return nil
		},
	})

	r.Register(Command{
		Name:         "review",
		Description:  "Code review from git diff",
		ArgumentHint: "[--staged]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			if !requireGit() {
				return nil
			}

			diffArgs := []string{"diff"}
			if strings.TrimSpace(args) == "--staged" {
				diffArgs = append(diffArgs, "--cached")
			}

			diff, err := runGit(diffArgs...)
			if err != nil {
				return fmt.Errorf("review: %w", err)
			}
			if diff == "" {
				fmt.Println("No changes to review.")
				return nil
			}

			prompt := fmt.Sprintf("Review the following diff for correctness, logic errors, code style, readability, performance concerns, and test coverage gaps.\n\n```diff\n%s\n```", diff)

			return injectOrPrint(loop, prompt)
		},
	})

	r.Register(Command{
		Name:         "security-review",
		Description:  "Security-focused code review",
		ArgumentHint: "[--staged]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			if !requireGit() {
				return nil
			}

			diffArgs := []string{"diff"}
			if strings.TrimSpace(args) == "--staged" {
				diffArgs = append(diffArgs, "--cached")
			}

			diff, err := runGit(diffArgs...)
			if err != nil {
				return fmt.Errorf("security-review: %w", err)
			}
			if diff == "" {
				fmt.Println("No changes to review.")
				return nil
			}

			fmt.Println("## Security Review")
			fmt.Println()
			fmt.Println("Review the following diff for security issues:")
			fmt.Println("  - Injection vulnerabilities (SQL, command, XSS)")
			fmt.Println("  - Authentication and authorization flaws")
			fmt.Println("  - Sensitive data exposure (secrets, PII)")
			fmt.Println("  - Insecure deserialization")
			fmt.Println("  - Dependency vulnerabilities")
			fmt.Println("  - Path traversal and file access issues")
			fmt.Println("\n```diff")
			fmt.Println(diff)
			fmt.Println("```")
			return nil
		},
	})

	r.Register(Command{
		Name:         "release-notes",
		Description:  "Generate changelog from git log",
		ArgumentHint: "[since-tag]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			if !requireGit() {
				return nil
			}

			sinceTag := strings.TrimSpace(args)
			var logArgs []string
			if sinceTag != "" {
				logArgs = []string{"log", "--oneline", sinceTag + "..HEAD"}
			} else {
				// Try to find the last tag automatically.
				tag, err := runGit("describe", "--tags", "--abbrev=0")
				if err != nil || tag == "" {
					logArgs = []string{"log", "--oneline", "-30"}
				} else {
					logArgs = []string{"log", "--oneline", tag + "..HEAD"}
				}
			}

			log, err := runGit(logArgs...)
			if err != nil {
				return fmt.Errorf("release-notes: %w", err)
			}
			if log == "" {
				fmt.Println("No commits found for release notes.")
				return nil
			}

			fmt.Println("## Release Notes")
			fmt.Println()
			fmt.Println("Commits:")
			fmt.Println(log)
			fmt.Println("\nGenerate categorized release notes (features, fixes, breaking changes) from the above.")
			return nil
		},
	})

	r.Register(Command{
		Name:         "test",
		Description:  "Run project tests",
		ArgumentHint: "[--watch]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			testCmd, testArgs := detectTestCommand()
			if testCmd == "" {
				fmt.Println("Could not detect project type. No test command found.")
				return nil
			}

			if strings.TrimSpace(args) == "--watch" {
				fmt.Printf("Watch mode requested. Run: %s %s (watch not built-in; use project tooling).\n", testCmd, strings.Join(testArgs, " "))
				return nil
			}

			fmt.Printf("Running: %s %s\n", testCmd, strings.Join(testArgs, " "))
			cmd := exec.Command(testCmd, testArgs...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("test: %w", err)
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "explain",
		Description:  "Explain code in the current context",
		ArgumentHint: "[file or code reference]",
		Category:     CategoryCode,
		Handler:      promptCommandHandler("Please explain the code", "Please explain the code in %s"),
	})

	r.Register(Command{
		Name:         "refactor",
		Description:  "Refactor code in the current context",
		ArgumentHint: "[file or code reference]",
		Category:     CategoryCode,
		Handler:      promptCommandHandler("Please refactor the code for better readability, maintainability, and performance", "Please refactor the code in %s for better readability, maintainability, and performance"),
	})

	r.Register(Command{
		Name:         "docs",
		Description:  "Generate documentation for code",
		ArgumentHint: "[file or code reference]",
		Category:     CategoryCode,
		Handler:      promptCommandHandler("Please generate documentation for the code", "Please generate documentation for %s"),
	})

	r.Register(Command{
		Name:         "fix",
		Description:  "Fix code issues",
		ArgumentHint: "[file or description]",
		Category:     CategoryCode,
		Handler:      promptCommandHandler("Please identify and fix issues in the code", "Please identify and fix issues in %s"),
	})

	r.Register(Command{
		Name:         "perf",
		Description:  "Analyze code for performance issues",
		ArgumentHint: "[file or code reference]",
		Category:     CategoryCode,
		Handler:      promptCommandHandler("Please analyze the code for performance issues and suggest optimizations", "Please analyze %s for performance issues and suggest optimizations"),
	})

	r.Register(Command{
		Name:        "lint",
		Description: "Run linting on the project",
		Category:    CategoryCode,
		Handler: func(args string, loop interface{}) error {
			found, err := detectAndRunTool([]toolSpec{
				{"go.mod", "go", []string{"vet", "./..."}},
				{"package.json", "npm", []string{"run", "lint"}},
				{"Cargo.toml", "cargo", []string{"clippy"}},
				{"pyproject.toml", "python", []string{"-m", "flake8"}},
			}, "Linting")
			if err != nil {
				fmt.Println(err)
				return nil
			}
			if !found {
				fmt.Println("No supported linter found. Supported: go vet, npm lint, cargo clippy, flake8")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "build",
		Description: "Run the project build command",
		Category:    CategoryCode,
		Handler: func(args string, loop interface{}) error {
			found, err := detectAndRunTool([]toolSpec{
				{"go.mod", "go", []string{"build", "./..."}},
				{"Cargo.toml", "cargo", []string{"build"}},
				{"package.json", "npm", []string{"run", "build"}},
				{"Makefile", "make", nil},
			}, "Building")
			if err != nil {
				return err
			}
			if !found {
				fmt.Println("No supported build system found.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "run",
		Description:  "Run a command in the project context",
		ArgumentHint: "<command>",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			command := strings.TrimSpace(args)
			if command == "" {
				fmt.Println("Usage: /run <command>")
				return nil
			}
			type commandRunner interface {
				RunCommand(command string) (string, error)
			}
			if cr, ok := loop.(commandRunner); ok {
				output, err := cr.RunCommand(command)
				if err != nil {
					return fmt.Errorf("run: %w", err)
				}
				if output != "" {
					fmt.Println(output)
				}
			} else {
				fmt.Printf("Running: %s\n", command)
				cmd := exec.Command("sh", "-c", command)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return fmt.Errorf("run: %w", err)
				}
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "chat",
		Description: "Switch to free-form chat mode",
		Category:    CategoryCode,
		Handler: func(args string, loop interface{}) error {
			type chatMode interface {
				SetChatMode(on bool)
			}
			if cm, ok := loop.(chatMode); ok {
				cm.SetChatMode(true)
				fmt.Println("Switched to free-form chat mode.")
			} else {
				fmt.Println("Chat mode switching not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "web",
		Description:  "Fetch and summarize a web page",
		ArgumentHint: "<url>",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			url := strings.TrimSpace(args)
			if url == "" {
				fmt.Println("Usage: /web <url>")
				return nil
			}
			type webFetcher interface {
				FetchAndSummarize(url string) (string, error)
			}
			if wf, ok := loop.(webFetcher); ok {
				summary, err := wf.FetchAndSummarize(url)
				if err != nil {
					return fmt.Errorf("web: %w", err)
				}
				fmt.Println(summary)
			} else {
				fmt.Println("Web fetching not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "autofix",
		Description:  "Auto-fix all fixable diagnostics",
		ArgumentHint: "[path]",
		Category:     CategoryCode,
		Handler: func(args string, loop interface{}) error {
			type autofixer interface {
				Autofix(path string) (string, error)
			}
			path := strings.TrimSpace(args)
			if af, ok := loop.(autofixer); ok {
				result, err := af.Autofix(path)
				if err != nil {
					return fmt.Errorf("autofix: %w", err)
				}
				fmt.Println(result)
			} else {
				fmt.Println("Autofix not available in this context.")
			}
			return nil
		},
	})
}

// detectTestCommand inspects the working directory for known project markers
// and returns the appropriate test command and arguments.
func detectTestCommand() (string, []string) {
	if _, err := os.Stat("go.mod"); err == nil {
		return "go", []string{"test", "./..."}
	}
	if _, err := os.Stat("Cargo.toml"); err == nil {
		return "cargo", []string{"test"}
	}
	if _, err := os.Stat("package.json"); err == nil {
		return "npm", []string{"test"}
	}
	if _, err := os.Stat("Makefile"); err == nil {
		return "make", []string{"test"}
	}
	return "", nil
}
