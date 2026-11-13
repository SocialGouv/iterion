package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func codexTestToken(exp time.Time) string {
	claims, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	return "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}

func writeCodexAuthFixture(t *testing.T, path, token, accountID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]string{
			"access_token":  token,
			"account_id":    accountID,
			"refresh_token": "secret-refresh-token",
		},
	})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAuthPath(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "custom"))
	path, err := CodexAuthPath()
	if err != nil || path != filepath.Join(os.Getenv("CODEX_HOME"), "auth.json") {
		t.Fatalf("CODEX_HOME path = %q, %v", path, err)
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", home)
	path, err = CodexAuthPath()
	if err != nil || path != filepath.Join(home, ".codex", "auth.json") {
		t.Fatalf("default path = %q, %v", path, err)
	}
}

func TestLoadCodexCredentialsAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	token := codexTestToken(time.Now().Add(time.Hour))
	writeCodexAuthFixture(t, path, token, "account-1")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := LoadCodexCredentials(path)
	if err != nil || creds.AccessToken != token || creds.AccountID != "account-1" {
		t.Fatalf("credentials = %+v, %v", creds, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("reading Codex auth modified its file")
	}
	writeCodexAuthFixture(t, path, codexTestToken(time.Now().Add(-time.Minute)), "account-1")
	_, err = LoadCodexCredentials(path)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token error = %v", err)
	}
}

func TestLoadCodexCredentialsRejectsBadFileWithoutSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"secret-access-token","refresh_token":"secret-refresh-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCodexCredentials(path)
	if err == nil || strings.Contains(err.Error(), "secret-") {
		t.Fatalf("missing account ID error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"tokens":"secret-refresh-token"`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadCodexCredentials(path)
	if err == nil || strings.Contains(err.Error(), "secret-") {
		t.Fatalf("malformed file error = %v", err)
	}
}
