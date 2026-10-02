package commands

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// RegisterDiagnosticCommands registers diagnostic slash commands.
func RegisterDiagnosticCommands(r *Registry) {
	r.Register(Command{
		Name:            "doctor",
		Description:     "Diagnose setup issues and environment health",
		ResumeSupported: true,
		Category:        CategoryDiagnostics,
		Handler: func(args string, loop interface{}) error {
			fmt.Println("Running diagnostics...")
			fmt.Println()
			allOK := true

			// Check Go version
			fmt.Printf("  Go version:   %s", runtime.Version())
			fmt.Println(" ✓")

			// Check git
			gitPath, err := exec.LookPath("git")
			if err != nil {
				fmt.Println("  git:          not found ⚠")
				fmt.Println("    Hint: Install git to enable diff, commit, and branch operations.")
				allOK = false
			} else {
				fmt.Printf("  git:          %s ✓\n", gitPath)
			}

			// Check API key
			apiKey := os.Getenv("ANTHROPIC_API_KEY")
			if apiKey == "" {
				fmt.Println("  API key:      not set ⚠")
				fmt.Println("    Hint: Set ANTHROPIC_API_KEY or run /login to authenticate.")
				allOK = false
			} else {
				masked := apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
				fmt.Printf("  API key:      %s ✓\n", masked)
			}

			// Check provider flags
			for _, env := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
				if val := os.Getenv(env); val != "" {
					fmt.Printf("  %s = %s\n", env, val)
				}
			}

			// Check sandbox
			type sandboxChecker interface {
				SandboxEnabled() bool
			}
			if sc, ok := loop.(sandboxChecker); ok {
				if sc.SandboxEnabled() {
					fmt.Println("  Sandbox:      enabled ✓")
				} else {
					fmt.Println("  Sandbox:      disabled")
				}
			}

			fmt.Println()
			if allOK {
				fmt.Println("All checks passed.")
			} else {
				fmt.Println("Some checks need attention. See hints above.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "diff",
		Description:     "Show git diff for current workspace changes",
		ResumeSupported: true,
		Category:        CategoryDiagnostics,
		Handler: func(args string, loop interface{}) error {
			gitPath, err := exec.LookPath("git")
			if err != nil {
				fmt.Println("git not found. Install git to use /diff.")
				return nil
			}

			cmdArgs := []string{"diff"}
			if extra := strings.TrimSpace(args); extra != "" {
				cmdArgs = append(cmdArgs, strings.Fields(extra)...)
			}

			cmd := exec.Command(gitPath, cmdArgs...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("git diff: %w", err)
			}
			return nil
		},
	})
}
