package apikit

import (
	"encoding/json"
	"fmt"
)

const (
	fnvOffsetBasis            uint64 = 0xcbf29ce484222325
	fnvPrime                  uint64 = 0x00000100000001b3
	currentFingerprintVersion uint32 = 1
	requestFingerprintPrefix         = "v1"
)

// StableHashBytes computes FNV-1a hash over the given bytes, matching the
// exact Rust constants for cross-language cache compatibility.
func StableHashBytes(data []byte) uint64 {
	hash := fnvOffsetBasis
	for _, b := range data {
		hash ^= uint64(b)
		hash *= fnvPrime
	}
	return hash
}

// hashSerializable hashes the JSON serialization of a value.
func hashSerializable(value any) uint64 {
	data, err := json.Marshal(value)
	if err != nil {
		return StableHashBytes(nil)
	}
	return StableHashBytes(data)
}

// requestHashHex returns a versioned hex hash for a cache request.
func requestHashHex(request *CacheRequest) string {
	return fmt.Sprintf("%s-%016x", requestFingerprintPrefix, hashSerializable(request))
}

// requestFingerprints holds per-field hashes for cache break detection.
type requestFingerprints struct {
	model    uint64
	system   uint64
	tools    uint64
	messages uint64
}

func fingerprintsFromRequest(request *CacheRequest) requestFingerprints {
	return requestFingerprints{
		model:    hashSerializable(request.Model),
		system:   hashSerializable(request.System),
		tools:    hashSerializable(request.Tools),
		messages: hashSerializable(request.Messages),
	}
}

// trackedPromptState tracks the state needed for cache break detection.
type trackedPromptState struct {
	ObservedAtUnixSecs   uint64 `json:"observed_at_unix_secs"`
	FingerprintVersion   uint32 `json:"fingerprint_version"`
	ModelHash            uint64 `json:"model_hash"`
	SystemHash           uint64 `json:"system_hash"`
	ToolsHash            uint64 `json:"tools_hash"`
	MessagesHash         uint64 `json:"messages_hash"`
	CacheReadInputTokens uint32 `json:"cache_read_input_tokens"`
}

func trackedFromUsage(request *CacheRequest, usage *CacheUsage) *trackedPromptState {
	fp := fingerprintsFromRequest(request)
	return &trackedPromptState{
		ObservedAtUnixSecs:   nowUnixSecs(),
		FingerprintVersion:   currentFingerprintVersion,
		ModelHash:            fp.model,
		SystemHash:           fp.system,
		ToolsHash:            fp.tools,
		MessagesHash:         fp.messages,
		CacheReadInputTokens: usage.CacheReadInputTokens,
	}
}
