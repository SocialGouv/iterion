package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/internal/shellquote"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// ClaudeCodeRoutingEnv is every environment variable the Claude Code CLI reads
// to decide where a request carrying the run's credential goes, or which
// channel carries it: the API endpoints, the provider each one selects and the
// companions the CLI groups with them, the proxies, and the TLS trust of the
// connection.
//
// The CLI copies the `env` block of every settings source it loads over its own
// environment, at startup and again whenever a settings file changes. The
// target repository is the project source: without a pin, its committed
// `.claude/settings.json` points the CLI, and the API key or subscription token
// it sends, at any endpoint. Every claude_code spawn therefore pins each of
// these variables in the flag settings layer, which only managed policy
// settings outrank (claudeRoutingPin).
//
// The list follows the CLI's own routing tables (2.1.282: its endpoint map,
// its provider switches, its proxy and TLS sets). It leaves out:
//   - credentials: they stay in the process environment, never in a settings
//     object;
//   - ANTHROPIC_UNIX_SOCKET: the CLI drops it from every settings source;
//   - the cloud SDK variables (AWS_*, GOOGLE_*, AZURE_*), which only matter
//     once a provider switch above selects that cloud. The settings `env`
//     reaches the agent's commands, and those SDKs read a set-but-empty
//     variable differently from an unset one.
var ClaudeCodeRoutingEnv = []string{
	"ANTHROPIC_BASE_URL",
	"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL",
	"ANTHROPIC_CUSTOM_HEADERS",
	"CLAUDE_CODE_API_BASE_URL",
	"CLAUDE_CODE_USE_GATEWAY",

	"ANTHROPIC_BEDROCK_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
	"CLAUDE_CODE_USE_MANTLE",
	"CLAUDE_CODE_SKIP_MANTLE_AUTH",
	"ANTHROPIC_VERTEX_BASE_URL",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_SKIP_VERTEX_AUTH",
	"ANTHROPIC_FOUNDRY_BASE_URL",
	"ANTHROPIC_FOUNDRY_RESOURCE",
	"CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
	"ANTHROPIC_AWS_BASE_URL",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS",
	"CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH",
	"ANTHROPIC_GOOGLE_CLOUD_BASE_URL",
	"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	"CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH",

	"HTTPS_PROXY", "https_proxy",
	"HTTP_PROXY", "http_proxy",
	"ALL_PROXY", "all_proxy",
	"NO_PROXY", "no_proxy",

	"NODE_EXTRA_CA_CERTS",
	"NODE_TLS_REJECT_UNAUTHORIZED",
	"CLAUDE_CODE_CERT_STORE",
}

// claudeRoutingSpelling pairs the proxy variables the CLI reads under either
// case. Its readers disagree on which spelling comes first and on whether an
// empty value counts (`||` against `??`), so pinning "" on the spelling a
// spawn leaves unset would turn the spawn's own proxy off for some of them. A
// pair spelled one way only is pinned to that value under both spellings.
var claudeRoutingSpelling = map[string]string{
	"HTTPS_PROXY": "https_proxy", "https_proxy": "HTTPS_PROXY",
	"HTTP_PROXY": "http_proxy", "http_proxy": "HTTP_PROXY",
	"ALL_PROXY": "all_proxy", "all_proxy": "ALL_PROXY",
	"NO_PROXY": "no_proxy", "no_proxy": "NO_PROXY",
}

// claudeRoutingPin is the routing half of the flag settings `env` block: each
// variable of ClaudeCodeRoutingEnv at the value lookup reports for the spawn's
// environment, or "" when the spawn leaves it unset. The CLI reads an empty
// routing variable as unset, and an empty value in the flag layer still
// overrides the one a lower settings source sets.
func claudeRoutingPin(lookup func(string) (string, bool)) map[string]string {
	pin := make(map[string]string, len(ClaudeCodeRoutingEnv))
	for _, key := range ClaudeCodeRoutingEnv {
		value, ok := lookup(key)
		if other, paired := claudeRoutingSpelling[key]; !ok && paired {
			value, ok = lookup(other)
		}
		if !ok {
			value = ""
		}
		pin[key] = value
	}
	return pin
}

// envListLookup reads a KEY=value list the way os/exec applies it: the last
// entry for a key wins, an entry without '=' is ignored.
func envListLookup(env []string) func(string) (string, bool) {
	values := make(map[string]string, len(env))
	for _, kv := range env {
		if key, value, ok := strings.Cut(kv, "="); ok {
			values[key] = value
		}
	}
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// claudeFlagSettingsArgs removes the `--settings` pair carrying the flag
// settings object from a spawn's argv. It matches the flag AND its value,
// the object perTaskSpawnOpts gave the spawn, so a prompt that happens to read
// "--settings" is never taken for it. A spawn without that pair is refused:
// every spawn carries it, and starting one without the routing pin is the
// failure this guards.
func claudeFlagSettingsArgs(args []string, flagSettings []byte) ([]string, error) {
	want := string(flagSettings)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--settings" && args[i+1] == want {
			return slices.Concat(args[:i], args[i+2:]), nil
		}
	}
	return nil, errors.New("claude-code: the spawn's argv does not carry its --settings object; refusing to start the CLI without the routing pin")
}

// claudeSettingsWithRouting merges the routing pin into the `env` block of the
// flag settings object. A key both define keeps the routing value.
func claudeSettingsWithRouting(flagSettings []byte, pin map[string]string) ([]byte, error) {
	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(flagSettings, &settings); err != nil {
		return nil, fmt.Errorf("claude-code: the --settings object is not a JSON object: %w", err)
	}
	env := map[string]string{}
	if raw, ok := settings["env"]; ok {
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("claude-code: the --settings env block is not a string map: %w", err)
		}
	}
	maps.Copy(env, pin)
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	settings["env"] = raw
	return json.Marshal(settings)
}

// claudeSettingsFiles holds the flag settings files of the host spawns one
// pass makes, so the pass removes them once its CLI is done.
//
// The flag settings travel as a file, never inline on argv: the routing values
// can embed a credential (a proxy's userinfo, the sandbox egress proxy's
// token, a gateway header in ANTHROPIC_CUSTOM_HEADERS), and argv is readable
// by every user of the host. The file is 0600 in a 0700 directory, readable
// by the same user only, as the process environment is. The CLI keeps the
// content it read at startup: a later write to the file does not move the
// pin, and the CLI refuses to start when the file is missing.
type claudeSettingsFiles struct {
	mu    sync.Mutex
	paths []string
	// released is set by remove: a spawn the SDK starts after its pass has
	// returned (promptWithTimeout gives up on a cancelled context without
	// waiting for it) is refused rather than leave a file behind.
	released bool
}

const claudeSettingsFileName = "settings.json"

// pinHost rewrites a host spawn so the CLI reads its flag settings from a
// private file whose `env` block also pins every routing variable at the value
// cmd.Env holds. On failure it sets cmd.Err, which Start returns: the spawn
// fails rather than run without the pin.
func (f *claudeSettingsFiles) pinHost(cmd *exec.Cmd, flagSettings []byte) {
	rest, err := claudeFlagSettingsArgs(cmd.Args[1:], flagSettings)
	if err == nil {
		var path string
		path, err = f.write(flagSettings, claudeRoutingPin(envListLookup(cmd.Env)))
		if err == nil {
			cmd.Args = slices.Concat(cmd.Args[:1], []string{"--settings", path}, rest)
			return
		}
	}
	cmd.Err = err
}

func (f *claudeSettingsFiles) write(flagSettings []byte, pin map[string]string) (string, error) {
	content, err := claudeSettingsWithRouting(flagSettings, pin)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.released {
		return "", errors.New("claude-code: the pass that owns this spawn has ended; refusing to start the CLI")
	}
	dir, err := os.MkdirTemp("", "iterion-claude-settings-")
	if err != nil {
		return "", fmt.Errorf("claude-code: create the --settings directory: %w", err)
	}
	path := filepath.Join(dir, claudeSettingsFileName)
	f.paths = append(f.paths, path)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", fmt.Errorf("claude-code: write the --settings file: %w", err)
	}
	return path, nil
}

// remove deletes every file write created, then its directory, and refuses
// any later write. It removes exactly those paths, never a tree.
func (f *claudeSettingsFiles) remove(logger *iterlog.Logger) {
	f.mu.Lock()
	paths := f.paths
	f.paths = nil
	f.released = true
	f.mu.Unlock()
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("claude-code: remove the --settings file %s: %v", path, err)
		}
		if err := os.Remove(filepath.Dir(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("claude-code: remove the --settings directory %s: %v", filepath.Dir(path), err)
		}
	}
}

// claudeFailedCmd is a command whose Start returns err.
func claudeFailedCmd(ctx context.Context, path string, err error) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path)
	cmd.Err = err
	return cmd
}

// sandboxSettingsFile is the in-container path of a sandboxed spawn's flag
// settings file. /tmp is writable in every sandbox image whatever the pinned
// User (see sandboxDelegatePIDFile).
func sandboxSettingsFile(mark string) string {
	return "/tmp/iterion-delegate-" + mark + ".settings.json"
}

// claudeSandboxArgv is the argv of a sandboxed spawn: the CLI behind a POSIX
// sh program that writes its flag settings file INSIDE the container, then
// exec's the CLI with that file. The routing values are read there, from the
// environment the CLI runs with: the container's own (the egress proxy, its
// CA, the image) plus the variables the exec forwards. None of them is known
// on the host, and none reaches an argv.
func claudeSandboxArgv(mark, path string, args []string, flagSettings []byte) ([]string, error) {
	rest, err := claudeFlagSettingsArgs(args, flagSettings)
	if err != nil {
		return nil, err
	}
	script, err := claudeRoutingScript(sandboxSettingsFile(mark), flagSettings)
	if err != nil {
		return nil, err
	}
	return slices.Concat([]string{"sh", "-c", script, "iterion-settings", path}, rest), nil
}

// claudeRoutingScript renders the sh program claudeSandboxArgv runs: it writes
// the flag settings object to file, with the routing pin claudeRoutingPin
// would compute from the program's own environment, then exec's "$1" with
// `--settings <file>` ahead of the remaining arguments. Values are JSON-escaped
// byte by byte (quote, backslash, control characters); every other byte is
// copied as is. A malformed file would make the CLI drop the whole object and
// run unpinned, so the encoding is held to the Go builder by
// TestClaudeRoutingScriptMatchesTheGoBuilder.
func claudeRoutingScript(file string, flagSettings []byte) (string, error) {
	head, tail, err := claudeSettingsAround(flagSettings)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("cli=$1; shift\n")
	b.WriteString("f=" + shellquote.Quote(file) + "\n")
	b.WriteString(`(
set -e
LC_ALL=C; export LC_ALL
umask 077
nl='
'
cr=$(printf '\r'); tab=$(printf '\t')
ctl=$(printf '\001\002\003\004\005\006\007\010\013\014\016\017\020\021\022\023\024\025\026\027\030\031\032\033\034\035\036\037')
j() {
  s=$1; o=
  while [ -n "$s" ]; do
    r=${s#?}; c=${s%"$r"}; s=$r
    case $c in
      '"') o="$o\\\"" ;;
      '\') o="$o\\\\" ;;
      "$nl") o="$o\\n" ;;
      "$cr") o="$o\\r" ;;
      "$tab") o="$o\\t" ;;
      ["$ctl"]) o="$o$(printf '\\u%04x' "'$c")" ;;
      *) o="$o$c" ;;
    esac
  done
}
rm -f "$f"
{
`)
	b.WriteString("printf '%s' " + shellquote.Quote(head) + "\n")
	for i, key := range ClaudeCodeRoutingEnv {
		b.WriteString(`if [ "${` + key + `+x}" ]; then j "$` + key + `"; `)
		if other, paired := claudeRoutingSpelling[key]; paired {
			b.WriteString(`elif [ "${` + other + `+x}" ]; then j "$` + other + `"; `)
		}
		b.WriteString("else o=; fi\n")
		sep := ","
		if i == 0 {
			sep = ""
		}
		b.WriteString("printf '" + sep + `"%s":"%s"' ` + key + ` "$o"` + "\n")
	}
	b.WriteString("printf '%s' " + shellquote.Quote(tail) + "\n")
	b.WriteString(`} > "$f"
) || { printf 'iterion: cannot write the claude settings file %s\n' "$f" >&2; exit 125; }
exec "$cli" --settings "$f" "$@"
`)
	return b.String(), nil
}

// claudeSettingsAround splits the flag settings object around the place the
// routing entries go: head opens the object and its `env` block, tail carries
// the remaining `env` entries, closes the block, then the remaining members.
// head + routing entries + tail is the object claudeSettingsWithRouting builds.
func claudeSettingsAround(flagSettings []byte) (head, tail string, err error) {
	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(flagSettings, &settings); err != nil {
		return "", "", fmt.Errorf("claude-code: the --settings object is not a JSON object: %w", err)
	}
	env := map[string]string{}
	if raw, ok := settings["env"]; ok {
		if err := json.Unmarshal(raw, &env); err != nil {
			return "", "", fmt.Errorf("claude-code: the --settings env block is not a string map: %w", err)
		}
	}
	for _, key := range ClaudeCodeRoutingEnv {
		delete(env, key)
	}
	delete(settings, "env")
	envMembers, err := jsonMembers(env)
	if err != nil {
		return "", "", err
	}
	otherMembers, err := jsonMembers(settings)
	if err != nil {
		return "", "", err
	}
	tail = ""
	if envMembers != "" {
		tail = "," + envMembers
	}
	tail += "}"
	if otherMembers != "" {
		tail += "," + otherMembers
	}
	return `{"env":{`, tail + "}", nil
}

// jsonMembers is a map marshalled as a JSON object without its braces: the
// members alone, or "" for an empty map.
func jsonMembers[V any](m map[string]V) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	raw = bytes.TrimPrefix(bytes.TrimSuffix(raw, []byte("}")), []byte("{"))
	return string(raw), nil
}
