// Package devcontainer parses the subset of devcontainer.json fields
// iterion's sandbox driver consumes.
//
// Spec reference: https://containers.dev/implementors/json_reference/
//
// MVP coverage (per .plans/on-va-tudier-la-snappy-lemon.md §3):
//
//	image, build.{dockerfile,context,args}
//	containerEnv, remoteEnv
//	mounts (devcontainer mount syntax)
//	runArgs (whitelisted: --cap-add, --security-opt; --privileged refused)
//	remoteUser, containerUser, workspaceFolder, workspaceMount
//	postCreateCommand
//
// Ignored on purpose:
//
//	customizations.vscode.*    (out of scope)
//	initializeCommand          (host-side hook, violates sandbox boundary)
//	onCreateCommand,           (Codespaces prebuild concern)
//	updateContentCommand
//
// Deferred to V2:
//
//	postStartCommand, forwardPorts, features, hostRequirements
//
// The parser tolerates JSONC / JSON5 conventions commonly seen in
// .devcontainer/devcontainer.json: line comments (`// ...`), block
// comments (`/* ... */`), and trailing commas. We strip these before
// json.Unmarshal so the strict stdlib decoder can read the file.
package devcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// File is the parsed shape of a devcontainer.json. Field names mirror
// the spec; types are the most permissive form callers may then
// canonicalise (e.g. command-as-string vs command-as-array).
type File struct {
	Name string `json:"name,omitempty"`

	// Image and Build are mutually exclusive — the spec calls this out
	// and so do we during validation.
	Image string `json:"image,omitempty"`
	Build *Build `json:"build,omitempty"`

	ContainerEnv map[string]string `json:"containerEnv,omitempty"`
	RemoteEnv    map[string]string `json:"remoteEnv,omitempty"`

	Mounts []string `json:"mounts,omitempty"`

	RunArgs []string `json:"runArgs,omitempty"`

	RemoteUser      string `json:"remoteUser,omitempty"`
	ContainerUser   string `json:"containerUser,omitempty"`
	WorkspaceFolder string `json:"workspaceFolder,omitempty"`
	WorkspaceMount  string `json:"workspaceMount,omitempty"`

	// PostCreateCommand can be a string ("npm install") or an array
	// (["npm", "install"]). We accept both shapes via a custom type.
	PostCreateCommand Command `json:"postCreateCommand,omitempty"`

	// Forwarded V2 fields kept on the struct so unmarshal doesn't drop
	// them silently. Iterion doesn't read them today.
	PostStartCommand Command        `json:"postStartCommand,omitempty"`
	ForwardPorts     []any          `json:"forwardPorts,omitempty"`
	Features         map[string]any `json:"features,omitempty"`
	HostRequirements map[string]any `json:"hostRequirements,omitempty"`
}

// Build mirrors the devcontainer.json `build` object.
type Build struct {
	Dockerfile string            `json:"dockerfile,omitempty"`
	Context    string            `json:"context,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	Target     string            `json:"target,omitempty"`
}

// Command represents a devcontainer command property: either a single
// shell string or a list-form argv. Iterion canonicalises both to a
// single shell string via [Command.Shell] — block-form arrays are
// joined with spaces.
type Command struct {
	Shell string
	Argv  []string
}

// UnmarshalJSON accepts either a string or an array of strings.
func (c *Command) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		return json.Unmarshal(data, &c.Shell)
	}
	if data[0] == '[' {
		return json.Unmarshal(data, &c.Argv)
	}
	return fmt.Errorf("devcontainer: command must be a string or array, got %s", data[:1])
}

// MarshalJSON preserves the original form when round-tripping.
func (c Command) MarshalJSON() ([]byte, error) {
	if c.Shell != "" {
		return json.Marshal(c.Shell)
	}
	if c.Argv != nil {
		return json.Marshal(c.Argv)
	}
	return []byte("null"), nil
}

// Empty reports whether the command carries no content.
func (c Command) Empty() bool {
	return c.Shell == "" && len(c.Argv) == 0
}

// AsShell returns the command as a shell-snippet string. Argv form is
// joined with spaces — callers that need precise quoting should pass
// the Argv form through their own quoting routine.
func (c Command) AsShell() string {
	if c.Shell != "" {
		return c.Shell
	}
	return strings.Join(c.Argv, " ")
}

// ReadFromRepo locates and parses a devcontainer.json in the canonical
// locations:
//
//  1. <repoRoot>/.devcontainer/devcontainer.json
//  2. <repoRoot>/.devcontainer.json
//
// The first existing file wins. Missing files return [ErrNotFound]
// (not a generic "no such file" error) so callers can surface a clear
// "use sandbox: inline or add a .devcontainer/" message.
func ReadFromRepo(repoRoot string) (*File, string, error) {
	candidates := []string{
		filepath.Join(repoRoot, ".devcontainer", "devcontainer.json"),
		filepath.Join(repoRoot, ".devcontainer.json"),
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			f, err := Parse(data)
			if err != nil {
				return nil, path, fmt.Errorf("devcontainer: parse %s: %w", path, err)
			}
			return f, path, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, path, fmt.Errorf("devcontainer: read %s: %w", path, err)
		}
	}
	return nil, "", ErrNotFound
}

// ErrNotFound signals that no devcontainer.json was found in the
// canonical locations. Callers translate this to the C-sandbox
// diagnostic at compile time, or to the user-visible "no
// devcontainer.json — use sandbox: inline" hint at runtime.
var ErrNotFound = errors.New("devcontainer: no devcontainer.json found in .devcontainer/ or repo root")

// Parse reads JSON-with-comments+trailing-commas content and returns
// the parsed [File] plus a validation pass.
func Parse(data []byte) (*File, error) {
	cleaned := stripJSONC(data)
	var f File
	if err := json.Unmarshal(cleaned, &f); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// ParseReader is a convenience for callers holding an io.Reader (e.g.
// HTTP response, embedded fs).
func ParseReader(r io.Reader) (*File, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Validate enforces the spec invariants we depend on. Called by
// [Parse] but exported so callers that build a [File] in code (tests,
// in-memory fixtures) can run the same checks.
func (f *File) Validate() error {
	if f == nil {
		return errors.New("devcontainer: nil file")
	}
	if f.Image == "" && f.Build == nil {
		return errors.New("devcontainer: must set either image or build")
	}
	if f.Image != "" && f.Build != nil {
		return errors.New("devcontainer: image and build are mutually exclusive")
	}
	if f.Build != nil && f.Build.Dockerfile == "" {
		return errors.New("devcontainer: build.dockerfile is required when build is set")
	}
	for _, arg := range f.RunArgs {
		if isPrivilegedRunArg(arg) {
			return fmt.Errorf("devcontainer: runArgs must not include %q (privileged is refused for safety)", arg)
		}
	}
	if f.WorkspaceFolder != "" && !strings.HasPrefix(f.WorkspaceFolder, "/") {
		return fmt.Errorf("devcontainer: workspaceFolder %q must be absolute", f.WorkspaceFolder)
	}
	return nil
}

// isPrivilegedRunArg reports whether a runArgs entry would grant the
// container privileges iterion explicitly disallows. These flags
// negate the entire point of the sandbox. The list is intentionally
// small — adding `--cap-add SYS_ADMIN` or `--security-opt seccomp=...`
// is permissible (security choice the workflow author makes), but
// `--privileged` shorthand for "all the things" is not.
func isPrivilegedRunArg(arg string) bool {
	switch arg {
	case "--privileged":
		return true
	}
	return false
}

// stripJSONC removes line-comments, block-comments and trailing commas
// from a JSON-with-comments / JSON5 input so the strict stdlib decoder
// accepts it. Performs no syntax-aware parsing — it walks bytes,
// respecting string literals (including escapes).
//
// This is intentionally minimal: it covers the conventions VS Code's
// schema uses (and that we observe in real devcontainer.json files).
// It does NOT cover unquoted keys or single-quoted strings — those
// are JSON5 features less commonly seen in practice.
func stripJSONC(in []byte) []byte {
	out := make([]byte, 0, len(in))
	i := 0
	inString := false
	for i < len(in) {
		c := in[i]
		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < len(in) {
				out = append(out, in[i+1])
				i += 2
				continue
			}
			if c == '"' {
				inString = false
			}
			i++
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			i++
			continue
		}
		if c == '/' && i+1 < len(in) && in[i+1] == '/' {
			// line comment until newline
			for i < len(in) && in[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(in) && in[i+1] == '*' {
			i += 2
			for i+1 < len(in) && !(in[i] == '*' && in[i+1] == '/') {
				i++
			}
			i += 2 // consume `*/`
			continue
		}
		// trailing comma: ", }" or ", ]" (allowing whitespace in between)
		if c == ',' {
			j := i + 1
			for j < len(in) && (in[j] == ' ' || in[j] == '\t' || in[j] == '\n' || in[j] == '\r') {
				j++
			}
			if j < len(in) && (in[j] == '}' || in[j] == ']') {
				i++ // skip the comma
				continue
			}
		}
		out = append(out, c)
		i++
	}
	return out
}
