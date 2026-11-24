package protocol

import (
	"bytes"
	"io"
	"sort"
	"strings"
)

// ReadNativeEnvironment decodes the capsule's NUL-terminated native strings.
// It preserves order, duplicate entries and bytes that are not UTF-8. It owns
// no descriptor; the caller must close its owned capsule on every path.
func ReadNativeEnvironment(r io.Reader) ([]string, error) {
	var entries []string
	var pending strings.Builder
	var buf [32 * 1024]byte
	for {
		n, err := r.Read(buf[:])
		if n < 0 || n > len(buf) {
			return nil, refused("capsule", "read-count", io.ErrNoProgress)
		}
		chunk := buf[:n]
		for {
			i := bytes.IndexByte(chunk, 0)
			if i < 0 {
				pending.Write(chunk)
				break
			}
			pending.Write(chunk[:i])
			entries = append(entries, pending.String())
			pending.Reset()
			chunk = chunk[i+1:]
		}
		if err != nil {
			if err != io.EOF {
				return nil, refused("capsule", "read", err)
			}
			if pending.Len() != 0 {
				return nil, refused("capsule", "unterminated", io.ErrUnexpectedEOF)
			}
			return entries, nil
		}
		// A legal Reader may temporarily return 0,nil; the owned regular-file
		// capsule does not, but this codec preserves the io.Reader contract.
	}
}

// ApplyEnvironmentOverrides leaves unselected inherited entries in their exact
// native order. An override removes every occurrence of its exact key before
// appending one deterministic replacement. It never mutates either argument or
// installs any value in the helper process environment.
func ApplyEnvironmentOverrides(inherited []string, overrides map[string]string) ([]string, error) {
	keys := make([]string, 0, len(overrides))
	for key, value := range overrides {
		if strings.ContainsAny(key, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return nil, refused("environment", "override", nil)
		}
		keys = append(keys, key)
	}
	result := make([]string, 0, len(inherited))
	for _, entry := range inherited {
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, refused("environment", "entry", nil)
		}
		if key, _, hasValue := strings.Cut(entry, "="); hasValue {
			if _, replaced := overrides[key]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+overrides[key])
	}
	return result, nil
}
