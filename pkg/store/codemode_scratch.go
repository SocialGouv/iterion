package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

const (
	CodemodeScratchDefaultEntries = scripthost.DefaultStoreEntries
	CodemodeScratchDefaultBytes   = scripthost.DefaultStoreBytes
	CodemodeScratchMaxEntries     = scripthost.MaxStoreEntries
	CodemodeScratchMaxBytes       = scripthost.MaxStoreBytes
	CodemodeScratchMaxKeyBytes    = scripthost.MaxStateKeyBytes
	CodemodeScratchMaxValueBytes  = scripthost.MaxStateValueBytes

	// Keys in the bank envelope can grow to six bytes per UTF-8 byte when
	// JSON-escaped. The envelope overhead does not count against user quota.
	codemodeScratchMaxEncodedBytes = CodemodeScratchMaxBytes + CodemodeScratchMaxEntries*(5*CodemodeScratchMaxKeyBytes+4) + 2
)

var (
	ErrCodemodeScratchQuota   = errors.New("codemode scratch quota exceeded")
	ErrCodemodeScratchInvalid = errors.New("invalid codemode scratch input")
)

// CodemodeScratchLimits bounds the entry count and total key + JSON bytes.
// A zero dimension selects its default; the launcher resolves run overrides.
type CodemodeScratchLimits struct {
	Entries int64
	Bytes   int64
}

// CodemodeScratchStore is an optional private, run-scoped JSON bank. A
// missing key in a live run returns (nil, false, nil); JSON null is present.
// A missing or deleted run returns ErrRunNotFound or ErrRunDeleted.
type CodemodeScratchStore interface {
	PutCodemodeScratch(ctx context.Context, runID, key string, value json.RawMessage, limits CodemodeScratchLimits) error
	LoadCodemodeScratch(ctx context.Context, runID, key string) (json.RawMessage, bool, error)
}

// AsCodemodeScratchStore resolves the capability through store decorators.
func AsCodemodeScratchStore(s RunStore) CodemodeScratchStore {
	return capability[CodemodeScratchStore](s)
}

// ValidateCodemodeScratchKey checks a key without including it in diagnostics.
func ValidateCodemodeScratchKey(key string) error {
	if len(key) == 0 || len(key) > CodemodeScratchMaxKeyBytes || !utf8.ValidString(key) {
		return fmt.Errorf("%w: key must be nonempty UTF-8 of at most %d bytes", ErrCodemodeScratchInvalid, CodemodeScratchMaxKeyBytes)
	}
	return nil
}

// CodemodeScratchPut prepares one mutation without referencing any backend.
// Backends apply it to a snapshot protected by a lock or compare-and-swap.
type CodemodeScratchPut struct {
	key    string
	value  json.RawMessage
	limits CodemodeScratchLimits
}

// PrepareCodemodeScratchPut validates and compacts the incoming write.
func PrepareCodemodeScratchPut(key string, value json.RawMessage, limits CodemodeScratchLimits) (CodemodeScratchPut, error) {
	if err := ValidateCodemodeScratchKey(key); err != nil {
		return CodemodeScratchPut{}, err
	}
	if limits.Entries < 0 || limits.Entries > CodemodeScratchMaxEntries || limits.Bytes < 0 || limits.Bytes > CodemodeScratchMaxBytes {
		return CodemodeScratchPut{}, fmt.Errorf("%w: limits exceed the storage bounds", ErrCodemodeScratchInvalid)
	}
	if limits.Entries == 0 {
		limits.Entries = CodemodeScratchDefaultEntries
	}
	if limits.Bytes == 0 {
		limits.Bytes = CodemodeScratchDefaultBytes
	}
	if len(value) > CodemodeScratchMaxValueBytes || !utf8.Valid(value) {
		return CodemodeScratchPut{}, fmt.Errorf("%w: value must be UTF-8 JSON of at most %d bytes", ErrCodemodeScratchInvalid, CodemodeScratchMaxValueBytes)
	}
	// Compact JSON without lossy number conversion or re-escaping strings.
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return CodemodeScratchPut{}, fmt.Errorf("%w: value must be one JSON document", ErrCodemodeScratchInvalid)
	}
	return CodemodeScratchPut{key: key, value: compact.Bytes(), limits: limits}, nil
}

// decodeCodemodeScratchBank never includes data from the private bank in errors.
func decodeCodemodeScratchBank(data []byte) (map[string]json.RawMessage, int64, error) {
	if data == nil {
		return make(map[string]json.RawMessage), 0, nil
	}
	corrupt := errors.New("store: invalid persisted codemode scratch bank")
	var bank map[string]json.RawMessage
	if len(data) > codemodeScratchMaxEncodedBytes || !utf8.Valid(data) || json.Unmarshal(data, &bank) != nil || bank == nil || len(bank) > CodemodeScratchMaxEntries {
		return nil, 0, corrupt
	}
	var size int64
	for key, value := range bank {
		if ValidateCodemodeScratchKey(key) != nil || len(value) > CodemodeScratchMaxValueBytes {
			return nil, 0, corrupt
		}
		size += int64(len(key) + len(value))
	}
	if size > CodemodeScratchMaxBytes {
		return nil, 0, corrupt
	}
	return bank, size, nil
}

// LoadCodemodeScratchValue reads a value from a private bank snapshot. The
// caller validates the key even if the run or its bank is absent.
func LoadCodemodeScratchValue(data []byte, key string) (json.RawMessage, bool, error) {
	bank, _, err := decodeCodemodeScratchBank(data)
	if err != nil {
		return nil, false, err
	}
	value, ok := bank[key]
	return value, ok, nil
}

// Apply accounts for a replacement before encoding the new snapshot. A
// lowered limit never deletes data: a non-growing replacement is allowed
// even if the existing bank is over the new limit.
func (p CodemodeScratchPut) Apply(data []byte) ([]byte, error) {
	bank, size, err := decodeCodemodeScratchBank(data)
	if err != nil {
		return nil, err
	}
	old, exists := bank[p.key]
	delta := int64(len(p.value) - len(old))
	if !exists {
		delta += int64(len(p.key))
	}
	entries := int64(len(bank))
	if !exists {
		entries++
	}
	if entries > p.limits.Entries && delta > 0 {
		return nil, fmt.Errorf("%w: entry limit %d", ErrCodemodeScratchQuota, p.limits.Entries)
	}
	if size+delta > p.limits.Bytes && delta > 0 {
		return nil, fmt.Errorf("%w: byte limit %d", ErrCodemodeScratchQuota, p.limits.Bytes)
	}
	bank[p.key] = p.value
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(bank); err != nil {
		return nil, errors.New("store: encode codemode scratch bank")
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}), nil
}
