package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCreds(t *testing.T, blob string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestReadAnthropicExpiry(t *testing.T) {
	want := time.Now().Add(8 * time.Hour).UnixMilli()
	path := writeCreds(t, fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":"acc","refreshToken":"refr","expiresAt":%d}}`, want))

	exp, refreshTok, err := readAnthropicExpiry(path)
	if err != nil {
		t.Fatalf("readAnthropicExpiry: %v", err)
	}
	if refreshTok != "refr" {
		t.Errorf("refresh token = %q, want %q", refreshTok, "refr")
	}
	if exp.UnixMilli() != want {
		t.Errorf("expiry = %d, want %d", exp.UnixMilli(), want)
	}
}

func TestReadAnthropicExpiry_MissingFile(t *testing.T) {
	if _, _, err := readAnthropicExpiry(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// A credentials file with no refresh_token must make the loop give up (return)
// rather than spin — readAnthropicExpiry reports an empty refresh token, which
// the loop treats as "nothing we can do".
func TestReadAnthropicExpiry_NoRefreshToken(t *testing.T) {
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"acc","expiresAt":123}}`)
	_, refreshTok, err := readAnthropicExpiry(path)
	if err != nil {
		t.Fatalf("readAnthropicExpiry: %v", err)
	}
	if refreshTok != "" {
		t.Errorf("expected empty refresh token, got %q", refreshTok)
	}
}
