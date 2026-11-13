package auth

import (
	"fmt"
	"os"
	"time"
)

// Status holds the current authentication state.
type Status struct {
	Authenticated bool
	Method        string // "api_key", "oauth", "none"
	ExpiresAt     time.Time
	HasRefresh    bool
}

// GetAccessToken returns a valid credential for the Anthropic API.
//
// Resolution order:
//  1. ANTHROPIC_API_KEY env var (returned directly as the key value)
//  2. OAuth tokens from ~/.claw-code/auth.json (auto-refreshed if expired)
//
// Returns an error if no credentials are available.
func GetAccessToken() (string, error) {
	// API key takes precedence over OAuth.
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return key, nil
	}

	td, err := LoadTokens()
	if err != nil {
		return "", fmt.Errorf("no credentials found: set ANTHROPIC_API_KEY or run '/auth login'")
	}

	if IsExpired(td) {
		if td.RefreshToken == "" {
			return "", fmt.Errorf("oauth token expired and no refresh token available; run '/auth login'")
		}
		td, err = RefreshToken(td.RefreshToken)
		if err != nil {
			return "", fmt.Errorf("refresh oauth token: %w", err)
		}
		if saveErr := SaveTokens(td); saveErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not persist refreshed token: %v\n", saveErr)
		}
	}

	return td.AccessToken, nil
}

// IsOAuthAuth returns true when credentials come from stored OAuth tokens
// (i.e., ANTHROPIC_API_KEY is not set but valid tokens are on disk).
func IsOAuthAuth() bool {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return false
	}
	_, err := LoadTokens()
	return err == nil
}

// GetStatus returns a snapshot of the current authentication state.
func GetStatus() Status {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return Status{Authenticated: true, Method: "api_key"}
	}
	td, err := LoadTokens()
	if err != nil {
		return Status{Authenticated: false, Method: "none"}
	}
	return Status{
		Authenticated: !IsExpired(td),
		Method:        "oauth",
		ExpiresAt:     td.ExpiresAt,
		HasRefresh:    td.RefreshToken != "",
	}
}
