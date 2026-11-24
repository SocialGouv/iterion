package protocol

import (
	"encoding/binary"
	"io"
	"math"
	"path"
	"sort"
	"strings"
)

const frameMagic = "IENVONE1"
const profileFrameMagic = "IENVONE2"

// ExecutionProfile is resolved by the operator boundary, never from target Env.
// DefaultPath is used only if the final target environment lacks PATH. Empty
// explicitly searches cwd. Shell is the absolute executable for ENOEXEC only.
type ExecutionProfile struct{ DefaultPath, Shell string }

// Invocation contains native byte strings, not JSON strings. A nil WorkDir
// preserves the inherited directory and PWD. A nonnil value requests native
// chdir even when empty; callers must make that choice before transport.
type Invocation struct {
	Argv    []string
	Env     map[string]string
	WorkDir *string
	// A nil profile keeps the byte-identical legacy v1 frame and defaults.
	Profile *ExecutionProfile
}

// WriteInvocation validates the complete invocation before writing any bytes.
// It writes exactly one frame; the caller may append unframed workload stdin.
func WriteInvocation(w io.Writer, in Invocation) error {
	if len(in.Argv) == 0 {
		return refused("frame", "argv", nil)
	}
	body := uint64(24) // env count, WorkDir presence, argv count
	add := func(n uint64) bool {
		if n > math.MaxInt64-body {
			return false
		}
		body += n
		return true
	}
	part := func(s string) bool { return add(8) && add(uint64(len(s))) }
	magic := frameMagic
	if in.Profile != nil {
		if err := validateExecutionProfile(*in.Profile); err != nil {
			return err
		}
		if !part(in.Profile.DefaultPath) || !part(in.Profile.Shell) {
			return refused("frame", "length", nil)
		}
		magic = profileFrameMagic
	}
	keys := make([]string, 0, len(in.Env))
	for k, v := range in.Env {
		if strings.ContainsAny(k, "=\x00") || strings.IndexByte(v, 0) >= 0 {
			return refused("frame", "environment", nil)
		}
		if !part(k) || !part(v) {
			return refused("frame", "length", nil)
		}
		keys = append(keys, k)
	}
	if in.WorkDir != nil {
		if strings.IndexByte(*in.WorkDir, 0) >= 0 {
			return refused("frame", "directory", nil)
		}
		if !part(*in.WorkDir) {
			return refused("frame", "length", nil)
		}
	}
	for _, arg := range in.Argv {
		if strings.IndexByte(arg, 0) >= 0 {
			return refused("frame", "argv", nil)
		}
		if !part(arg) {
			return refused("frame", "length", nil)
		}
	}
	sort.Strings(keys)
	if err := writeAll(w, []byte(magic)); err != nil {
		return refused("frame", "write", err)
	}
	writeN := func(n uint64) error { var b [8]byte; binary.BigEndian.PutUint64(b[:], n); return writeAll(w, b[:]) }
	writePart := func(s string) error {
		if err := writeN(uint64(len(s))); err != nil {
			return err
		}
		return writeAll(w, []byte(s))
	}
	if err := writeN(body); err != nil {
		return refused("frame", "write", err)
	}
	if in.Profile != nil {
		if err := writePart(in.Profile.DefaultPath); err != nil {
			return refused("frame", "write", err)
		}
		if err := writePart(in.Profile.Shell); err != nil {
			return refused("frame", "write", err)
		}
	}
	if err := writeN(uint64(len(keys))); err != nil {
		return refused("frame", "write", err)
	}
	for _, k := range keys {
		if err := writePart(k); err != nil {
			return refused("frame", "write", err)
		}
		if err := writePart(in.Env[k]); err != nil {
			return refused("frame", "write", err)
		}
	}
	presence := uint64(0)
	if in.WorkDir != nil {
		presence = 1
	}
	if err := writeN(presence); err != nil {
		return refused("frame", "write", err)
	}
	if in.WorkDir != nil {
		if err := writePart(*in.WorkDir); err != nil {
			return refused("frame", "write", err)
		}
	}
	if err := writeN(uint64(len(in.Argv))); err != nil {
		return refused("frame", "write", err)
	}
	for _, arg := range in.Argv {
		if err := writePart(arg); err != nil {
			return refused("frame", "write", err)
		}
	}
	return nil
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// ReadInvocation never reads beyond the declared frame. In particular it does
// not buffer stdin bytes belonging to the eventual workload. Lengths are
// bounded by the frame and the platform's representable string size, not by an
// arbitrary protocol ceiling. Truncation never returns a partial invocation.
func ReadInvocation(r io.Reader) (Invocation, error) {
	bad := func(kind string, cause error) (Invocation, error) { return Invocation{}, refused("frame", kind, cause) }
	var header [16]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return bad("header", err)
	}
	version := string(header[:8])
	if version != frameMagic && version != profileFrameMagic {
		return bad("version", nil)
	}
	n := binary.BigEndian.Uint64(header[8:])
	if n > math.MaxInt64 {
		return bad("length", nil)
	}
	limited := &io.LimitedReader{R: r, N: int64(n)}
	number := func() (uint64, error) {
		var b [8]byte
		if _, err := io.ReadFull(limited, b[:]); err != nil {
			return 0, err
		}
		return binary.BigEndian.Uint64(b[:]), nil
	}
	part := func() (string, error) {
		n, err := number()
		if err != nil {
			return "", err
		}
		if n > uint64(limited.N) || n > uint64(^uint(0)>>1) {
			return "", refused("frame", "length", nil)
		}
		// Do not allocate the advertised length before reading: a truncated pipe
		// with a huge declaration must not turn into an eager huge allocation.
		var s strings.Builder
		if _, err = io.CopyN(&s, limited, int64(n)); err != nil {
			return "", err
		}
		return s.String(), nil
	}
	var profile *ExecutionProfile
	if version == profileFrameMagic {
		defaultPath, err := part()
		if err != nil {
			return bad("body", err)
		}
		shell, err := part()
		if err != nil {
			return bad("body", err)
		}
		value := ExecutionProfile{DefaultPath: defaultPath, Shell: shell}
		if err := validateExecutionProfile(value); err != nil {
			return Invocation{}, err
		}
		profile = &value
	}
	count, err := number()
	if err != nil {
		return bad("body", err)
	}
	if count > uint64(limited.N)/16 {
		return bad("environment-count", nil)
	}
	in := Invocation{Env: make(map[string]string), Profile: profile}
	for i := uint64(0); i < count; i++ {
		k, err := part()
		if err != nil {
			return bad("body", err)
		}
		v, err := part()
		if err != nil {
			return bad("body", err)
		}
		if strings.ContainsAny(k, "=\x00") || strings.IndexByte(v, 0) >= 0 {
			return bad("environment", nil)
		}
		if _, dup := in.Env[k]; dup {
			return bad("duplicate-environment", nil)
		}
		in.Env[k] = v
	}
	presence, err := number()
	if err != nil {
		return bad("body", err)
	}
	if presence > 1 {
		return bad("directory-presence", nil)
	}
	if presence == 1 {
		s, err := part()
		if err != nil {
			return bad("body", err)
		}
		if strings.IndexByte(s, 0) >= 0 {
			return bad("directory", nil)
		}
		in.WorkDir = &s
	}
	count, err = number()
	if err != nil {
		return bad("body", err)
	}
	if count == 0 || count > uint64(limited.N)/8 {
		return bad("argv-count", nil)
	}
	for i := uint64(0); i < count; i++ {
		s, err := part()
		if err != nil {
			return bad("body", err)
		}
		if strings.IndexByte(s, 0) >= 0 {
			return bad("argv", nil)
		}
		in.Argv = append(in.Argv, s)
	}
	if limited.N != 0 {
		return bad("trailing-frame", nil)
	}
	return in, nil
}

func validateExecutionProfile(p ExecutionProfile) error {
	if strings.IndexByte(p.DefaultPath, 0) >= 0 || strings.IndexByte(p.Shell, 0) >= 0 || !path.IsAbs(p.Shell) {
		return refused("profile", "execution", nil)
	}
	return nil
}
func effectiveExecutionProfile(p *ExecutionProfile) ExecutionProfile {
	if p == nil {
		return ExecutionProfile{DefaultPath: "/bin:/usr/bin", Shell: "/bin/sh"}
	}
	return *p
}
