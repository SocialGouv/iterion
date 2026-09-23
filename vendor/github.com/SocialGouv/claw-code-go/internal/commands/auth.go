package commands

import (
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/auth"
)

// requireInteractiveLoop returns an error message and true if the loop is nil
// or non-interactive, indicating the command should not proceed.
func requireInteractiveLoop(loop interface{}) bool {
	if loop == nil {
		return true
	}
	type loginContext interface {
		IsInteractive() bool
	}
	if lc, ok := loop.(loginContext); ok && !lc.IsInteractive() {
		return true
	}
	return false
}

// RegisterAuthCommands adds the /auth command group to the registry.
func RegisterAuthCommands(r *Registry) {
	r.Register(Command{
		Name:        "auth",
		Description: "Authentication: /auth login | logout | status",
		Category:    CategoryAuth,
		Handler:     handleAuthCommand,
	})

	r.Register(Command{
		Name:        "login",
		Description: "Log in to the service",
		Category:    CategoryAuth,
		Handler: func(args string, loop interface{}) error {
			if requireInteractiveLoop(loop) {
				fmt.Println("Login requires an interactive session.")
				return nil
			}
			return cmdAuthLogin()
		},
	})

	r.Register(Command{
		Name:        "logout",
		Description: "Log out of the current session",
		Category:    CategoryAuth,
		Handler: func(args string, loop interface{}) error {
			if loop == nil {
				fmt.Println("Logout requires an interactive session.")
				return nil
			}
			return cmdAuthLogout()
		},
	})
}

func handleAuthCommand(args string, loop interface{}) error {
	sub, _ := splitSubcommand(args)
	if sub == "" {
		sub = "status"
	}

	switch sub {
	case "login":
		if requireInteractiveLoop(loop) {
			fmt.Println("Login requires an interactive session.")
			return nil
		}
		return cmdAuthLogin()
	case "logout":
		if loop == nil {
			fmt.Println("Logout requires an interactive session.")
			return nil
		}
		return cmdAuthLogout()
	case "status":
		return cmdAuthStatus()
	default:
		fmt.Printf("Unknown auth subcommand %q. Usage: /auth login | logout | status\n", sub)
		return nil
	}
}

func cmdAuthLogin() error {
	fmt.Println("Starting OAuth login...")
	td, err := auth.StartOAuthFlow()
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if err := auth.SaveTokens(td); err != nil {
		return fmt.Errorf("save tokens: %w", err)
	}
	fmt.Println("Login successful. Token saved to ~/.claw-code/auth.json")
	return nil
}

func cmdAuthLogout() error {
	if err := auth.ClearTokens(); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	fmt.Println("Logged out. Stored tokens cleared.")
	return nil
}

func cmdAuthStatus() error {
	s := auth.GetStatus()
	fmt.Printf("Auth status:\n")
	fmt.Printf("  Authenticated : %v\n", s.Authenticated)
	fmt.Printf("  Method        : %s\n", s.Method)
	if s.Method == "oauth" && !s.ExpiresAt.IsZero() {
		fmt.Printf("  Token expires : %s\n", s.ExpiresAt.Format("2006-01-02 15:04:05 MST"))
		fmt.Printf("  Has refresh   : %v\n", s.HasRefresh)
	}
	return nil
}
