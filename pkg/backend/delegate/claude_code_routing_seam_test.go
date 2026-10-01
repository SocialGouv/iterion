package delegate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// wantRoutingKeys is every variable each claude_code spawn must pin, spelled
// out here rather than read from ClaudeCodeRoutingEnv: a name dropped from the
// product's list must redden a test, not move both sides.
var wantRoutingKeys = []string{
	"ANTHROPIC_BASE_URL", "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
	"CLAUDE_CODE_API_BASE_URL", "CLAUDE_CODE_USE_GATEWAY",
	"ANTHROPIC_BEDROCK_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"ANTHROPIC_BEDROCK_MANTLE_BASE_URL", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_SKIP_MANTLE_AUTH",
	"ANTHROPIC_VERTEX_BASE_URL", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_SKIP_VERTEX_AUTH",
	"ANTHROPIC_FOUNDRY_BASE_URL", "ANTHROPIC_FOUNDRY_RESOURCE", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
	"ANTHROPIC_AWS_BASE_URL", "CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_SKIP_ANTHROPIC_AWS_AUTH",
	"ANTHROPIC_GOOGLE_CLOUD_BASE_URL", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "CLAUDE_CODE_SKIP_ANTHROPIC_GOOGLE_CLOUD_AUTH",
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	"NODE_EXTRA_CA_CERTS", "NODE_TLS_REJECT_UNAUTHORIZED", "CLAUDE_CODE_CERT_STORE",
}

// routingCredentialKeys never ride the routing pin.
var routingCredentialKeys = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

// The CLI reads these pairs under either spelling, so a spawn that sets one
// spelling has both pinned to its value.
var wantRoutingSpelling = map[string]string{
	"HTTPS_PROXY": "https_proxy", "https_proxy": "HTTPS_PROXY",
	"HTTP_PROXY": "http_proxy", "http_proxy": "HTTP_PROXY",
	"ALL_PROXY": "all_proxy", "all_proxy": "ALL_PROXY",
	"NO_PROXY": "no_proxy", "no_proxy": "NO_PROXY",
}

// Synthetic credentials. The proxy one is a routing value that carries a
// credential, the shape of the sandbox egress proxy URL.
const (
	routingProbeAPIKey      = "sk-ant-api03-PROBE-routing-run-key"
	routingProbeProxySecret = "PROBE-proxy-userinfo-secret"
)

// fakeClaudeRouting stands in for the CLI and records, per spawn, its argv,
// the flag settings object it reads (the file --settings names, else the
// inline value) and the routing and credential variables it runs with, then
// answers the minimum stream-json a turn needs. Values are single-line.
func fakeClaudeRouting() string {
	var dump strings.Builder
	for _, key := range append(slices.Clone(wantRoutingKeys), routingCredentialKeys...) {
		dump.WriteString(`if [ "${` + key + `+x}" ]; then printf 'env %s=[%s]\n' ` + key + ` "$` + key + `"; else printf 'env %s=<unset>\n' ` + key + `; fi >> "$ROUTING_LOG"` + "\n")
	}
	return `#!/bin/sh
{ printf '=== spawn\nargv '; printf '%s' "$*" | tr '\n' ' '; printf '\n'; } >> "$ROUTING_LOG"
settings=; prev=
for a in "$@"; do if [ "$prev" = --settings ]; then settings=$a; fi; prev=$a; done
if [ -n "$settings" ] && [ -f "$settings" ]; then
	{ printf 'settings-file '; tr '\n' ' ' < "$settings"; printf '\n'; } >> "$ROUTING_LOG"
else
	printf 'settings-inline %s\n' "$settings" >> "$ROUTING_LOG"
fi
` + dump.String() + `case "$*" in *--input-format*)
	while read -r line; do
		case "$line" in *'"type":"user"'*) break ;; esac
	done ;;
esac
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"s1"}'
`
}

// routingSpawn is one spawn as fakeClaudeRouting recorded it.
type routingSpawn struct {
	argv         string
	settingsFile bool   // --settings named a file
	settings     string // the object the CLI read
	env          map[string]string
	unset        map[string]bool
}

func readRoutingLog(t *testing.T, path string) []routingSpawn {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the stand-in CLI was never spawned: %v", err)
	}
	var spawns []routingSpawn
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case line == "=== spawn":
			spawns = append(spawns, routingSpawn{env: map[string]string{}, unset: map[string]bool{}})
		case len(spawns) == 0 || line == "":
		case strings.HasPrefix(line, "argv "):
			spawns[len(spawns)-1].argv = strings.TrimPrefix(line, "argv ")
		case strings.HasPrefix(line, "settings-file "):
			spawns[len(spawns)-1].settingsFile = true
			spawns[len(spawns)-1].settings = strings.TrimSpace(strings.TrimPrefix(line, "settings-file "))
		case strings.HasPrefix(line, "settings-inline "):
			spawns[len(spawns)-1].settings = strings.TrimPrefix(line, "settings-inline ")
		case strings.HasPrefix(line, "env "):
			key, value, _ := strings.Cut(strings.TrimPrefix(line, "env "), "=")
			if value == "<unset>" {
				spawns[len(spawns)-1].unset[key] = true
			} else {
				spawns[len(spawns)-1].env[key] = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
			}
		}
	}
	return spawns
}

// wantPin is the contract, stated from the variables the spawned process
// actually ran with: a variable it holds is pinned to that value, one it
// holds under the other spelling of a proxy pair to that spelling's value,
// any other to "".
func (s routingSpawn) wantPin(key string) string {
	if value, ok := s.env[key]; ok {
		return value
	}
	if other, paired := wantRoutingSpelling[key]; paired {
		if value, ok := s.env[other]; ok {
			return value
		}
	}
	return ""
}

// assertRoutingPinned holds one recorded spawn to the contract: one
// --settings flag naming a file, every routing variable pinned there at the
// value the process runs with, no credential in that object, none on argv.
func assertRoutingPinned(t *testing.T, label string, s routingSpawn, argvs ...string) {
	t.Helper()
	if n := strings.Count(s.argv, "--settings "); n != 1 {
		t.Errorf("%s carries %d --settings flag(s), want exactly one: %s", label, n, s.argv)
	}
	if !s.settingsFile {
		t.Errorf("%s passes its flag settings inline on argv, not in a file: %.200s", label, s.settings)
	}
	var settings struct {
		Env map[string]*string `json:"env"`
	}
	if err := json.Unmarshal([]byte(s.settings), &settings); err != nil {
		t.Fatalf("%s: the flag settings object is not JSON (%s): %v", label, s.settings, err)
	}
	for _, key := range wantRoutingKeys {
		got, pinned := settings.Env[key]
		switch {
		case !pinned || got == nil:
			t.Errorf("%s does not pin %s in the flag settings layer (process value %q): a repository's settings env could set it", label, key, s.wantPin(key))
		case *got != s.wantPin(key):
			t.Errorf("%s pins %s=%q, but the process runs with %q", label, key, *got, s.wantPin(key))
		}
	}
	for _, key := range routingCredentialKeys {
		if _, pinned := settings.Env[key]; pinned {
			t.Errorf("%s writes the credential %s into its flag settings object", label, key)
		}
	}
	for _, marker := range []string{routingProbeAPIKey, routingProbeProxySecret} {
		for i, argv := range append([]string{s.argv}, argvs...) {
			if strings.Contains(argv, marker) {
				t.Errorf("%s: argv #%d carries a credential (%s): %.300s", label, i, marker, argv)
			}
		}
	}
	if strings.Contains(s.settings, routingProbeAPIKey) {
		t.Errorf("%s: the flag settings object carries the run's API key", label)
	}
}

// clearRoutingEnv starts a test from a process environment where no routing
// or credential variable is set, restored afterwards.
func clearRoutingEnv(t *testing.T) {
	t.Helper()
	for _, key := range append(slices.Clone(wantRoutingKeys), append(routingCredentialKeys,
		"CLAUDE_CONFIG_DIR", "ZAI_API_KEY", "MOONSHOT_API_KEY", "MOONSHOT_BASE_URL", FacadeSlotEnvKey, ForfaitSuppressedEnvKey)...) {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

// Every host spawn — the Session pass and the structured-output pass that
// resumes it — reads its flag settings from a private file that pins every
// routing variable at the value the CLI process runs with. The values are
// read from the process the CLI actually is (the stand-in prints its own
// environment), so an iterion layer that rewrote one on the way would show.
// A proxy URL carrying a credential is pinned in the file and stays off argv.
func TestEverySpawnPinsTheRoutingEnvironment(t *testing.T) {
	pinWatchdogs(t)
	clearRoutingEnv(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("ANTHROPIC_API_KEY", routingProbeAPIKey)
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9/PROBE-operator-gateway")
	t.Setenv("HTTPS_PROXY", "http://user:"+routingProbeProxySecret+"@127.0.0.1:9")
	t.Setenv("no_proxy", "localhost,127.0.0.1")
	t.Setenv("NODE_EXTRA_CA_CERTS", "/etc/ssl/PROBE-operator-ca.pem")

	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte(fakeClaudeRouting()), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "routing.log")
	t.Setenv("ROUTING_LOG", log)

	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
	_, err := b.Execute(context.Background(), Task{NodeID: "n", Command: script, WorkDir: dir, UserPrompt: "x", OutputSchema: []byte(schemaOK)})
	t.Logf("Execute: %v", err)

	spawns := readRoutingLog(t, log)
	if len(spawns) < 2 {
		t.Fatalf("expected the Session spawn and the formatting pass, got %d spawn(s)", len(spawns))
	}
	for i, s := range spawns {
		assertRoutingPinned(t, "spawn #"+string(rune('1'+i)), s)
	}
	// The values the contract was checked against are the ones set above:
	// the stand-in did see them, and the unset ones are pinned empty.
	first := spawns[0]
	if first.env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:9/PROBE-operator-gateway" || !first.unset["CLAUDE_CODE_USE_BEDROCK"] {
		t.Fatalf("the stand-in did not run with the environment the test set: %v unset=%v", first.env, first.unset)
	}
	// Written under the process temp dir, and removed once the pass is done.
	if !strings.Contains(spawns[0].argv, "--settings "+tmp+string(filepath.Separator)+"iterion-claude-settings-") {
		t.Errorf("the flag settings file is not a private temp file: %s", spawns[0].argv)
	}
	if matches, _ := filepath.Glob(filepath.Join(tmp, "iterion-claude-settings-*")); len(matches) != 0 {
		t.Errorf("flag settings left behind after the passes: %v", matches)
	}
}

// execSandboxRun is a sandbox double that RUNS what it is given, on the host,
// with a container-like environment under the forwarded one — the way
// `docker exec --env` layers them — so the in-container program really
// executes. A command that is not a CLI spawn (the pidfile reaper) is
// recorded and answered by `true`.
type execSandboxRun struct {
	sandbox.Run
	container []string
	workdir   string
	mu        sync.Mutex
	argvs     [][]string
}

func (r *execSandboxRun) Driver() string { return "docker" }

func (r *execSandboxRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	r.mu.Lock()
	r.argvs = append(r.argvs, slices.Clone(argv))
	r.mu.Unlock()
	if !slices.Contains(argv, "--print") {
		return exec.CommandContext(ctx, "true")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = slices.Clone(r.container)
	for key, value := range opts.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Dir = r.workdir
	return cmd
}

func (r *execSandboxRun) hostArgvs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.argvs))
	for i, argv := range r.argvs {
		out[i] = strings.Join(argv, " ")
	}
	return out
}

// Sandboxed spawns pin the values the CLI runs with INSIDE the container:
// the egress proxy and its CA the driver set at container creation, which
// the host never sees, and the variables the exec forwards. A host-side pin
// would blank the proxy. The proxy URL carries the egress token: it reaches
// neither the exec's argv nor the CLI's.
func TestSandboxedSpawnsPinTheRoutingEnvironmentInsideTheContainer(t *testing.T) {
	pinWatchdogs(t)
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", routingProbeAPIKey)
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte(fakeClaudeRouting()), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "routing.log")
	container := []string{
		"PATH=" + os.Getenv("PATH"),
		"ROUTING_LOG=" + log,
		"HTTPS_PROXY=http://t:" + routingProbeProxySecret + "@host.docker.internal:3128",
		"HTTP_PROXY=http://t:" + routingProbeProxySecret + "@host.docker.internal:3128",
		"NO_PROXY=localhost,127.0.0.1,host.docker.internal",
		"NODE_EXTRA_CA_CERTS=/run/iterion/egress-ca.pem",
	}
	for _, formatting := range []bool{false, true} {
		run := &execSandboxRun{container: container, workdir: dir}
		task := Task{NodeID: "n", Command: script, UserPrompt: "x", Sandbox: run, OutputSchema: []byte(schemaOK)}
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
		if err := os.WriteFile(log, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if formatting {
			_, _, err := b.formatOutput(context.Background(), task, "sid")
			t.Logf("formatOutput: %v", err)
		} else {
			_, err := b.Execute(context.Background(), task)
			t.Logf("Execute: %v", err)
		}
		spawns := readRoutingLog(t, log)
		if len(spawns) == 0 {
			t.Fatalf("formatting=%v: no CLI spawn ran in the sandbox (execs: %v)", formatting, run.hostArgvs())
		}
		for i, s := range spawns {
			label := "formatting=" + map[bool]string{false: "false", true: "true"}[formatting] + " spawn #" + string(rune('1'+i))
			assertRoutingPinned(t, label, s, run.hostArgvs()...)
			if s.wantPin("https_proxy") != "http://t:"+routingProbeProxySecret+"@host.docker.internal:3128" ||
				s.wantPin("NODE_EXTRA_CA_CERTS") != "/run/iterion/egress-ca.pem" {
				t.Errorf("%s did not run with the container's proxy and CA (env %v)", label, s.env)
			}
		}
	}
}
