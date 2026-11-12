// Package execenv owns the dedicated host relay, separate from the guest leaf.
package execenv

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

const wirePrefix = "ITERION_EXECENV_WIRE_"
const wireCount = wirePrefix + "COUNT"
const hostMagic = "IENVHST1"
const chunkBytes = 24000 // A chunk size, not a payload ceiling; exec limits remain native.

type wireLaunch struct {
	ClientPath   string
	ClientArgs   []string
	Invocation   protocol.Invocation
	StartTimeout time.Duration
	WaitDelay    time.Duration
}

func refusal(phase, kind string, cause error) error {
	return &protocol.Error{Phase: phase, Kind: kind, Cause: cause}
}

func encodeWire(l wireLaunch) ([]byte, error) {
	if !filepath.IsAbs(l.ClientPath) || strings.IndexByte(l.ClientPath, 0) >= 0 {
		return nil, refusal("host", "client-path", nil)
	}
	if l.StartTimeout < 0 || l.WaitDelay < 0 {
		return nil, refusal("host", "duration", nil)
	}
	var b bytes.Buffer
	b.WriteString(hostMagic)
	var nums [16]byte
	binary.BigEndian.PutUint64(nums[:8], uint64(l.StartTimeout))
	binary.BigEndian.PutUint64(nums[8:], uint64(l.WaitDelay))
	b.Write(nums[:])
	// Reuse the byte-string codec, without a second JSON/native-string contract.
	// This control frame is a command only: Env, WorkDir and Profile are forbidden.
	control := protocol.Invocation{Argv: append([]string{l.ClientPath}, l.ClientArgs...)}
	if err := protocol.WriteInvocation(&b, control); err != nil {
		return nil, err
	}
	if err := protocol.WriteInvocation(&b, l.Invocation); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func decodeWire(b []byte) (wireLaunch, error) {
	var z wireLaunch
	r := bytes.NewReader(b)
	var h [24]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return z, refusal("host", "envelope-header", err)
	}
	if string(h[:8]) != hostMagic {
		return z, refusal("host", "envelope-version", nil)
	}
	a, d := binary.BigEndian.Uint64(h[8:16]), binary.BigEndian.Uint64(h[16:24])
	if a > math.MaxInt64 || d > math.MaxInt64 {
		return z, refusal("host", "duration", nil)
	}
	control, err := protocol.ReadInvocation(r)
	if err != nil {
		return z, err
	}
	if len(control.Env) != 0 || control.WorkDir != nil || control.Profile != nil || !filepath.IsAbs(control.Argv[0]) {
		return z, refusal("host", "control-shape", nil)
	}
	in, err := protocol.ReadInvocation(r)
	if err != nil {
		return z, err
	}
	if r.Len() != 0 {
		return z, refusal("host", "envelope-trailing", nil)
	}
	return wireLaunch{ClientPath: control.Argv[0], ClientArgs: control.Argv[1:], Invocation: in, StartTimeout: time.Duration(a), WaitDelay: time.Duration(d)}, nil
}

func wireEnvironment(control []string, wire []byte) ([]string, error) {
	out := append([]string(nil), control...)
	for _, e := range out {
		key, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(key, wirePrefix) {
			return nil, refusal("host", "reserved-environment", nil)
		}
		if strings.IndexByte(e, 0) >= 0 {
			return nil, refusal("host", "control-environment", nil)
		}
	}
	encoded := base64.StdEncoding.EncodeToString(wire)
	count := 0
	for len(encoded) > 0 {
		n := min(chunkBytes, len(encoded))
		out = append(out, wirePrefix+strconv.Itoa(count)+"="+encoded[:n])
		count++
		encoded = encoded[n:]
	}
	out = append(out, wireCount+"="+strconv.Itoa(count))
	return out, nil
}

func readWireEnvironment(environment []string) (wireLaunch, []string, error) {
	var z wireLaunch
	bad := func(kind string, cause error) (wireLaunch, []string, error) {
		return z, nil, refusal("host", kind, cause)
	}
	clean := make([]string, 0, len(environment))
	chunks := make(map[int]string)
	count := -1
	for _, e := range environment {
		key, value, equals := strings.Cut(e, "=")
		if !strings.HasPrefix(key, wirePrefix) {
			clean = append(clean, e)
			continue
		}
		if !equals {
			return bad("envelope-environment", nil)
		}
		suffix := strings.TrimPrefix(key, wirePrefix)
		if suffix == "COUNT" {
			n, err := canonicalNumber(value)
			if err != nil || count >= 0 {
				return bad("envelope-count", err)
			}
			count = n
			continue
		}
		n, err := canonicalNumber(suffix)
		if err != nil {
			return bad("envelope-key", err)
		}
		if _, exists := chunks[n]; exists {
			return bad("envelope-duplicate", nil)
		}
		chunks[n] = value
	}
	if count < 1 || count != len(chunks) {
		return bad("envelope-count", nil)
	}
	var b strings.Builder
	for i := 0; i < count; i++ {
		v, ok := chunks[i]
		if !ok {
			return bad("envelope-missing", nil)
		}
		b.WriteString(v)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(b.String())
	if err != nil {
		return bad("envelope-base64", err)
	}
	l, err := decodeWire(raw)
	if err != nil {
		return z, nil, err
	}
	return l, clean, nil
}

func canonicalNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < 0 || strconv.Itoa(n) != s {
		return 0, refusal("host", "envelope-number", nil)
	}
	return n, nil
}
