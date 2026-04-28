package apikit

import (
	"fmt"
	"os"
	"path/filepath"
)

const maxSanitizedLength = 80

// baseCacheRoot returns the root directory for prompt cache data.
func baseCacheRoot() string {
	if configHome := os.Getenv("CLAUDE_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "cache", "prompt-cache")
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".claude", "cache", "prompt-cache")
	}
	return filepath.Join(os.TempDir(), "claude-prompt-cache")
}

// SanitizePathSegment replaces non-alphanumeric characters with hyphens and
// caps the length at maxSanitizedLength, appending a hash suffix if needed.
func SanitizePathSegment(value string) string {
	runes := []rune(value)
	sanitized := make([]rune, len(runes))
	for i, ch := range runes {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			sanitized[i] = ch
		} else {
			sanitized[i] = '-'
		}
	}
	result := string(sanitized)
	if len(result) <= maxSanitizedLength {
		return result
	}
	suffix := fmt.Sprintf("-%x", StableHashBytes([]byte(value)))
	prefixLen := maxSanitizedLength - len(suffix)
	if prefixLen < 0 {
		prefixLen = 0
	}
	return result[:prefixLen] + suffix
}
