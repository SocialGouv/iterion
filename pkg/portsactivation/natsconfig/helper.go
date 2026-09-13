package natsconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/nats-io/nats-server/v2/conf"
)

const HelperCommand = "__contracts-nats-config"
const ParserVersion = "2.14.5"

type Result struct {
	ParserVersion string                     `json:"parser_version"`
	Digest        string                     `json:"digest"`
	Config        map[string]json.RawMessage `json:"config"`
	// The upstream server tolerates unknown top-level fields used as local
	// variables. Preserve that evidence for the supported-profile evaluator;
	// a known operational/auth field must still be validated even if used.
	Variables []string `json:"variables,omitempty"`
}

// Parse runs the helper command of this same production binary. Its caller
// receives only a structural failure message, never an upstream parse error
// quoting credentials, source text or external environment values.
func Parse(ctx context.Context, sources Sources) (*Result, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("NATS authority parser executable is unavailable")
	}
	return parseWithExecutable(ctx, executable, []string{HelperCommand}, sources)
}

func parseWithExecutable(ctx context.Context, executable string, args []string, sources Sources) (*Result, error) {
	if err := sources.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(sources)
	if err != nil || len(body) > MaxMessageBytes {
		return nil, fmt.Errorf("NATS authority source request exceeds supported size")
	}
	// The parent owns scratch cleanup even if the parser is killed by its
	// deadline. A child-only defer would leave plaintext source files behind.
	scratch, err := os.MkdirTemp("", "iterion-nats-authority-")
	if err != nil {
		return nil, fmt.Errorf("NATS authority parser cannot allocate private scratch")
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	// The entrypoint must bypass .env autoload and error tracking as well.
	// Do not temporarily clear environment variables in the server process.
	cmd.Env = []string{}
	cmd.Dir = scratch
	cmd.Stdin = bytes.NewReader(body)
	var stdout boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("NATS authority parser did not complete: %w", ctx.Err())
		}
		return nil, fmt.Errorf("NATS authority source parser refused the configuration")
	}
	var result Result
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.ParserVersion != ParserVersion || result.Config == nil {
		return nil, fmt.Errorf("NATS authority parser returned an invalid result")
	}
	return &result, nil
}

// Do not embed bytes.Buffer: its promoted ReadFrom method would let io.Copy
// in os/exec bypass Write and the subprocess output limit.
type boundedBuffer struct{ buffer bytes.Buffer }

func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > MaxMessageBytes-b.buffer.Len() {
		return 0, fmt.Errorf("NATS authority parser output exceeds supported size")
	}
	return b.buffer.Write(p)
}

// RunHelper is an internal pipe protocol, not an operator verification or
// activation command. It neither opens NATS nor reads deployment credentials.
func RunHelper(in io.Reader, out io.Writer) error {
	if len(os.Environ()) != 0 {
		return fmt.Errorf("NATS authority parser requires an empty environment")
	}
	var sources Sources
	decoder := json.NewDecoder(io.LimitReader(in, MaxMessageBytes+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&sources) != nil || decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("NATS authority parser requires one valid source request")
	}
	if err := sources.Validate(); err != nil {
		return err
	}
	root, err := os.MkdirTemp(".", "sources-")
	if err != nil {
		return fmt.Errorf("NATS authority parser cannot allocate private scratch")
	}
	defer func() { _ = os.RemoveAll(root) }()
	for name, body := range sources.Files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			return fmt.Errorf("NATS authority parser cannot prepare source directory")
		}
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			return fmt.Errorf("NATS authority parser cannot prepare source file")
		}
	}
	parsed, digest, err := conf.ParseFileWithChecksDigest(filepath.Join(root, filepath.FromSlash(sources.Entry)))
	if err != nil {
		return fmt.Errorf("NATS authority configuration is invalid or requires an external variable")
	}
	result := Result{ParserVersion: ParserVersion, Digest: digest, Config: make(map[string]json.RawMessage, len(parsed))}
	for name, value := range parsed {
		if token, ok := value.(interface{ IsUsedVariable() bool }); ok && token.IsUsedVariable() {
			result.Variables = append(result.Variables, name)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("NATS authority configuration cannot be represented")
		}
		result.Config[name] = raw
	}
	slices.Sort(result.Variables)
	return json.NewEncoder(out).Encode(result)
}
