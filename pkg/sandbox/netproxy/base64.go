package netproxy

import "encoding/base64"

// base64Decode wraps stdlib base64 with a small adapter that keeps
// the proxy.go file focused on protocol concerns. We use Std (not
// URL) encoding because Basic-Auth uses standard base64 per RFC 7617.
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
