package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
	} {
		t.Run(tc.command+"/"+tc.env, func(t *testing.T) {
			if !devcontainer.DeniedEnvKey(tc.env) {
				t.Errorf("iterion runs %s (%s) in the container and %s gives it %s, but devcontainer.DeniedEnvKey refuses neither: a reviewed repository's containerEnv reaches that step",
					tc.command, tc.where, tc.env, tc.what)
			}
		})
	}
}

// A table is a claim; the two tests below RUN two of its rows. The deny
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
