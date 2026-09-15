package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CodexCredentials contains only the fields needed for ChatGPT-Codex requests.
// The refresh token remains owned by Codex and is never loaded into this type.
type CodexCredentials struct {
	AccessToken string
	AccountID   string
}

// CodexAuthPath follows Codex's CODEX_HOME override, then its default home.
func CodexAuthPath() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "auth.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("Codex authentication directory unavailable")
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

// LoadCodexCredentials reads a fresh snapshot of Codex's auth file. Codex
// owns refresh-token rotation; callers must never rewrite this file.
func LoadCodexCredentials(path string) (CodexCredentials, error) {
	const hint = "Codex authentication unavailable; run `codex login` and retry"
	for attempt := 0; attempt < 2; attempt++ {
		data, err := os.ReadFile(path)
		if err != nil {
			return CodexCredentials{}, errors.New(hint)
		}
		var document struct {
			AuthMode string `json:"auth_mode"`
			Tokens   struct {
				AccessToken string `json:"access_token"`
				AccountID   string `json:"account_id"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			// Codex may have replaced auth.json between the open and read.
			if attempt == 0 {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			return CodexCredentials{}, errors.New(hint)
		}
		if document.AuthMode != "chatgpt" || document.Tokens.AccessToken == "" || document.Tokens.AccountID == "" {
			return CodexCredentials{}, errors.New(hint)
		}
		if expiredCodexToken(document.Tokens.AccessToken, time.Now()) {
			return CodexCredentials{}, errors.New("Codex access token expired; open Codex to refresh your login, then retry")
		}
		tokens := document.Tokens
		return CodexCredentials{AccessToken: tokens.AccessToken, AccountID: tokens.AccountID}, nil
	}
	return CodexCredentials{}, errors.New(hint)
}

// A JWT expiry is advisory; the backend remains authoritative if the token
// has no readable exp claim. This also supports externally managed tokens.
func expiredCodexToken(token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Expires int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Expires <= 0 {
		return false
	}
	return !now.Before(time.Unix(claims.Expires, 0))
}
