package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCodexCredentialsFrom(t *testing.T) {
	dir := t.TempDir()
	blob := `{"auth_mode":"chatgpt","tokens":{"access_token":"tok-1","account_id":"acct-1"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := LoadCodexCredentialsFrom(dir)
	if err != nil {
		t.Fatalf("LoadCodexCredentialsFrom: %v", err)
	}
	if !v.IsChatGPTMode() {
		t.Fatalf("expected chatgpt mode, got %+v", v)
	}
	if v.Tokens.AccessToken != "tok-1" || v.Tokens.AccountID != "acct-1" {
		t.Errorf("wrong tokens: %+v", v.Tokens)
	}

	if _, err := LoadCodexCredentialsFrom(""); err == nil {
		t.Error("expected error on empty dir")
	}
	if _, err := LoadCodexCredentialsFrom(t.TempDir()); err == nil {
		t.Error("expected error when auth.json is absent")
	}
}
