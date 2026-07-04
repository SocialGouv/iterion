package secrets

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestLoadOrCreateMasterKey_EnvOverride(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	t.Setenv("ITERION_SECRETS_KEY", base64.StdEncoding.EncodeToString(key))
	got, err := LoadOrCreateMasterKey(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("LoadOrCreateMasterKey: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatalf("env key not honoured")
	}
}

func TestLoadOrCreateMasterKey_EnvOverrideWrongSize(t *testing.T) {
	t.Setenv("ITERION_SECRETS_KEY", base64.StdEncoding.EncodeToString([]byte("too-short")))
	if _, err := LoadOrCreateMasterKey(t.TempDir(), nil); err == nil {
		t.Fatalf("expected error for non-32-byte key")
	}
}

func TestLoadOrCreateMasterKey_KeychainCreateAndReuse(t *testing.T) {
	t.Setenv("ITERION_SECRETS_KEY", "")
	keyring.MockInit()
	dir := t.TempDir()

	k1, err := LoadOrCreateMasterKey(dir, nil)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("key size %d", len(k1))
	}
	// No keyfile should have been written when the keychain works.
	if _, err := os.Stat(filepath.Join(dir, MasterKeyFileName)); !os.IsNotExist(err) {
		t.Fatalf("keyfile written despite working keychain")
	}
	// Second call returns the same key from the keychain.
	k2, err := LoadOrCreateMasterKey(dir, nil)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatalf("keychain key not stable across calls")
	}
}

func TestLoadOrCreateMasterKey_ExistingKeyfileWins(t *testing.T) {
	t.Setenv("ITERION_SECRETS_KEY", "")
	keyring.MockInit() // keychain available, but an existing keyfile must win
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x22}, 32)
	if err := os.WriteFile(filepath.Join(dir, MasterKeyFileName), []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatalf("seed keyfile: %v", err)
	}
	got, err := LoadOrCreateMasterKey(dir, nil)
	if err != nil {
		t.Fatalf("LoadOrCreateMasterKey: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatalf("existing keyfile not preferred over keychain")
	}
}

func TestLoadOrCreateMasterKey_KeyfileFallbackWhenKeychainBroken(t *testing.T) {
	t.Setenv("ITERION_SECRETS_KEY", "")
	keyring.MockInitWithError(keyring.ErrUnsupportedPlatform)
	dir := t.TempDir()
	var warned bool
	k1, err := LoadOrCreateMasterKey(dir, func(string, ...any) { warned = true })
	if err != nil {
		t.Fatalf("LoadOrCreateMasterKey: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("key size %d", len(k1))
	}
	if !warned {
		t.Fatalf("expected an explicit warn on keyfile fallback")
	}
	fi, err := os.Stat(filepath.Join(dir, MasterKeyFileName))
	if err != nil {
		t.Fatalf("keyfile not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keyfile perm = %o, want 600", perm)
	}
	// Reuse: existing keyfile is read back (same key).
	k2, err := LoadOrCreateMasterKey(dir, nil)
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatalf("keyfile key not stable across calls")
	}
}

func TestLoadOrCreateMasterKey_RefusesMintWhenStoreExists(t *testing.T) {
	t.Setenv("ITERION_SECRETS_KEY", "")
	keyring.MockInitWithError(keyring.ErrUnsupportedPlatform) // keychain unavailable
	dir := t.TempDir()
	// A sealed store exists but there is no keyfile and no keychain key — minting
	// a fresh key would orphan it, so resolution must hard-error.
	if err := os.WriteFile(filepath.Join(dir, LocalSecretsFileName), []byte(`{"version":1,"secrets":[{"id":"x","name":"T","sealed":"AAo="}]}`), 0o600); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if _, err := LoadOrCreateMasterKey(dir, nil); err == nil {
		t.Fatalf("expected error refusing to mint a fresh key over an existing store")
	}
	// No keyfile should have been minted.
	if _, err := os.Stat(filepath.Join(dir, MasterKeyFileName)); !os.IsNotExist(err) {
		t.Fatalf("orphaning keyfile was minted despite existing store")
	}
}

func TestLoadOrCreateMasterKey_CorruptKeyfileErrors(t *testing.T) {
	t.Setenv("ITERION_SECRETS_KEY", "")
	keyring.MockInitWithError(keyring.ErrUnsupportedPlatform)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MasterKeyFileName), []byte("!!!not-base64!!!"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := LoadOrCreateMasterKey(dir, nil); err == nil {
		t.Fatalf("expected error on corrupt keyfile")
	}
}
