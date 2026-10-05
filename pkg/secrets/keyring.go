package secrets

import (
	"fmt"
	"sort"
	"strings"
)

// KeyedSealer is the seam a caller uses when the KEY matters as much as
// the ciphertext: run-secret bundles record WHICH key sealed them (the
// record's KeyID) so rotation keeps old bundles openable and a runner
// never needs "whatever key the process holds" to match the sealing
// key. The plain Sealer methods keep their exact semantics (operate on
// the ring's CURRENT key) so single-key deployments read identically.
type KeyedSealer interface {
	Sealer
	// CurrentKeyID names the key Seal uses. Never empty for a ring.
	CurrentKeyID() string
	// SealWith seals under the NAMED key; an unknown id is an error.
	SealWith(keyID string, plaintext, aad []byte) ([]byte, error)
	// OpenWith opens under the NAMED key; an unknown id is an error.
	OpenWith(keyID string, sealed, aad []byte) ([]byte, error)
}

// KeyRingSealer implements KeyedSealer over a set of AES-GCM keys.
// Every key must be 32 bytes; the current id must name a ring entry.
type KeyRingSealer struct {
	current string
	keys    map[string]Sealer
}

// NewKeyRingSealer builds a ring. The map is copied; mutation by the
// caller after return does not affect the ring.
func NewKeyRingSealer(ring map[string][]byte, current string) (*KeyRingSealer, error) {
	if len(ring) == 0 {
		return nil, fmt.Errorf("secrets: empty key ring")
	}
	if current == "" {
		return nil, fmt.Errorf("secrets: key ring needs a current key id")
	}
	if _, ok := ring[current]; !ok {
		return nil, fmt.Errorf("secrets: current key id %q is not in the ring", current)
	}
	keys := make(map[string]Sealer, len(ring))
	for id, material := range ring {
		if id == "" {
			return nil, fmt.Errorf("secrets: key ring ids must be non-empty")
		}
		s, err := NewAESGCMSealer(material)
		if err != nil {
			return nil, fmt.Errorf("secrets: ring key %q: %w", id, err)
		}
		keys[id] = s
	}
	return &KeyRingSealer{current: current, keys: keys}, nil
}

// CurrentKeyID implements KeyedSealer.
func (s *KeyRingSealer) CurrentKeyID() string { return s.current }

// Seal implements Sealer over the current key.
func (s *KeyRingSealer) Seal(plaintext, aad []byte) ([]byte, error) {
	return s.keys[s.current].Seal(plaintext, aad)
}

// Open implements Sealer over the current key.
func (s *KeyRingSealer) Open(sealed, aad []byte) ([]byte, error) {
	return s.keys[s.current].Open(sealed, aad)
}

// SealWith implements KeyedSealer.
func (s *KeyRingSealer) SealWith(keyID string, plaintext, aad []byte) ([]byte, error) {
	key, ok := s.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("secrets: key %q is not in the ring", keyID)
	}
	return key.Seal(plaintext, aad)
}

// OpenWith implements KeyedSealer.
func (s *KeyRingSealer) OpenWith(keyID string, sealed, aad []byte) ([]byte, error) {
	key, ok := s.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("secrets: key %q is not in the ring (rotation removed it?)", keyID)
	}
	return key.Open(sealed, aad)
}

// OpenAny attempts every ring key in sorted-id order — the legacy
// bundle path, where the record predates key ids and the sealing key
// is only known to have been in the ring at some point. GCM
// authentication gates each attempt; trying keys is not a weakening.
func (s *KeyRingSealer) OpenAny(sealed, aad []byte) ([]byte, error) {
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var firstErr error
	for _, id := range ids {
		pt, err := s.keys[id].Open(sealed, aad)
		if err == nil {
			return pt, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// NewKeyRingFromConfig builds the sealer a cloud process seals and
// opens run bundles with. The ring spec ("id=base64,id=base64" with
// ITERION_SECRETS_KEY_ID naming the current key) is the rotation
// surface; a bare single key (the pre-ring configuration) becomes a
// one-entry ring under the id "default". Local desktop processes never
// seal run bundles and keep their own master-key path.
func NewKeyRingFromConfig(singleB64, ringSpec, currentID string) (KeyedSealer, error) {
	ringSpec = strings.TrimSpace(ringSpec)
	if ringSpec == "" {
		if strings.TrimSpace(singleB64) == "" {
			return nil, fmt.Errorf("secrets: no key material (ITERION_SECRETS_KEY or ITERION_SECRETS_KEYS)")
		}
		key, err := DecodeBase64Lenient(singleB64)
		if err != nil {
			return nil, fmt.Errorf("secrets: decode ITERION_SECRETS_KEY: %w", err)
		}
		return NewKeyRingSealer(map[string][]byte{"default": key}, "default")
	}
	ring := map[string][]byte{}
	for _, part := range strings.Split(ringSpec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, material, found := strings.Cut(part, "=")
		id = strings.TrimSpace(id)
		if !found || id == "" {
			return nil, fmt.Errorf("secrets: ITERION_SECRETS_KEYS entry %q is not id=base64", part)
		}
		if _, dup := ring[id]; dup {
			return nil, fmt.Errorf("secrets: ITERION_SECRETS_KEYS repeats id %q", id)
		}
		key, err := DecodeBase64Lenient(material)
		if err != nil {
			return nil, fmt.Errorf("secrets: decode ITERION_SECRETS_KEYS entry %q: %w", id, err)
		}
		ring[id] = key
	}
	if currentID == "" {
		return nil, fmt.Errorf("secrets: ITERION_SECRETS_KEYS set but ITERION_SECRETS_KEY_ID does not name the current key")
	}
	return NewKeyRingSealer(ring, currentID)
}
