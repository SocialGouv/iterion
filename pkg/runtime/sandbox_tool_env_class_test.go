package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Every program iterion starts INSIDE the container runs under that
// container's environment, which a found devcontainer.json seeds from the
// reviewed repository (#2316). Several of those programs take options, a
// configuration file or a helper program from the environment — and an
// option is enough: GNU tar's --checkpoint-action=exec=, go's -toolexec
// through the file GOENV names, a kubeconfig's exec credential plugin all
// run a program the repository chose, inside a step the OPERATOR started.
//
// So the deny class owes one entry per such name, and this table is the
// claim: the programs iterion itself runs there, and what each of them
// reads. It exists because the hole was found twice in a row at a program
// nobody had listed (tar, then the go a bot's build step runs) — the
// previous sweeps had read the agent CLIs' own variables, never iterion's
// container-side commands.
//
// A new in-container command adds a row. A row whose name the class does
// not refuse is a hole: either deny the name, or write here why the
// program cannot be reached.
func TestDeniedEnvClassCoversTheCommandsIterionRunsInTheContainer(t *testing.T) {
	for _, tc := range []struct {
		command string // the program iterion starts in the container
		where   string // the site that starts it
		env     string // the name that gives the program options, a config file or a helper
		what    string // what the planted value buys
	}{
		{"sh", "every Run.Exec of a generated script", "ENV", "a startup file sourced by the shell"},
		{"sh", "every Run.Exec of a generated script", "BASH_ENV", "the same, when the image's sh is bash"},
		{"sh", "every Run.Exec of a generated script", "PATH", "the binaries the script's bare names resolve to"},
		{"bash", "a tool node's command", "BASH_FUNC_x%%", "a function that shadows a command the script calls"},
		{"bash", "a tool node's command", "SHELLOPTS", "xtrace, whose PS4 substitutions then run"},
		{"tar", "kubernetes populate/export, scratch bank and restore", "TAR_OPTIONS", "--checkpoint-action=exec=<program>"},
		{"git", "the clone, commit and push of the run's work", "GIT_SSH_COMMAND", "the transport program"},
		{"git", "the clone, commit and push of the run's work", "GIT_CONFIG_GLOBAL", "a config file, hence insteadOf and every helper"},
		{"git", "the clone, commit and push of the run's work", "EDITOR", "the program a commit without a message opens"},
		{"nix", "the devbox install of the post-create prologue", "NIX_CONFIG", "plugin-files, dlopened on any invocation"},
		{"npx", "an MCP server a workflow declares", "NPM_CONFIG_NODE_OPTIONS", "--require <module>, in the server's own process"},
		{"node", "the claude, pi and opencode CLIs", "NODE_OPTIONS", "--require <module>"},
		{"go", "a bot's build or verify step", "GOFLAGS", "-toolexec=<program>"},
		{"go", "a bot's build or verify step", "GOENV", "a file that sets GOFLAGS itself"},
		{"kubectl", "a deploy-shaped workflow's step", "KUBECONFIG", "a kubeconfig whose exec plugin is a program"},
		{"docker", "a deploy-shaped workflow's step", "DOCKER_CONFIG", "a config whose credsStore names a program"},
		{"iterion __claw-runner", "every sandboxed claw node", "ITERION_OPENAI_USE_OAUTH", "which instrument the node spends"},
		{"iterion __claw-runner", "every sandboxed claw node", "SENTRY_DSN", "an outbound reporting channel, switched on"},
		{"git", "the clone, commit and push of the run's work", "HOME", "the gitconfig ~/.gitconfig is, hence every insteadOf and helper"},
		{"git", "the clone, commit and push of the run's work", "XDG_CONFIG_HOME", "the same config by its XDG road, $XDG_CONFIG_HOME/git/config"},
		{"opencode", "an opencode node's agent CLI", "XDG_DATA_HOME", "the credential store its login is read from (detect's openCodeAuthPath)"},
		{"python3", "a tool node's command", "PYTHONPATH", "the module path whose sitecustomize.py executes at interpreter startup"},
		{"java", "a tool node's command", "JAVA_TOOL_OPTIONS", "options every JVM picks up with no flag of ours — --javaagent runs a jar"},
	} {
		t.Run(tc.command+"/"+tc.env, func(t *testing.T) {
			if !devcontainer.DeniedEnvKey(tc.env) {
				t.Errorf("iterion runs %s (%s) in the container and %s gives it %s, but devcontainer.DeniedEnvKey refuses neither: a reviewed repository's containerEnv reaches that step",
					tc.command, tc.where, tc.env, tc.what)
			}
		})
	}
}

// A table is a claim; the tests below RUN four of its rows. The deny
// class is what keeps a reviewed repository's containerEnv out of the steps
// the operator started, and a row that nobody ever drove is a sentence, not a
// guarantee: #2316's hole was found twice at a program the table did not
// name, and the fix landed with no end-to-end witness at all (#2358). Each
// test carries a CONTROL that keeps the planted value in the container
// environment: the repository's program must then run, which proves the
// scenario still has its premise — a quiet verdict is the class's doing, not
// that of a tar or a devbox which ignores the variable.

// containerEnvRun runs a sandbox's commands on the host the way the container
// runs them: under the container's environment — the spec's Env, which the
// drivers hand the container — plus the exec's own, and nothing of the host's
// but the PATH its binaries are found on.
type containerEnvRun struct {
	*podRun
	env map[string]string
}

func (r containerEnvRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	c := r.podRun.Command(ctx, argv, opts)
	c.Env = make([]string, 0, len(r.env)+len(opts.Env)+1)
	for k, v := range r.env {
		c.Env = append(c.Env, k+"="+v)
	}
	for k, v := range opts.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Env = append(c.Env, "PATH="+os.Getenv("PATH"))
	return c
}

func (r containerEnvRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	c := r.Command(ctx, argv, opts)
	c.Stdin = opts.Stdin
	return sandbox.ExecCmd(c, opts)
}

// resolveRepoContainerEnv resolves the run's sandbox for wf over a repository
// whose devcontainer.json carries containerEnv, and returns the container's
// environment with the names the run reports it removed.
func resolveRepoContainerEnv(t *testing.T, wf *ir.Workflow, containerEnv map[string]string) (map[string]string, []string) {
	t.Helper()
	planted, err := json.Marshal(containerEnv)
	if err != nil {
		t.Fatal(err)
	}
	repo := writeRepoDevcontainer(t, `{"image":"alpine:3.20","containerEnv":`+string(planted)+`}`)
	res, err := resolveSandboxSpecWithFallback(wf, repo, "", "", "ghcr.io/test/sandbox:v1", "")
	if err != nil {
		t.Fatalf("resolve the sandbox spec: %v", err)
	}
	return res.spec.Env, res.deniedEnv
}

// The table's tar row, run: a reviewed repository's devcontainer.json plants
// TAR_OPTIONS with a checkpoint action, and the scratch bank's own tar steps
// — bankScratch and restoreScratch, driven for real under GNU tar — run with
// the container environment the run resolves from that repository.
func TestTheScratchBankTarStepsNeverRunTheRepositorysProgram(t *testing.T) {
	if out, err := exec.Command("tar", "--version").Output(); err != nil || !bytes.Contains(out, []byte("GNU tar")) {
		t.Skip("TAR_OPTIONS is GNU tar's, and no GNU tar is on PATH")
	}
	marker := filepath.Join(t.TempDir(), "ran")
	program := filepath.Join(t.TempDir(), "planted.sh")
	// tar splits TAR_OPTIONS on blanks: the program's path is one word.
	if strings.ContainsAny(program, " \t\n") {
		t.Skipf("the test's temporary path %q holds a blank", program)
	}
	if err := os.WriteFile(program, []byte("#!/bin/sh\necho ran >> "+shellquote.Quote(marker)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Whether the planted program CAN run here is the machine's business (a
	// noexec temporary directory), and it is asked before the control, so
	// the control's failure keeps one meaning: the program runs, and tar did
	// not run it.
	if err := exec.Command(program).Run(); err != nil {
		t.Skipf("the planted program does not run from %s (%v): this scenario needs an executable temporary directory", filepath.Dir(program), err)
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	planted := "--checkpoint=1 --checkpoint-action=exec=" + program
	env, denied := resolveRepoContainerEnv(t, &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}},
		map[string]string{"TAR_OPTIONS": planted})
	if !slices.Contains(denied, "TAR_OPTIONS") {
		t.Errorf("deniedEnv = %v, want TAR_OPTIONS named", denied)
	}

	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-tar-options"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	bs := store.AsScratchBankStore(s)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "notes.txt"), []byte("kept across the park\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// runs counts the planted program's runs so far.
	runs := func(t *testing.T) int {
		t.Helper()
		b, err := os.ReadFile(marker)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return bytes.Count(b, []byte("ran\n"))
	}
	// steps banks src and restores the bank under env, the container's, and
	// reports whether the planted program ran in each tar step.
	steps := func(t *testing.T, env map[string]string) (inBank, inRestore bool) {
		t.Helper()
		before := runs(t)
		if got := bankScratch(ctx, containerEnvRun{podRun: &podRun{scratch: src}, env: env}, sandboxScratchContainerPath, bs, runID, scratchBankMaxBytes); !got.banked {
			t.Fatalf("not banked: %+v", got)
		}
		inBank = runs(t) > before
		body, err := bs.OpenScratchBank(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		dst := filepath.Join(t.TempDir(), "scratch")
		before = runs(t)
		if err := restoreScratch(ctx, containerEnvRun{podRun: &podRun{scratch: dst}, env: env}, sandboxScratchContainerPath, body); err != nil {
			t.Fatalf("restore: %v", err)
		}
		inRestore = runs(t) > before
		if b, err := os.ReadFile(filepath.Join(dst, "notes.txt")); err != nil || string(b) != "kept across the park\n" {
			t.Fatalf("the scratch did not come back: %q %v", b, err)
		}
		return inBank, inRestore
	}

	if inBank, inRestore := steps(t, map[string]string{"TAR_OPTIONS": planted}); !inBank || !inRestore {
		t.Fatalf("control: with TAR_OPTIONS=%q in the container, the planted program ran in the bank %v and the restore %v, want both — the GNU tar here no longer runs a TAR_OPTIONS checkpoint action, and this scenario lost its premise", planted, inBank, inRestore)
	}
	if inBank, inRestore := steps(t, env); inBank || inRestore {
		t.Errorf("the repository's program ran in iterion's own tar step (bank %v, restore %v): its TAR_OPTIONS reached the container environment %v", inBank, inRestore, env)
	}
}

// The table's nix row, run: a reviewed repository's devcontainer.json plants
// NIX_CONFIG with a `plugin-files`, which nix dlopens on any invocation, and
// the post-create prologue iterion generates runs under `sh -c` with the
// container environment the run resolves from it — a `devbox` on PATH records
// the NIX_CONFIG it, hence the nix it drives, starts under. The prologue
// yields to a NIX_CONFIG already set (the control), so the class alone keeps
// the repository's value out, and the workflow's `sandbox.env:` still decides.
func TestTheDevboxInstallIterionRunsNeverTakesTheRepositorysNixConfig(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	const (
		planted   = "plugin-files = /workspaces/repo/.devcontainer/planted.so"
		operators = "substituters = https://cache.operator.example"
		iterions  = "build-users-group ="
	)
	snippet := devboxInstallSnippet([]devboxProject{{label: "repo", dir: writeDevboxConfig(t, t.TempDir(), "jq@1.8")}})
	// seen runs the prologue under env, the container's, and returns the
	// NIX_CONFIG devbox was started under.
	seen := func(t *testing.T, env map[string]string) string {
		t.Helper()
		bin := t.TempDir()
		record := filepath.Join(t.TempDir(), "nix-config")
		fakeDevbox := "#!/bin/sh\nprintf '%s' \"${NIX_CONFIG-unset}\" > " + shellquote.Quote(record) + "\n"
		if err := os.WriteFile(filepath.Join(bin, "devbox"), []byte(fakeDevbox), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(sh, "-c", snippet)
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		// HOME and TMPDIR are the test's own: the prologue keeps its scratch
		// under $TMPDIR and may link a profile under $HOME.
		cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+t.TempDir(), "TMPDIR="+t.TempDir())
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("the prologue exited non-zero: %v\n%s", err, stderr.String())
		}
		got, err := os.ReadFile(record)
		if err != nil {
			t.Fatalf("the prologue never started devbox: %v\n%s", err, stderr.String())
		}
		return string(got)
	}

	if got := seen(t, map[string]string{"NIX_CONFIG": planted}); got != planted {
		t.Fatalf("control: with NIX_CONFIG=%q in the container, devbox started under %q — the prologue no longer yields to a NIX_CONFIG already set, and this scenario lost its premise", planted, got)
	}
	t.Run("the repository's devcontainer", func(t *testing.T) {
		env, denied := resolveRepoContainerEnv(t, &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}},
			map[string]string{"NIX_CONFIG": planted})
		if got := seen(t, env); got != iterions {
			t.Errorf("devbox started under NIX_CONFIG %q, want iterion's own %q: the repository's devcontainer.json reached the nix iterion's provisioning drives", got, iterions)
		}
		if !slices.Contains(denied, "NIX_CONFIG") {
			t.Errorf("deniedEnv = %v, want NIX_CONFIG named", denied)
		}
	})
	t.Run("the workflow's sandbox.env", func(t *testing.T) {
		wf := &ir.Workflow{Sandbox: &ir.SandboxSpec{
			Mode: string(sandbox.ModeAuto),
			Env:  map[string]string{"NIX_CONFIG": operators},
		}}
		env, _ := resolveRepoContainerEnv(t, wf, map[string]string{"NIX_CONFIG": planted})
		if got := seen(t, env); got != operators {
			t.Errorf("devbox started under NIX_CONFIG %q, want the workflow's %q: the operator's sandbox.env decides", got, operators)
		}
	})
}

// The home a program reads its configuration FROM is the same channel as the
// configuration variables themselves (#2318): these two constants are the
// remote the planted config rewrites, under a `.invalid` host (RFC 2606) so
// nothing reachable and no operator's own `insteadOf` can decide the witness.
const (
	upstreamRemoteURL = "https://upstream.invalid/repo.git"
	plantedMirrorBase = "https://attacker.invalid/mirror/"
)

// plantHostileGitConfig writes, at path, the git configuration a reviewed
// repository committed: every URL under the upstream host rewritten to a
// mirror of its own, and a credential helper that is a program — the shape
// #2316's rva reproduced in rounds 5-6, where the helper RAN and was handed
// the operator's token (Jo, 2026-10-12). canary is the file that program
// appends to when git runs it.
func plantHostileGitConfig(t *testing.T, path, canary string) {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "planted-credential-helper")
	// git takes the rest of the line as the helper's argv: a blank in the
	// path would split it, and the scenario would die as a false negative.
	if strings.ContainsAny(helper, " \t\n") || strings.ContainsAny(canary, " \t\n") {
		t.Skipf("the test's temporary paths hold a blank: %q, %q", helper, canary)
	}
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho ran >> "+shellquote.Quote(canary)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Whether the planted program CAN run here is the machine's business (a
	// noexec temporary directory), and it is asked before the control, so a
	// quiet canary below keeps one meaning.
	if err := exec.Command(helper).Run(); err != nil {
		t.Skipf("the planted helper does not run from %s (%v): this scenario needs an executable temporary directory", filepath.Dir(helper), err)
	}
	if err := os.Remove(canary); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[url \"" + plantedMirrorBase + "\"]\n\tinsteadOf = https://upstream.invalid/\n" +
		"[credential]\n\thelper = " + helper + "\n"
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitUnderContainerEnv runs git the way the container runs it: under the
// environment the drivers hand the container and nothing of the host's but
// the PATH its binaries are found on (containerEnvRun's rule), from a
// directory outside any repository so the only configuration in play is the
// one the environment points at. Two names ride on BOTH arms, the test's own
// instrumentation: GIT_TERMINAL_PROMPT=0, so a helper that answers nothing
// fails instead of reaching for a terminal, and GIT_CONFIG_NOSYSTEM=1, so
// /etc/gitconfig — the MACHINE's, which a CI image may give a credential
// helper of its own — decides nothing here either way.
func gitUnderContainerEnv(t *testing.T, env map[string]string, stdin string, args ...string) (string, error) {
	t.Helper()
	// A writing git since 2.48 detaches `git maintenance run --auto`; the
	// guard of pkg/git refuses it so nothing keeps writing under .git after
	// the test returns.
	cmd := exec.Command("git", git.NoAutoMaintenance(args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = make([]string, 0, len(env)+2)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "PATH="+os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// The table's git rows, run: a reviewed repository's devcontainer.json plants
// the home git reads its configuration from, and a real git resolves a remote
// URL and fills a credential under the container environment the run resolves
// from that repository. ONE name per subtest, planted alone — two guards that
// cover each other leave both untested, and the runtime's HOME pin is no
// guard here at all: it yields to a value already set, it takes HOME only,
// and it exists only where host_state binds a home (#2318).
func TestGitNeverReadsTheHomeTheRepositoryPlanted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("the witness drives a real git, and none is on PATH")
	}
	for _, tc := range []struct {
		name    string // the env name the repository plants, alone
		relPath string // where git reads the config from, under the planted dir
	}{
		{"HOME", ".gitconfig"},
		{"XDG_CONFIG_HOME", "git/config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The pin's own home is a scratch dir we own: with no HOME in
			// the environment git falls through to the passwd entry, i.e.
			// the OPERATOR's ~/.gitconfig — and would run a credential
			// helper THEY configured.
			hostHome := t.TempDir()
			t.Setenv("HOME", hostHome)
			planted := t.TempDir()
			canary := filepath.Join(t.TempDir(), "helper-ran")
			plantHostileGitConfig(t, filepath.Join(planted, tc.relPath), canary)

			env, denied := resolveRepoContainerEnv(t, &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}},
				map[string]string{tc.name: planted})
			if !slices.Contains(denied, tc.name) {
				t.Errorf("deniedEnv = %v, want %s named: the event is how the operator learns what the repository tried", denied, tc.name)
			}
			// The container's HOME is iterion's own (the host_state pin) or
			// the image's, never the repository's. Both arms get it, so the
			// planted value is the ONLY difference between them. The map is
			// nil when the planted name was the whole of the repository's
			// env — which is exactly the refusal this witness wants.
			if env == nil {
				env = map[string]string{}
			}
			if _, pinned := env["HOME"]; !pinned {
				env["HOME"] = hostHome
			}
			control := maps.Clone(env)
			control[tc.name] = planted

			// The control keeps the planted value in the container: git must
			// then take the mirror AND run the helper, or this scenario has
			// lost its premise and the quiet verdict below proves nothing.
			if url, err := gitUnderContainerEnv(t, control, "", "ls-remote", "--get-url", upstreamRemoteURL); !strings.HasPrefix(url, plantedMirrorBase) {
				t.Fatalf("control: with %s=%s in the container, git resolved %q (%v), want the planted mirror %s — this git does not read %s", tc.name, planted, url, err, plantedMirrorBase, tc.relPath)
			}
			// `credential fill` runs the helper, then fails for want of a
			// username it never returned: the canary, not the exit code, is
			// the effect this reads.
			_, _ = gitUnderContainerEnv(t, control, "protocol=https\nhost=upstream.invalid\n\n", "credential", "fill")
			if _, err := os.Stat(canary); err != nil {
				t.Fatalf("control: the planted credential helper did not run (%v) — the premise is gone", err)
			}
			if err := os.Remove(canary); err != nil {
				t.Fatal(err)
			}

			// The verdict: the container environment the run resolved.
			if url, err := gitUnderContainerEnv(t, env, "", "ls-remote", "--get-url", upstreamRemoteURL); url != upstreamRemoteURL {
				t.Errorf("git resolved %q (%v), want the upstream %s: the repository's %s reached the container environment %v", url, err, upstreamRemoteURL, tc.name, env)
			}
			_, _ = gitUnderContainerEnv(t, env, "protocol=https\nhost=upstream.invalid\n\n", "credential", "fill")
			if _, err := os.Stat(canary); err == nil {
				t.Errorf("the repository's credential helper RAN in iterion's own git step: its %s reached the container environment %v", tc.name, env)
			}
		})
	}
}

// The table's opencode row: $XDG_DATA_HOME/opencode/auth.json is where that
// CLI's login lives, and pkg/backend/detect reads it from the variable
// (openCodeAuthPath, held by that package's own tests) — a planted store
// answers the credential lookup of an agent iterion runs, which is what Jo
// saw moved on 2026-10-12. Read at the seam and not through detect.Detect,
// which shells out to the operator's own claude CLI; the control row proves
// the seam dropped the name and not the whole map.
func TestTheRepositorysPlantedXDGDataHomeNeverReachesTheContainer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	planted := t.TempDir()
	env, denied := resolveRepoContainerEnv(t, &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}},
		map[string]string{"XDG_DATA_HOME": planted, "MYTOOL_DATA": planted})
	if !slices.Contains(denied, "XDG_DATA_HOME") {
		t.Errorf("deniedEnv = %v, want XDG_DATA_HOME named", denied)
	}
	if got, present := env["XDG_DATA_HOME"]; present {
		t.Errorf("XDG_DATA_HOME = %q in the container environment: the repository moved the store an agent's login is read from", got)
	}
	if env["MYTOOL_DATA"] != planted {
		t.Fatalf("control: MYTOOL_DATA = %q, want %q — the seam dropped a name OUTSIDE the class, so the verdict above is not the class's doing", env["MYTOOL_DATA"], planted)
	}
}

// The table's interpreter rows, run: the interpreters a tool node starts
// read the repository's planted paths at startup — PYTHONPATH's
// sitecustomize executes before the first statement, every JVM picks up
// JAVA_TOOL_OPTIONS with no flag of ours — and the workspace is mounted,
// so the relative paths a devcontainer.json spells resolve. The container
// environment is the one the run resolves from that repository
// (mode=auto, the repository's file, no opt-in); deniedEnv is the list
// the sandbox_env_denied event carries, the operator's record of the
// attempt.
func TestTheContainerEnvTheRunResolvesNeverCarriesTheInterpreterStartupKnobs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// The premise, both interpreters, under the PLANTED value: the
	// interpreter reads the repository's file — sitecustomize creates the
	// marker, the JVM announces the pickup. A lost premise leaves the
	// quiet verdict below meaningless.
	pyDir := t.TempDir()
	pyMarker := filepath.Join(t.TempDir(), "sitecustomize-ran")
	sitecustomize := "import os\nopen(" + strconv.Quote(pyMarker) + ", 'w').close()\n"
	if err := os.WriteFile(filepath.Join(pyDir, "sitecustomize.py"), []byte(sitecustomize), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("the witness drives a real python3, and none is on PATH")
	}
	controlCmd := exec.Command("python3", "-c", "print(1)")
	controlCmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONNOUSERSITE=1", "PYTHONPATH=" + pyDir}
	if out, err := controlCmd.CombinedOutput(); err != nil {
		t.Fatalf("control: python3 does not run under the planted PYTHONPATH (%v): %s", err, out)
	}
	if _, err := os.Stat(pyMarker); err != nil {
		t.Fatalf("control: sitecustomize did not execute from the planted PYTHONPATH (%v) — the premise is gone", err)
	}
	// The premise's marker is exactly what the verdict below reads: take
	// it away, or the control's execution answers for the verdict's.
	if err := os.Remove(pyMarker); err != nil {
		t.Fatal(err)
	}

	javaPicksUp := func(env []string) bool {
		cmd := exec.Command("java", "-version")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return err == nil && strings.Contains(string(out), "Picked up JAVA_TOOL_OPTIONS")
	}
	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("the witness drives a real java, and none is on PATH")
	}
	premiseCmd := exec.Command("java", "-version")
	premiseCmd.Env = []string{"PATH=" + os.Getenv("PATH"), "JAVA_TOOL_OPTIONS=-Dprobe=1"}
	premiseOut, premiseErr := premiseCmd.CombinedOutput()
	if premiseErr != nil || !strings.Contains(string(premiseOut), "Picked up JAVA_TOOL_OPTIONS") {
		t.Fatalf("control: the JVM did not pick up the planted JAVA_TOOL_OPTIONS (err=%v): %s — the premise is gone", premiseErr, premiseOut)
	}

	env, denied := resolveRepoContainerEnv(t, &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}},
		map[string]string{
			"PYTHONPATH":        ".mylib",
			"JAVA_TOOL_OPTIONS": "-javaagent=.mylib/agent.jar",
			"PYTHONUNBUFFERED":  "1", // the repo's own knob, same family prefix: kept
		})
	for _, name := range []string{"PYTHONPATH", "JAVA_TOOL_OPTIONS"} {
		if !slices.Contains(denied, name) {
			t.Errorf("deniedEnv = %v, want %s named: the event is how the operator learns what the repository tried", denied, name)
		}
		if _, present := env[name]; present {
			t.Errorf("%s reached the container environment: the interpreters the steps start would read the repository's file at startup", name)
		}
	}
	if env["PYTHONUNBUFFERED"] != "1" {
		t.Fatalf("control: PYTHONUNBUFFERED = %q, want 1 — the seam dropped a name OUTSIDE the class, so the verdict above is not the class's doing", env["PYTHONUNBUFFERED"])
	}

	// The verdict, run: the interpreters under the resolved environment
	// see nothing of the repository's planting.
	if env == nil {
		env = map[string]string{}
	}
	envWith := func(extra ...string) []string {
		e := append([]string{"PATH=" + os.Getenv("PATH")}, extra...)
		for k, v := range env {
			e = append(e, k+"="+v)
		}
		return e
	}
	verdictCmd := exec.Command("python3", "-c", "print(1)")
	verdictCmd.Env = envWith("PYTHONNOUSERSITE=1")
	if _, err := verdictCmd.CombinedOutput(); err != nil {
		t.Fatalf("verdict: python3 does not run under the resolved container environment (%v)", err)
	}
	if _, err := os.Stat(pyMarker); err == nil {
		t.Errorf("sitecustomize from the repository's PYTHONPATH executed under the resolved container environment: the knob reached it")
	}
	if javaPicksUp(envWith()) {
		t.Errorf("the JVM picked up JAVA_TOOL_OPTIONS under the resolved container environment: the knob reached it")
	}
}
