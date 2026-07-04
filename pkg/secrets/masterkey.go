package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"

	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	// masterKeyService / masterKeyAccount identify the AES-GCM master key
	// entry in the OS keychain (macOS Keychain / libsecret / Windows
	// Credential Manager). Distinct from cmd/iterion-desktop's
	// "io.iterion.desktop" (which holds LLM-provider API keys), so the two
	// concerns never collide.
	masterKeyService = "io.iterion"
	masterKeyAccount = "secrets-master-key"

	// MasterKeyFileName is the basename of the keyfile fallback used when
	// the OS keychain is unavailable (headless servers with no dbus/session).
	MasterKeyFileName = "secrets.key"
)

// LoadOrCreateMasterKey resolves the 32-byte AES-GCM master key that seals
// the local secret store, in this precedence order:
//
//  1. ITERION_SECRETS_KEY (base64) — explicit operator override (parity with
//     cloud, scriptable/CI). Must decode to exactly 32 bytes.
//  2. An existing keyfile at <dataDir>/secrets.key — a prior run already
//     chose the keyfile; reuse it so a store sealed on a headless host stays
//     openable even if a keychain later becomes available (no orphaning).
//  3. The OS keychain entry, when present.
//  4. Keychain empty but functional → generate a fresh key and store it in
//     the keychain.
//  5. Keychain unavailable → generate a fresh key and write the keyfile
//     (0600), logging the fallback explicitly (no silent recovery).
//
// logf is an optional Warn-level sink (pass logger.Warn; nil is a no-op).
func LoadOrCreateMasterKey(dataDir string, logf func(string, ...any)) ([]byte, error) {
	warn := func(format string, args ...any) {
		if logf != nil {
			logf(format, args...)
		}
	}

	// 1. Explicit env override.
	if env := strings.TrimSpace(os.Getenv("ITERION_SECRETS_KEY")); env != "" {
		key, err := DecodeBase64Lenient(env)
		if err != nil {
			return nil, fmt.Errorf("secrets: decode ITERION_SECRETS_KEY: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("secrets: ITERION_SECRETS_KEY must decode to 32 bytes, got %d", len(key))
		}
		return key, nil
	}

	keyfilePath := filepath.Join(dataDir, MasterKeyFileName)

	// 2. Existing keyfile wins (prior headless choice — avoid orphaning a
	//    store already sealed with it).
	if key, ok, err := readKeyfile(keyfilePath); err != nil {
		return nil, err
	} else if ok {
		return key, nil
	}

	// 3. Keychain read.
	keychainBroken := false
	if b64, err := keyring.Get(masterKeyService, masterKeyAccount); err == nil {
		key, derr := DecodeBase64Lenient(b64)
		if derr == nil && len(key) == 32 {
			return key, nil
		}
		// A present-but-corrupt keychain entry is a hard error: silently
		// regenerating would orphan an existing sealed store.
		return nil, fmt.Errorf("secrets: keychain entry %s/%s is not a valid 32-byte key (re-set ITERION_SECRETS_KEY or clear the keychain entry)", masterKeyService, masterKeyAccount)
	} else if !errors.Is(err, keyring.ErrNotFound) {
		// Keychain is unavailable (no dbus/session, unsupported platform).
		keychainBroken = true
	}

	// Orphan guard: reaching here means no key was resolvable from env, an
	// existing keyfile, or the keychain — yet if a sealed store already exists,
	// it was sealed with a key we can no longer produce. Minting a fresh one
	// would silently make every stored secret undecryptable (erreurs-explicites:
	// surface the failure, never mask it with a fresh key). A first-ever run has
	// no store yet, so minting there is correct.
	if sealedStoreExists(dataDir) {
		return nil, fmt.Errorf("secrets: a sealed secret store exists at %s but its master key is unavailable (keychain unreadable/empty and no keyfile). The stored secrets cannot be decrypted — set ITERION_SECRETS_KEY to the original key if you have it, restore ~/.iterion/secrets.key, or delete the store to start fresh", filepath.Join(dataDir, LocalSecretsFileName))
	}

	// 4 & 5. Generate a fresh key, prefer keychain, fall back to keyfile.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secrets: generate master key: %w", err)
	}
	b64 := base64.StdEncoding.EncodeToString(key)

	if !keychainBroken {
		if err := keyring.Set(masterKeyService, masterKeyAccount, b64); err == nil {
			return key, nil
		} else {
			warn("secrets: OS keychain write failed (%v) — falling back to keyfile", err)
		}
	}

	if err := store.WriteFileAtomic(keyfilePath, []byte(b64+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("secrets: write master keyfile %s: %w", keyfilePath, err)
	}
	warn("secrets: OS keychain unavailable — master key written to %s (0600). Protect this file: it decrypts the local secret store.", keyfilePath)
	return key, nil
}

// sealedStoreExists reports whether a global sealed secret store file exists
// in dataDir. The store file is written only after a secret is sealed (the
// store never persists an empty file), so its presence means a master key
// once sealed it — used to refuse minting a fresh (orphaning) key. Note: only
// the global layer is checked; a project-only store elsewhere is not covered.
func sealedStoreExists(dataDir string) bool {
	fi, err := os.Stat(filepath.Join(dataDir, LocalSecretsFileName))
	return err == nil && fi.Size() > 0
}

// readKeyfile reads and decodes the base64 master key at path. Returns
// (key, true, nil) when present and valid, (nil, false, nil) when the file
// does not exist, and an error for a present-but-malformed file (never
// silently ignored).
func readKeyfile(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("secrets: read master keyfile %s: %w", path, err)
	}
	key, derr := DecodeBase64Lenient(strings.TrimSpace(string(data)))
	if derr != nil {
		return nil, false, fmt.Errorf("secrets: decode master keyfile %s: %w", path, derr)
	}
	if len(key) != 32 {
		return nil, false, fmt.Errorf("secrets: master keyfile %s must decode to 32 bytes, got %d", path, len(key))
	}
	return key, true, nil
}

// NewLocalSealer resolves the master key (LoadOrCreateMasterKey) and builds
// the AES-GCM sealer for the local secret store. Convenience for the studio/
// CLI wiring so callers don't repeat the key→sealer dance.
func NewLocalSealer(dataDir string, logf func(string, ...any)) (*AESGCMSealer, error) {
	key, err := LoadOrCreateMasterKey(dataDir, logf)
	if err != nil {
		return nil, err
	}
	return NewAESGCMSealer(key)
}

// lazySealer defers master-key resolution (a keychain round-trip, and on first
// run the eager minting of a keychain entry / keyfile) until the first Seal or
// Open. The studio wires this so merely launching it — for an operator who
// never opens the Secrets view or declares a secret — does not create key
// material (nor raise a macOS keychain-unlock prompt). The `sealer != nil` gate
// that surfaces the Secrets UI is satisfied without touching the keychain.
type lazySealer struct {
	dataDir string
	logf    func(string, ...any)
	mu      sync.Mutex
	sealer  Sealer
}

// NewLazyLocalSealer returns a Sealer that resolves the real AES-GCM sealer on
// first use. Errors from master-key resolution surface at the first Seal/Open,
// not at construction.
func NewLazyLocalSealer(dataDir string, logf func(string, ...any)) Sealer {
	return &lazySealer{dataDir: dataDir, logf: logf}
}

// resolve builds (once) and caches the real sealer. Only success is cached: a
// transient failure (keychain momentarily locked / a macOS unlock denied) is
// NOT memoized, so a later Seal/Open retries after the condition clears —
// unlike sync.Once, which would poison the whole process for its lifetime.
func (l *lazySealer) resolve() (Sealer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sealer != nil {
		return l.sealer, nil
	}
	s, err := NewLocalSealer(l.dataDir, l.logf)
	if err != nil {
		return nil, err
	}
	l.sealer = s
	return s, nil
}

func (l *lazySealer) Seal(plaintext, aad []byte) ([]byte, error) {
	s, err := l.resolve()
	if err != nil {
		return nil, err
	}
	return s.Seal(plaintext, aad)
}

func (l *lazySealer) Open(sealed, aad []byte) ([]byte, error) {
	s, err := l.resolve()
	if err != nil {
		return nil, err
	}
	return s.Open(sealed, aad)
}
