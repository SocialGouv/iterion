package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// hostForfaitBlob is the HOST's ChatGPT-mode Codex auth.json — what
// `codex login` writes for "Sign in with ChatGPT": the API key it exchanged
// the id token for, the id token, the access token, the refresh token.
const hostForfaitBlob = `{"auth_mode":"chatgpt","OPENAI_API_KEY":"sk-proj-host-exchanged","tokens":{"id_token":"idt-host","access_token":"tok-host","refresh_token":"rt-host","account_id":"acct-host"},"last_refresh":"2026-10-11T09:00:00.123456789Z"}`

// The other shapes a host's codex home can be in.
const (
	hostAPIKeyModeBlob = `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-in-file"}`
	hostMalformedBlob  = `{"auth_mode":"chatgpt",`
)

// operatorEnv starts a test from an environment the operator set entirely:
// nothing a project `.env` planted, whatever the process running the tests
// inherited.
func operatorEnv(t *testing.T) {
	t.Helper()
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	// The crossing probes `codex --version` on the host; a test runs none.
	orig := hostCodexVersion
	t.Cleanup(func() { hostCodexVersion = orig })
	hostCodexVersion = func() string { return "" }
}

// hostCodexHome points CODEX_HOME — the host's codex home, as the in-process
// factory resolves it — at a fresh dir whose auth.json is blob ("" = none),
// and returns the dir.
func hostCodexHome(t *testing.T, blob string) string {
	t.Helper()
	operatorEnv(t)
	dir := t.TempDir()
	if blob != "" {
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(blob), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", dir)
	return dir
}

// deliverySandbox is a container sandbox whose filesystem is a directory of
// the test's own: the `sh -c <script> sh <path>` execs the launcher runs in it
// — sandbox.WriteFileExec's write, the delivery's removal — run for real, with
// the container path rooted there. It records every exec, and the env and the
// files each runner command starts with. The runner exits at once without
// answering, which fails the dispatch after the launcher has done everything
// it does before one.
type deliverySandbox struct {
	root      string
	driver    string
	failWrite bool
	onCommand func()

	mu       sync.Mutex
	execs    [][]string
	runEnvs  []map[string]string
	runFiles []map[string][]byte
}

func newDeliverySandbox(t *testing.T) *deliverySandbox {
	return &deliverySandbox{root: t.TempDir()}
}

func (s *deliverySandbox) Driver() string {
	if s.driver == "" {
		return "docker"
	}
	return s.driver
}

func (s *deliverySandbox) Command(ctx context.Context, _ []string, opts sandbox.ExecOpts) *exec.Cmd {
	files := s.files()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runEnvs = append(s.runEnvs, maps.Clone(opts.Env))
	s.runFiles = append(s.runFiles, files)
	if s.onCommand != nil {
		s.onCommand()
	}
	return exec.CommandContext(ctx, "true")
}

func (s *deliverySandbox) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	s.mu.Lock()
	s.execs = append(s.execs, slices.Clone(cmd))
	s.mu.Unlock()
	if len(cmd) != 5 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[3] != "sh" || !path.IsAbs(cmd[4]) || path.Clean(cmd[4]) != cmd[4] || cmd[4] == "/" {
		return sandbox.ExecResult{ExitCode: 127, Stderr: []byte("deliverySandbox: unexpected exec")}, nil
	}
	if s.failWrite && opts.Stdin != nil {
		// A write that stopped half-way: its temp file is left behind.
		partial := filepath.Join(s.root, cmd[4]) + ".tmp"
		if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
			return sandbox.ExecResult{}, err
		}
		if err := os.WriteFile(partial, []byte(`{"auth_mode":"chat`), 0o600); err != nil {
			return sandbox.ExecResult{}, err
		}
		return sandbox.ExecResult{ExitCode: 1, Stderr: []byte("no space left on device")}, nil
	}
	run := exec.CommandContext(ctx, "sh", "-c", cmd[2], "sh", filepath.Join(s.root, cmd[4]))
	run.Stdin = opts.Stdin
	var stderr bytes.Buffer
	run.Stderr = &stderr
	err := run.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return sandbox.ExecResult{ExitCode: exit.ExitCode(), Stderr: stderr.Bytes()}, nil
	}
	return sandbox.ExecResult{Stderr: stderr.Bytes()}, err
}

func (s *deliverySandbox) Cleanup(context.Context) error { return nil }

// files returns the container's files now, by container path.
func (s *deliverySandbox) files() map[string][]byte {
	out := map[string][]byte{}
	_ = filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, rerr := os.ReadFile(p); rerr == nil {
			out["/"+filepath.ToSlash(strings.TrimPrefix(p, s.root+string(filepath.Separator)))] = data
		}
		return nil
	})
	return out
}

// hostRunSandbox is the noop driver's shape: its commands run on the host.
type hostRunSandbox struct{ *deliverySandbox }

func (s hostRunSandbox) Driver() string             { return "noop" }
func (s hostRunSandbox) HostExecutesCommands() bool { return true }

// hidingRun wraps a run the way a subbot child's PATH wrapper does: only the
// sandbox.Run methods cross, every optional capability is lost.
type hidingRun struct{ sandbox.Run }

// dispatchToSandbox sends one claw task for model through the real sandbox
// dispatch into run, recorded by rec, and returns the env the runner started
// with (nil when the dispatch never started one) and the dispatch's error.
func dispatchToSandbox(t *testing.T, ctx context.Context, b *ClawBackend, run sandbox.Run, rec *deliverySandbox, model string) (map[string]string, error) {
	t.Helper()
	_, err := b.executeViaSandboxRunner(ctx, delegate.Task{
		NodeID: "n", Model: model, WorkDir: t.TempDir(), Sandbox: run,
	})
	if len(rec.runEnvs) == 0 {
		return nil, err
	}
	return rec.runEnvs[len(rec.runEnvs)-1], err
}

func newTestClawBackend() *ClawBackend {
	return NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
}

// runnerEnvOf dispatches and fails the test when no runner was started.
func runnerEnvOf(t *testing.T, ctx context.Context, box *deliverySandbox, model string) map[string]string {
	t.Helper()
	env, err := dispatchToSandbox(t, ctx, newTestClawBackend(), box, box, model)
	if env == nil {
		t.Fatalf("the dispatch started no runner: %v", err)
	}
	return env
}

// runCredentials are the shapes of the run's own credentials the tests walk:
// none at all (a local bot without a secrets: block), a scope holding no
// OpenAI instrument (a local bot with one), and each OpenAI instrument a run
// can hold.
func runCredentials(t *testing.T) map[string]func() context.Context {
	return map[string]func() context.Context{
		"none": context.Background,
		"generic only": func() context.Context {
			return secrets.WithCredentials(context.Background(), secrets.Credentials{
				Generic: map[string]string{"forge_token": "ghp-fixture"},
			})
		},
		"own key": func() context.Context {
			return secrets.WithCredentials(context.Background(), secrets.Credentials{
				APIKeys: map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-run"},
			})
		},
		"pinned key": func() context.Context {
			return secrets.WithCredentials(context.Background(), secrets.Credentials{
				PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-pinned"},
			})
		},
		"own forfait": func() context.Context {
			return secrets.WithCredentials(context.Background(), secrets.Credentials{
				OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindCodex): writeCodexAuth(t)},
			})
		},
		// A connected forfait that cannot serve (not a ChatGPT login): in
		// process the run falls through to the disk factory, as with none.
		"own forfait unusable": func() context.Context {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(hostAPIKeyModeBlob), 0o600); err != nil {
				t.Fatal(err)
			}
			return secrets.WithCredentials(context.Background(), secrets.Credentials{
				OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindCodex): dir},
			})
		},
	}
}

// 1. ITERION_OPENAI_USE_OAUTH=1 makes the host's ChatGPT forfait claw's
// instrument over OPENAI_API_KEY in process. A sandboxed node of a run holding
// no OpenAI credential of its own gets that same forfait: when its runner
// starts, the container holds the host's login in the dir CODEX_HOME names —
// never its refresh token — and the OAuth path is forced over the key that
// crosses beside it.
func TestSandboxedClawNodeGetsTheHostForfaitTheSwitchOrders(t *testing.T) {
	for _, tc := range []struct{ creds, driver string }{
		{"none", "docker"}, {"generic only", "docker"}, {"none", "podman"}, {"none", "kubernetes"},
	} {
		t.Run(tc.creds+"/"+tc.driver, func(t *testing.T) {
			name := tc.creds
			host := hostCodexHome(t, hostForfaitBlob)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
			t.Setenv("OPENAI_API_KEY", "sk-env")
			t.Setenv("OPENAI_BASE_URL", "")
			box := newDeliverySandbox(t)
			box.driver = tc.driver
			box.onCommand = func() {
				// What the runner can reach, while it runs: the copy.
				for p, data := range box.files() {
					for _, secret := range []string{"rt-host", "idt-host", "sk-proj-host-exchanged"} {
						if bytes.Contains(data, []byte(secret)) {
							t.Errorf("%s carries %q into the container while the node runs — a credential the runner never spends", p, secret)
						}
					}
				}
			}
			env := runnerEnvOf(t, runCredentials(t)[name](), box, "openai/gpt-6")

			if env["ITERION_OPENAI_USE_OAUTH"] != "1" {
				t.Error("the forfait is not forced in the container — its runner spends the OPENAI_API_KEY the host's switch overrides in process")
			}
			home := env["CODEX_HOME"]
			if home == "" {
				t.Fatal("no CODEX_HOME crossed — the runner has no forfait to spend")
			}
			atStart := box.runFiles[len(box.runFiles)-1]
			delivered, ok := atStart[path.Join(home, "auth.json")]
			if !ok {
				t.Fatalf("CODEX_HOME=%s names no auth.json the container held when the runner started (it held: %v) — the runner is pointed at a forfait that is not there", home, slices.Sorted(maps.Keys(atStart)))
			}
			got, err := secrets.ParseCodexView(delivered)
			if err != nil {
				t.Fatalf("the auth.json the runner found does not parse: %v", err)
			}
			want, err := secrets.LoadCodexCredentialsFrom(host)
			if err != nil {
				t.Fatal(err)
			}
			if !got.IsChatGPTMode() || got.Tokens.AccessToken != want.Tokens.AccessToken || got.Tokens.AccountID != want.Tokens.AccountID {
				t.Error("the auth.json the runner found is not the host's ChatGPT login")
			}
			assertAccessTokenLoginOnly(t, delivered)
			if env["OPENAI_API_KEY"] != "sk-env" {
				t.Error("OPENAI_API_KEY did not cross — the runner loses the fallback the in-process factory keeps")
			}
		})
	}
}

// assertAccessTokenLoginOnly fails unless login is the auth mode, the access
// token and the account, and nothing else: whatever else a codex login holds
// (its refresh token, its id token, the API key it exchanged one for) is a
// credential the runner never spends.
func assertAccessTokenLoginOnly(t *testing.T, login []byte) {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(login, &doc); err != nil {
		t.Fatalf("the copy does not parse: %v", err)
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(doc["tokens"], &tokens); err != nil {
		t.Fatalf("the copy's tokens do not parse: %v", err)
	}
	if keys := slices.Sorted(maps.Keys(doc)); !slices.Equal(keys, []string{"auth_mode", "tokens"}) {
		t.Errorf("the copy carries %v, want only auth_mode and tokens", keys)
	}
	if keys := slices.Sorted(maps.Keys(tokens)); !slices.Equal(keys, []string{"access_token", "account_id"}) {
		t.Errorf("the copy's tokens carry %v, want only access_token and account_id", keys)
	}
}

// The copy is the login the host judged, read once: a codex login spelled
// in ways one JSON reader accepts and another reads differently — a key in
// another case, a second tokens object — still yields the access token the
// in-process factory spends, and none of the rest.
func TestTheCopyIsTheLoginTheHostJudged(t *testing.T) {
	for _, tc := range []struct{ name, blob string }{
		{"a refresh token in another case", `{"auth_mode":"chatgpt","tokens":{"access_token":"tok-host","account_id":"acct-host","Refresh_Token":"rt-case"}}`},
		{"tokens in another case", `{"auth_mode":"chatgpt","TOKENS":{"access_token":"tok-host","account_id":"acct-host","refresh_token":"rt-case"}}`},
		{"a second tokens object", `{"auth_mode":"chatgpt","tokens":{"access_token":"tok-host","account_id":"acct-host"},"tokens":{"refresh_token":"rt-dup"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostCodexHome(t, tc.blob)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
			t.Setenv("OPENAI_API_KEY", "sk-env")
			t.Setenv("OPENAI_BASE_URL", "")
			hostClient, hostErr := NewRegistry().ResolveWithContext(context.Background(), "openai/gpt-6")
			inProcess := spendOf(t, hostClient, hostErr)
			if inProcess.token != "tok-host" {
				t.Fatalf("in process %s, want the host's forfait — the fixture does not set up what it claims", inProcess)
			}
			box := newDeliverySandbox(t)
			env := runnerEnvOf(t, context.Background(), box, "openai/gpt-6")
			atStart := box.runFiles[len(box.runFiles)-1]
			login, ok := atStart[path.Join(env["CODEX_HOME"], "auth.json")]
			if !ok {
				t.Fatalf("no copy when the runner started (CODEX_HOME=%q)", env["CODEX_HOME"])
			}
			assertAccessTokenLoginOnly(t, login)
			if inContainer := resolveInTheContainer(t, "openai/gpt-6", env, atStart); inContainer != inProcess {
				t.Errorf("in process %s, in the container %s", inProcess, inContainer)
			}
		})
	}
}

// 2. The run's own OpenAI instrument outranks the host's forfait, as it does in
// process: a run holding its own key, a key pinned for its route, or its own
// ChatGPT forfait never gets the host's — no subscription the run did not
// choose is drained beside it.
func TestTheRunsOwnOpenAIInstrumentKeepsTheHostForfaitHome(t *testing.T) {
	for _, name := range []string{"own key", "pinned key", "own forfait"} {
		t.Run(name, func(t *testing.T) {
			hostCodexHome(t, hostForfaitBlob)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
			t.Setenv("OPENAI_API_KEY", "sk-env")
			t.Setenv("OPENAI_BASE_URL", "")
			box := newDeliverySandbox(t)
			env := runnerEnvOf(t, runCredentials(t)[name](), box, "openai/gpt-6")

			if len(box.execs) != 0 {
				t.Errorf("the dispatch ran %q in the container — the host's forfait went to a run holding its own instrument", box.execs)
			}
			if home := env["CODEX_HOME"]; home != "" && home != secrets.CodexSandboxConfigDir {
				t.Errorf("CODEX_HOME=%s, want none or the run's own forfait dir %s", home, secrets.CodexSandboxConfigDir)
			}
		})
	}
}

// 3. ITERION_OPENAI_USE_OAUTH=0 refuses every subscription, and the host's
// forfait is no exception: whatever the run holds, the refusal crosses and
// nothing of the host's codex home is written into the container.
func TestTheKillSwitchKeepsTheHostForfaitHomeWhateverTheRunHolds(t *testing.T) {
	for name, ctxOf := range runCredentials(t) {
		for _, envKey := range []string{"", "sk-env"} {
			t.Run(fmt.Sprintf("%s/env key %v", name, envKey != ""), func(t *testing.T) {
				hostCodexHome(t, hostForfaitBlob)
				t.Setenv("ITERION_OPENAI_USE_OAUTH", "0")
				t.Setenv("OPENAI_API_KEY", envKey)
				t.Setenv("OPENAI_BASE_URL", "")
				box := newDeliverySandbox(t)
				env := runnerEnvOf(t, ctxOf(), box, "openai/gpt-6")

				if env["ITERION_OPENAI_USE_OAUTH"] != "0" {
					t.Errorf("ITERION_OPENAI_USE_OAUTH=%q in the container, want the refusal", env["ITERION_OPENAI_USE_OAUTH"])
				}
				if len(box.execs) != 0 {
					t.Errorf("the dispatch ran %q in the container under the kill switch", box.execs)
				}
			})
		}
	}
}

// 4. A forfait the host does not hold is not made up: with no auth.json, an
// unreadable one, a malformed one or one in API-key mode, the in-process
// factory spends OPENAI_API_KEY, and so does the container — nothing is
// written into it, nothing forces the OAuth path, and the dispatch proceeds.
func TestNoHostForfaitNothingIsDelivered(t *testing.T) {
	for _, tc := range []struct {
		name string
		home func(t *testing.T)
	}{
		{"no auth.json", func(t *testing.T) { hostCodexHome(t, "") }},
		{"unreadable auth.json", func(t *testing.T) {
			dir := hostCodexHome(t, "")
			if err := os.Mkdir(filepath.Join(dir, "auth.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"malformed auth.json", func(t *testing.T) { hostCodexHome(t, hostMalformedBlob) }},
		{"API-key mode", func(t *testing.T) { hostCodexHome(t, hostAPIKeyModeBlob) }},
		{"API-key mode holding tokens", func(t *testing.T) {
			hostCodexHome(t, `{"auth_mode":"apikey","tokens":{"access_token":"tok-k","refresh_token":"rt-k","account_id":"acct-k"}}`)
		}},
		{"a ChatGPT login without its account", func(t *testing.T) {
			hostCodexHome(t, `{"auth_mode":"chatgpt","tokens":{"access_token":"tok-x","refresh_token":"rt-x"}}`)
		}},
		{"no codex home", func(t *testing.T) {
			hostCodexHome(t, "")
			t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "absent"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.home(t)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
			t.Setenv("OPENAI_API_KEY", "sk-env")
			t.Setenv("OPENAI_BASE_URL", "")
			box := newDeliverySandbox(t)
			env := runnerEnvOf(t, context.Background(), box, "openai/gpt-6")

			if len(box.execs) != 0 {
				t.Errorf("the dispatch ran %q in the container with no forfait on the host", box.execs)
			}
			if home := env["CODEX_HOME"]; home != "" {
				t.Errorf("CODEX_HOME=%s crossed with no forfait on the host — the runner is pointed at whatever that dir holds", home)
			}
			if v, forced := env["ITERION_OPENAI_USE_OAUTH"]; forced {
				t.Errorf("ITERION_OPENAI_USE_OAUTH=%q crossed with no forfait to force", v)
			}
			if env["OPENAI_API_KEY"] != "sk-env" {
				t.Error("OPENAI_API_KEY did not cross — the key the in-process factory falls back on is not the container's")
			}
		})
	}
}

// openAISpend is what a resolved claw openai client spends: nothing (the
// resolution failed), an API key, or a forfait's OAuth token.
type openAISpend struct {
	failed     bool
	key, token string
}

func spendOf(t *testing.T, client any, err error) openAISpend {
	t.Helper()
	if err != nil {
		return openAISpend{failed: true}
	}
	key, token := openAIClientAuth(t, client)
	return openAISpend{key: key, token: token}
}

// String names the fixtures only: a wrong resolution may have read a real
// credential.
func (s openAISpend) String() string {
	fixture := func(v string) string {
		switch v {
		case "", "sk-env", "sk-run", "sk-pinned", "sk-in-file", "tok-host", "tok-abc":
			return v
		}
		return "<another credential>"
	}
	return fmt.Sprintf("{failed=%v key=%q token=%q}", s.failed, fixture(s.key), fixture(s.token))
}

// resolveInTheContainer resolves model as the in-container runner does, from
// what the launcher handed the sandbox: the env the runner started with, and
// the container's files at that moment — the run's own forfait sandbox start
// seeded at secrets.CodexSandboxAuthPath, and whatever the dispatch wrote. The
// container's disk holds no other forfait.
func resolveInTheContainer(t *testing.T, model string, runnerEnv map[string]string, files map[string][]byte) openAISpend {
	t.Helper()
	for _, name := range []string{"OPENAI_API_KEY", "ITERION_OPENAI_USE_OAUTH", "OPENAI_BASE_URL"} {
		t.Setenv(name, runnerEnv[name])
	}
	home := t.TempDir()
	if dir := runnerEnv["CODEX_HOME"]; dir != "" {
		if blob, ok := files[path.Join(dir, "auth.json")]; ok {
			if err := os.WriteFile(filepath.Join(home, "auth.json"), blob, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("CODEX_HOME", home)
	client, err := NewRegistry().Resolve(model)
	return spendOf(t, client, err)
}

// 5. In process and in the container, a claw `openai/…` node spends the SAME
// instrument for every combination of the operator's explicit switch (the
// order "1", the refusal "0"), OPENAI_API_KEY, the host's codex home, the
// run's own credentials and an OPENAI_BASE_URL — the container rebuilt from
// exactly what the real dispatch hands it, on a disk holding no forfait of
// its own. The run's credentials are read as a cloud runner reads them.
//
// The unset switch stays out of this matrix, deliberately: without it a host
// holding no key spends its forfait in process, and a container reaches it
// only through host_state's bind of ~/.codex — the operator never ordered the
// forfait into the sandbox, so this change does not ship it there.
func TestSandboxedClawSpendsWhatTheHostSpends(t *testing.T) {
	for _, useOAuth := range []string{"1", "0"} {
		for _, envKey := range []string{"", "sk-env"} {
			for _, home := range []string{"chatgpt", "none", "apikey", "malformed"} {
				for _, credsName := range []string{"none", "generic only", "own key", "pinned key", "own forfait", "own forfait unusable"} {
					for _, gateway := range []bool{false, true} {
						name := fmt.Sprintf("oauth=%s/env key=%v/host=%s/run=%s/gateway=%v", useOAuth, envKey != "", home, credsName, gateway)
						t.Run(name, func(t *testing.T) {
							installRunLookups(t)
							switch home {
							case "chatgpt":
								hostCodexHome(t, hostForfaitBlob)
							case "none":
								hostCodexHome(t, "")
							case "apikey":
								hostCodexHome(t, hostAPIKeyModeBlob)
							case "malformed":
								hostCodexHome(t, hostMalformedBlob)
							}
							t.Setenv("ITERION_OPENAI_USE_OAUTH", useOAuth)
							t.Setenv("OPENAI_API_KEY", envKey)
							baseURL := ""
							if gateway {
								baseURL = "http://gateway.parity.example/v1"
							}
							t.Setenv("OPENAI_BASE_URL", baseURL)
							ctx := runCredentials(t)[credsName]()

							hostClient, hostErr := NewRegistry().ResolveWithContext(ctx, "openai/gpt-6")
							inProcess := spendOf(t, hostClient, hostErr)

							box := newDeliverySandbox(t)
							env := runnerEnvOf(t, ctx, box, "openai/gpt-6")
							files := maps.Clone(box.runFiles[len(box.runFiles)-1])
							if creds, ok := secrets.CredentialsFromContext(ctx); ok {
								if dir := creds.OAuthDir(string(secrets.OAuthKindCodex)); dir != "" {
									blob, err := os.ReadFile(filepath.Join(dir, "auth.json"))
									if err != nil {
										t.Fatal(err)
									}
									files[secrets.CodexSandboxAuthPath] = blob
								}
							}
							inContainer := resolveInTheContainer(t, "openai/gpt-6", env, files)

							if inProcess != inContainer {
								t.Errorf("in process %s, in the container %s", inProcess, inContainer)
							}
						})
					}
				}
			}
		}
	}
}

// The local twin of the parity: no runner lookups, so the in-process factory
// reads no run credentials at all — the desktop, the CLI, a local studio.
func TestSandboxedClawSpendsWhatTheHostSpendsLocally(t *testing.T) {
	for _, envKey := range []string{"", "sk-env"} {
		for _, credsName := range []string{"none", "generic only"} {
			t.Run(fmt.Sprintf("env key=%v/run=%s", envKey != "", credsName), func(t *testing.T) {
				hostCodexHome(t, hostForfaitBlob)
				t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
				t.Setenv("OPENAI_API_KEY", envKey)
				t.Setenv("OPENAI_BASE_URL", "")
				ctx := runCredentials(t)[credsName]()

				hostClient, hostErr := NewRegistry().ResolveWithContext(ctx, "openai/gpt-6")
				inProcess := spendOf(t, hostClient, hostErr)
				if inProcess.token != "tok-host" {
					t.Fatalf("in process %s, want the host's forfait — the fixture does not set up what it claims", inProcess)
				}
				box := newDeliverySandbox(t)
				env := runnerEnvOf(t, ctx, box, "openai/gpt-6")
				if inContainer := resolveInTheContainer(t, "openai/gpt-6", env, box.runFiles[len(box.runFiles)-1]); inContainer != inProcess {
					t.Errorf("in process %s, in the container %s", inProcess, inContainer)
				}
			})
		}
	}
}

// An OPENAI_BASE_URL keeps a forfait off a third-party endpoint on both sides,
// so the container never needs the host's: nothing is written into it, where
// a copy would only sit beside the gateway key it cannot replace.
func TestAGatewayKeepsTheHostForfaitHome(t *testing.T) {
	hostCodexHome(t, hostForfaitBlob)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv("OPENAI_BASE_URL", "http://gateway.parity.example/v1")
	box := newDeliverySandbox(t)
	env := runnerEnvOf(t, context.Background(), box, "openai/gpt-6")
	if len(box.execs) != 0 {
		t.Errorf("the dispatch ran %q in the container of a gateway-bound node", box.execs)
	}
	if home := env["CODEX_HOME"]; home != "" {
		t.Errorf("CODEX_HOME=%s crossed for a gateway-bound node", home)
	}
}

// Only the operator's exact order ships the forfait into a sandbox, the value
// the in-process factory reads as one: unset or anything else, a host with no
// key spends its forfait in process, but nothing of its codex home is sent.
func TestWithoutTheOrderTheHostForfaitStaysHome(t *testing.T) {
	for _, useOAuth := range []string{"", "true", "yes", " 1", "1 "} {
		for _, envKey := range []string{"", "sk-env"} {
			t.Run(fmt.Sprintf("oauth=%q/env key=%v", useOAuth, envKey != ""), func(t *testing.T) {
				hostCodexHome(t, hostForfaitBlob)
				t.Setenv("ITERION_OPENAI_USE_OAUTH", useOAuth)
				t.Setenv("OPENAI_API_KEY", envKey)
				t.Setenv("OPENAI_BASE_URL", "")
				box := newDeliverySandbox(t)
				env := runnerEnvOf(t, context.Background(), box, "openai/gpt-6")
				if len(box.execs) != 0 {
					t.Errorf("the dispatch ran %q in the container without the operator's order", box.execs)
				}
				if home := env["CODEX_HOME"]; home != "" {
					t.Errorf("CODEX_HOME=%s crossed without the operator's order", home)
				}
				if v, forced := env["ITERION_OPENAI_USE_OAUTH"]; forced {
					t.Errorf("ITERION_OPENAI_USE_OAUTH=%q crossed without the operator's order", v)
				}
			})
		}
	}
}

// The order and the login it ships must both be the operator's. A project
// `.env` the CLI auto-loads can plant the switch, the codex home, or the home
// directory the default sits under; a planted value still decides what the
// in-process factory spends, but it does not speak for the operator, so
// nothing leaves the host on its word — and the operator is told why.
func TestAProjectDotEnvCannotShipTheHostForfait(t *testing.T) {
	type setup func(t *testing.T)
	plant := func(t *testing.T, name, value string) {
		t.Setenv(name, value)
		envtrust.MarkPlanted(name)
	}
	for _, tc := range []struct {
		name      string
		setup     setup
		delivered bool
	}{
		{"the operator's own switch and codex home", func(t *testing.T) {
			hostCodexHome(t, hostForfaitBlob)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
		}, true},
		{"a planted switch", func(t *testing.T) {
			hostCodexHome(t, hostForfaitBlob)
			plant(t, "ITERION_OPENAI_USE_OAUTH", "1")
		}, false},
		{"a planted codex home", func(t *testing.T) {
			dir := hostCodexHome(t, hostForfaitBlob)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
			plant(t, "CODEX_HOME", dir)
		}, false},
		{"a planted home beside the operator's own codex home", func(t *testing.T) {
			hostCodexHome(t, hostForfaitBlob)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
			plant(t, store.HomeEnvName(), t.TempDir())
		}, true},
		{"a planted home under the default codex home", func(t *testing.T) {
			hostCodexHome(t, "")
			t.Setenv("CODEX_HOME", "")
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(hostForfaitBlob), 0o600); err != nil {
				t.Fatal(err)
			}
			plant(t, store.HomeEnvName(), home)
		}, false},
	} {
		for _, envKey := range []string{"sk-env", ""} {
			t.Run(fmt.Sprintf("%s/env key=%v", tc.name, envKey != ""), func(t *testing.T) {
				tc.setup(t)
				t.Setenv("OPENAI_API_KEY", envKey)
				t.Setenv("OPENAI_BASE_URL", "")
				var logs bytes.Buffer
				b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}, WithClawLogger(iterlog.New(iterlog.LevelInfo, &logs)))
				box := newDeliverySandbox(t)
				env, err := dispatchToSandbox(t, context.Background(), b, box, box, "openai/gpt-6")
				if env == nil {
					t.Fatalf("the dispatch started no runner: %v", err)
				}
				if got := env["CODEX_HOME"] != ""; got != tc.delivered {
					t.Fatalf("CODEX_HOME crossed = %v, want %v (execs: %q)", got, tc.delivered, box.execs)
				}
				if tc.delivered {
					return
				}
				if len(box.execs) != 0 {
					t.Errorf("the dispatch ran %q in the container on a project .env's word", box.execs)
				}
				if v, forced := env["ITERION_OPENAI_USE_OAUTH"]; forced {
					t.Errorf("ITERION_OPENAI_USE_OAUTH=%q crossed on a project .env's word", v)
				}
				if !strings.Contains(logs.String(), "⚠️") || !strings.Contains(logs.String(), "project .env") {
					t.Errorf("the operator is not warned why the forfait stayed home; the log says: %q", logs.String())
				}
			})
		}
	}
}

// A planted switch is only worth a warning where the forfait would otherwise
// have gone: a node the host's forfait would not serve anyway — a run holding
// its own key, a node of another provider, a host with no ChatGPT login —
// hears nothing about it.
func TestAPlantedSwitchIsNoAlarmWhereNothingWouldHaveGone(t *testing.T) {
	for _, tc := range []struct{ name, creds, model, blob string }{
		{"the run's own key", "own key", "openai/gpt-6", hostForfaitBlob},
		{"another provider's node", "none", "anthropic/claude-haiku-4-5", hostForfaitBlob},
		{"a host holding no ChatGPT login", "none", "openai/gpt-6", ""},
		{"a host logged in with an API key", "none", "openai/gpt-6", hostAPIKeyModeBlob},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostCodexHome(t, tc.blob)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
			envtrust.MarkPlanted("ITERION_OPENAI_USE_OAUTH")
			t.Setenv("OPENAI_API_KEY", "sk-env")
			t.Setenv("OPENAI_BASE_URL", "")
			t.Setenv("ANTHROPIC_API_KEY", "sk-ant-fixture")
			var logs bytes.Buffer
			b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}, WithClawLogger(iterlog.New(iterlog.LevelInfo, &logs)))
			box := newDeliverySandbox(t)
			if env, err := dispatchToSandbox(t, runCredentials(t)[tc.creds](), b, box, box, tc.model); env == nil {
				t.Fatalf("the dispatch started no runner: %v", err)
			}
			if strings.Contains(logs.String(), "project .env") {
				t.Errorf("a warning about the planted switch where the forfait would not have gone anyway: %q", logs.String())
			}
		})
	}
}

// A driver whose commands run on the host (noop) has no container to deliver
// into: its runner inherits the host's own env, switch and codex home
// included, so nothing is written and nothing redirects its CODEX_HOME —
// behind a wrapper that hides the run's optional capabilities too, as a
// subbot child's PATH wrapper does.
func TestAHostRunGetsNoForfaitDelivery(t *testing.T) {
	hostCodexHome(t, hostForfaitBlob)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv("OPENAI_BASE_URL", "")
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%v", wrapped), func(t *testing.T) {
			rec := newDeliverySandbox(t)
			var run sandbox.Run = hostRunSandbox{rec}
			if wrapped {
				run = hidingRun{run}
			}
			env, err := dispatchToSandbox(t, context.Background(), newTestClawBackend(), run, rec, "openai/gpt-6")
			if env == nil {
				t.Fatalf("the dispatch started no runner: %v", err)
			}
			if len(rec.execs) != 0 {
				t.Errorf("the dispatch ran %q on the host", rec.execs)
			}
			if home, set := env["CODEX_HOME"]; set {
				t.Errorf("CODEX_HOME=%q set for a runner that already reads the host's own codex home", home)
			}
		})
	}
}

// Only a node on claw's openai provider spends the forfait: another node's
// container gets nothing of the host's codex home.
func TestOnlyAnOpenAINodeGetsTheHostForfait(t *testing.T) {
	hostCodexHome(t, hostForfaitBlob)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-fixture")
	t.Setenv("XAI_API_KEY", "xai-fixture")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	for _, model := range []string{"anthropic/claude-haiku-4-5", "xai/grok-4", "openai_compatible/qwen", "openaix/gpt-6"} {
		t.Run(model, func(t *testing.T) {
			box := newDeliverySandbox(t)
			env := runnerEnvOf(t, context.Background(), box, model)
			if len(box.execs) != 0 {
				t.Errorf("a %s node's container got %q", model, box.execs)
			}
			if home := env["CODEX_HOME"]; home != "" {
				t.Errorf("CODEX_HOME=%s crossed for a %s node", home, model)
			}
		})
	}
}

// The delivery is the node's credential: when it cannot be written, the node
// fails, naming it, before any runner spends OPENAI_API_KEY in its place — and
// what the failed write left in the container is removed.
func TestAFailedDeliveryFailsTheNode(t *testing.T) {
	hostCodexHome(t, hostForfaitBlob)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv("OPENAI_BASE_URL", "")
	box := newDeliverySandbox(t)
	box.failWrite = true
	env, err := dispatchToSandbox(t, context.Background(), newTestClawBackend(), box, box, "openai/gpt-6")
	if env != nil {
		t.Error("a runner started after the forfait delivery failed — it spends the key the operator's switch overrides")
	}
	if err == nil || !strings.Contains(err.Error(), "ITERION_OPENAI_USE_OAUTH=1") {
		t.Errorf("dispatch error = %v, want one naming the forfait delivery the switch ordered", err)
	}
	if left := box.files(); len(left) != 0 {
		t.Errorf("the failed delivery left %v in the container", slices.Sorted(maps.Keys(left)))
	}
}

// A node cancelled while its runner runs still has its copy removed: the
// removal does not ride the node's own, cancelled, context.
func TestACancelledNodeStillRemovesItsCopy(t *testing.T) {
	hostCodexHome(t, hostForfaitBlob)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv("OPENAI_BASE_URL", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	box := newDeliverySandbox(t)
	box.onCommand = cancel
	env := runnerEnvOf(t, ctx, box, "openai/gpt-6")
	if env["CODEX_HOME"] == "" {
		t.Fatal("no copy was delivered — the fixture does not set up what it claims")
	}
	if left := box.files(); len(left) != 0 {
		t.Errorf("the copy outlived its cancelled node: %v", slices.Sorted(maps.Keys(left)))
	}
}

// Every dispatch gets its own copy, there when its runner starts and gone once
// the runner is done, and the removal touches nothing else: two nodes running
// at once never share a file one of them is still writing, a copy does not
// outlive the node that spent it, one node's removal leaves another's copy in
// place, and what a node wrote into its CODEX_HOME itself stays.
func TestEachDispatchGetsItsOwnCopyRemovedAfterIt(t *testing.T) {
	hostCodexHome(t, hostForfaitBlob)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv("OPENAI_BASE_URL", "")
	var logs bytes.Buffer
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}, WithClawLogger(iterlog.New(iterlog.LevelInfo, &logs)))
	box := newDeliverySandbox(t)
	// Another dispatch's copy, still in use: no removal may take it.
	sibling := path.Join(hostCodexForfaitSandboxRoot, "SIBLING", "auth.json")
	if err := os.MkdirAll(filepath.Join(box.root, path.Dir(sibling)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(box.root, sibling), []byte(hostForfaitBlob), 0o600); err != nil {
		t.Fatal(err)
	}

	var homes []string
	for i := range 2 {
		box.onCommand = nil
		if i == 0 {
			// Something the node itself left in its CODEX_HOME: not the
			// delivery's to remove.
			box.onCommand = func() {
				home := box.runEnvs[len(box.runEnvs)-1]["CODEX_HOME"]
				kept := filepath.Join(box.root, home, "sessions", "kept.jsonl")
				if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(kept, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		env, err := dispatchToSandbox(t, context.Background(), b, box, box, "openai/gpt-6")
		if env == nil {
			t.Fatalf("the dispatch started no runner: %v", err)
		}
		home := env["CODEX_HOME"]
		if _, there := box.runFiles[len(box.runFiles)-1][path.Join(home, "auth.json")]; home == "" || !there {
			t.Fatalf("the runner started with CODEX_HOME=%q holding no auth.json", home)
		}
		homes = append(homes, home)
	}
	if homes[0] == homes[1] {
		t.Fatalf("two dispatches shared CODEX_HOME %s", homes[0])
	}
	files := box.files()
	if _, there := files[path.Join(homes[0], "auth.json")]; there {
		t.Errorf("the copy in %s outlived its runner", homes[0])
	}
	if _, there := files[path.Join(homes[0], "sessions", "kept.jsonl")]; !there {
		t.Errorf("the removal took %s, which the node wrote, not the delivery", path.Join(homes[0], "sessions", "kept.jsonl"))
	}
	if _, err := os.Lstat(filepath.Join(box.root, homes[1])); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s is still in the container after its runner was done (lstat: %v)", homes[1], err)
	}
	if _, there := box.files()[sibling]; !there {
		t.Errorf("a removal took %s, another dispatch's copy", sibling)
	}
	if strings.Contains(logs.String(), "remove the host's ChatGPT forfait copy") {
		t.Errorf("a warning that the copy was not removed, though it was: %q", logs.String())
	}
}

// A copy the removal could not take is said, where the operator reads it.
func TestACopyLeftBehindIsLogged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root unlinks through a read-only directory: the removal cannot be made to fail this way")
	}
	hostCodexHome(t, hostForfaitBlob)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "1")
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv("OPENAI_BASE_URL", "")
	var logs bytes.Buffer
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}, WithClawLogger(iterlog.New(iterlog.LevelInfo, &logs)))
	box := newDeliverySandbox(t)
	box.onCommand = func() {
		// The copy's directory turned read-only under the node: the removal
		// cannot unlink the copy.
		home := box.runEnvs[len(box.runEnvs)-1]["CODEX_HOME"]
		if err := os.Chmod(filepath.Join(box.root, home), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(box.root, home), 0o700) })
	}
	if env, err := dispatchToSandbox(t, context.Background(), b, box, box, "openai/gpt-6"); env == nil {
		t.Fatalf("the dispatch started no runner: %v", err)
	}
	if !strings.Contains(logs.String(), "remove the host's ChatGPT forfait copy") {
		t.Errorf("the copy stayed in the container and nothing says so; the log says: %q", logs.String())
	}
}
