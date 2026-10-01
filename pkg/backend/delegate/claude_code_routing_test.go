package delegate

import (
	"encoding/json"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The sh program a sandboxed spawn runs must write the very object the Go
// builder computes from the same environment: a malformed file makes the CLI
// drop the whole flag layer and run unpinned. Every shell on the machine,
// adversarial values (quotes, backslashes, control characters, multi-byte
// text, shell metacharacters, unset against empty, pairs spelled one way),
// then random bytes.
func TestClaudeRoutingScriptMatchesTheGoBuilder(t *testing.T) {
	var shells [][]string
	for _, shell := range [][]string{{"sh"}, {"dash"}, {"bash", "--posix"}, {"busybox", "sh"}} {
		if _, err := exec.LookPath(shell[0]); err == nil {
			shells = append(shells, shell)
		}
	}
	if len(shells) == 0 || shells[0][0] != "sh" {
		t.Fatal("no sh on PATH: the sandbox wrapper cannot be checked")
	}
	dir := t.TempDir()
	cli := filepath.Join(dir, "cli")
	// The wrapper exec's `cli --settings <file> ARGS…`: print the file.
	if err := os.WriteFile(cli, []byte("#!/bin/sh\ncat \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	flagSettings := []byte(`{"autoMemoryDirectory":"/mem/it's \"here\"","autoMemoryEnabled":true,"env":{"BASH_MAX_TIMEOUT_MS":"810000","CLAUDE_CODE_DISABLE_BACKGROUND_TASKS":"1"}}`)

	cases := []map[string]string{
		{},
		{"ANTHROPIC_BASE_URL": "https://gw.example/v1?a=1&b=<2>", "HTTPS_PROXY": "http://t:tok@h:3128"},
		{"https_proxy": "http://lower-only:1", "NO_PROXY": "", "no_proxy": "localhost"},
		{"ANTHROPIC_CUSTOM_HEADERS": "X-One: 1\nX-Two: \"two\"\r\n\tX-Three: \\three\\"},
		{"ANTHROPIC_BASE_URL": "\x01\x02\x1f\x7f é ünï 🚀 $HOME `id` $(id) * ? [a-z] %s %% ' ; | & > <"},
		{"NODE_TLS_REJECT_UNAUTHORIZED": "0", "CLAUDE_CODE_USE_BEDROCK": "1", "ANTHROPIC_BEDROCK_BASE_URL": "\\\\\\\""},
		{"HTTP_PROXY": "upper", "http_proxy": "lower"},
	}
	rng := rand.New(rand.NewSource(1))
	for range 60 {
		c := map[string]string{}
		for range 1 + rng.Intn(6) {
			key := ClaudeCodeRoutingEnv[rng.Intn(len(ClaudeCodeRoutingEnv))]
			value := make([]byte, rng.Intn(24))
			for i := range value {
				value[i] = byte(1 + rng.Intn(255))
			}
			c[key] = string(value)
		}
		cases = append(cases, c)
	}

	file := filepath.Join(dir, "settings.json")
	script, err := claudeRoutingScript(file, flagSettings)
	if err != nil {
		t.Fatal(err)
	}
	for _, shell := range shells {
		for i, c := range cases {
			env := []string{"PATH=" + os.Getenv("PATH")}
			for key, value := range c {
				env = append(env, key+"="+value)
			}
			want, err := claudeSettingsWithRouting(flagSettings, claudeRoutingPin(envListLookup(env)))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(shell[0], append(shell[1:], "-c", script, "iterion-settings", cli, "--print")...)
			cmd.Env = env
			got, err := cmd.Output()
			if err != nil {
				t.Fatalf("%v case %d: the wrapper failed: %v", shell, i, err)
			}
			if !json.Valid(got) {
				t.Fatalf("%v case %d: the wrapper wrote invalid JSON — the CLI would drop the whole flag layer: %q (env %q)", shell, i, got, c)
			}
			var gotObj, wantObj map[string]any
			if err := json.Unmarshal(got, &gotObj); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(want, &wantObj); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotObj, wantObj) {
				t.Fatalf("%v case %d: the wrapper wrote\n%s\nthe Go builder computes\n%s\n(env %q)", shell, i, got, want, c)
			}
		}
	}
}

// The builder only ever takes the flag settings object perTaskSpawnOpts
// handed the spawn — matched by flag AND value — and refuses a spawn that
// carries none: starting the CLI without the routing pin is the failure.
func TestClaudeFlagSettingsArgs(t *testing.T) {
	settings := []byte(`{"env":{"A":"1"}}`)
	rest, err := claudeFlagSettingsArgs([]string{"--print", "--append-system-prompt", "--settings", "--settings", string(settings), "prompt"}, settings)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--print", "--append-system-prompt", "--settings", "prompt"}; !slices.Equal(rest, want) {
		t.Errorf("rest = %q, want %q", rest, want)
	}
	if _, err := claudeFlagSettingsArgs([]string{"--print", "--settings", `{"env":{}}`}, settings); err == nil {
		t.Error("a spawn without its own flag settings object was accepted")
	}

	var files claudeSettingsFiles
	cmd := exec.Command("true", "--print")
	files.pinHost(cmd, settings)
	if cmd.Err == nil {
		t.Fatal("pinHost accepted a spawn without its --settings object")
	}
	if err := cmd.Start(); err == nil || !strings.Contains(err.Error(), "routing pin") {
		t.Errorf("Start() = %v, want the refusal", err)
	}
}

// A host spawn's flag settings file is readable by its owner only, carries
// the routing pin computed from the spawn's own environment, and is gone once
// the pass releases it; a spawn started after that is refused instead of
// leaving a file behind.
func TestClaudeSettingsFilesLifecycle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	settings := []byte(`{"env":{"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS":"1"}}`)
	var files claudeSettingsFiles
	cmd := exec.Command("true", "--print", "--settings", string(settings))
	cmd.Env = []string{"HTTPS_PROXY=http://u:p@proxy:3128", "HTTPS_PROXY=http://later:1"}
	files.pinHost(cmd, settings)
	if cmd.Err != nil {
		t.Fatal(cmd.Err)
	}
	path := cmd.Args[2]
	if cmd.Args[1] != "--settings" || !strings.HasPrefix(path, tmp) {
		t.Fatalf("argv = %q, want --settings <a file under %s>", cmd.Args, tmp)
	}
	for _, check := range []struct {
		path string
		want os.FileMode
	}{{path, 0o600}, {filepath.Dir(path), 0o700 | os.ModeDir}} {
		info, err := os.Stat(check.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != check.want {
			t.Errorf("%s mode = %v, want %v", check.path, info.Mode(), check.want)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Env map[string]string }
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Env["HTTPS_PROXY"] != "http://later:1" || got.Env["https_proxy"] != "http://later:1" ||
		got.Env["ANTHROPIC_BASE_URL"] != "" || got.Env["CLAUDE_CODE_DISABLE_BACKGROUND_TASKS"] != "1" {
		t.Errorf("settings env = %v: want the last HTTPS_PROXY under both spellings, the unset base URL pinned empty, the static pin kept", got.Env)
	}

	files.remove(iterlog.Nop())
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("left behind after remove: %v", entries)
	}
	late := exec.Command("true", "--print", "--settings", string(settings))
	files.pinHost(late, settings)
	if late.Err == nil {
		t.Error("a spawn started after the pass released its files was accepted")
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("a refused spawn still wrote %v", entries)
	}
}
