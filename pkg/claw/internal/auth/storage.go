// Package auth handles authentication token storage and OAuth flows.
package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// TokenData holds persisted OAuth token data.
type TokenData struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	TokenType    string    `json:"token_type"`
	Scope        string    `json:"scope,omitempty"`
}

// authFilePath returns the path to ~/.claw-code/auth.json.
func authFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claw-code", "auth.json"), nil
}

// LoadTokens reads stored OAuth token data from disk.
// Returns os.ErrNotExist (wrapped) if no tokens have been saved yet.
func LoadTokens() (*TokenData, error) {
	path, err := authFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var td TokenData
	if err := json.Unmarshal(data, &td); err != nil {
		return nil, err
	}
	return &td, nil
}

// SaveTokens writes token data to ~/.claw-code/auth.json (mode 0600).
func SaveTokens(td *TokenData) error {
	path, err := authFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(td, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// ClearTokens removes the stored token file (idempotent).
func ClearTokens() error {
	path, err := authFilePath()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// IsExpired returns true if the token is expired or will expire within 60 seconds.
func IsExpired(td *TokenData) bool {
	if td.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(td.ExpiresAt.Add(-60 * time.Second))
}
