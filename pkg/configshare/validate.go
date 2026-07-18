package configshare

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

const (
	maxFeeds     = 50
	maxFeedLen   = 512
	maxEditorial = 8192
)

// ValidateLeaf checks one edited leaf value against the constraints for its
// field (keyed by the last path segment). The two veille fields — feeds and
// editorial — carry the SSRF/size guards that keep a hostile editor's write
// from becoming an internal fetch or an oversized prompt; an unknown field
// passes structurally (the allow-list already gated its path).
func ValidateLeaf(dotted string, value any) error {
	seg := dotted
	if i := strings.LastIndex(dotted, "."); i >= 0 {
		seg = dotted[i+1:]
	}
	switch seg {
	case "feeds":
		return validateFeeds(value)
	case "editorial":
		return validateEditorial(value)
	}
	return nil
}

func validateFeeds(value any) error {
	arr, ok := value.([]any)
	if !ok {
		return fmt.Errorf("feeds must be a list of URL strings")
	}
	if len(arr) > maxFeeds {
		return fmt.Errorf("too many feeds (%d > %d)", len(arr), maxFeeds)
	}
	seen := map[string]bool{}
	for _, it := range arr {
		s, ok := it.(string)
		if !ok {
			return fmt.Errorf("each feed must be a URL string")
		}
		if len(s) > maxFeedLen {
			return fmt.Errorf("feed URL too long (%d > %d)", len(s), maxFeedLen)
		}
		u, err := url.Parse(s)
		if err != nil {
			return fmt.Errorf("invalid feed URL %q", s)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("feed URL must be http(s): %q", s)
		}
		if u.User != nil {
			return fmt.Errorf("feed URL must not carry credentials: %q", s)
		}
		host := u.Hostname()
		if host == "" {
			return fmt.Errorf("feed URL has no host: %q", s)
		}
		if net.ParseIP(host) != nil {
			return fmt.Errorf("feed URL host must be a name, not an IP literal: %q", s)
		}
		if seen[s] {
			return fmt.Errorf("duplicate feed URL: %q", s)
		}
		seen[s] = true
	}
	return nil
}

func validateEditorial(value any) error {
	s, ok := value.(string)
	if !ok {
		return fmt.Errorf("editorial must be a string")
	}
	if len(s) > maxEditorial {
		return fmt.Errorf("editorial too long (%d > %d bytes)", len(s), maxEditorial)
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
			return fmt.Errorf("editorial contains a control character")
		}
	}
	// Refuse the run-time fence marker so a write can't attempt to forge the
	// untrusted-editorial delimiter the bot wraps this value in.
	if strings.Contains(s, "UNTRUSTED_EDITORIAL") {
		return fmt.Errorf("editorial must not contain the reserved fence marker")
	}
	return nil
}
