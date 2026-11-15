package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// TestDepUpdateGuardVerifyBaseline drives Vetty's verify_run against a real
// repository in the state `align` leaves it: the bump on the PR branch's head,
// the alignment uncommitted on top. Vetty verifies BEFORE it commits, so a
// drift gate mirrored from CI compared against HEAD read the alignment itself
// as drift — buildkit-operator#65, a controller-tools bump whose alignment is
// the regenerated CRDs, held red on every rebase with a green build.
//
// verify_run now runs verify.sh on a baseline commit of the aligned tree, HEAD
// detached onto it. What these cases pin:
//   - the script meets the tree about to be committed — the alignment and
//     align's new file in, the canonical tree noise out — so both gate forms
//     pass when the regeneration reproduces the alignment, and fail when it
//     departs from it;
//   - HEAD, the branch ref and the index come back exactly, on a normal end,
//     on a SIGTERM, and — from the marker — on the call after a SIGKILL;
//   - a restore that fails holds the gate red;
//   - a tree with nothing pending gets no baseline at all.
func TestDepUpdateGuardVerifyBaseline(t *testing.T) {
	for _, bin := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	command := verifyRunCommand(t, "dep-update-guard/main.bot")

	type verifyResult struct {
		Passed           bool   `json:"passed"`
		ExitCode         int    `json:"exit_code"`
		PrecheckRejected bool   `json:"precheck_rejected"`
		PrecheckReason   string `json:"precheck_reason"`
		LogTail          string `json:"log_tail"`
	}
	const branch = "refs/heads/renovate/controller-tools"

	// exec runs verify_run the way the engine does: the substitutions, the
	// hermetic git environment a tool process gets, the noise list.
	execute := func(t *testing.T, ws, scratch string) ([]byte, error) {
		t.Helper()
		cmd := strings.ReplaceAll(command, "{{vars.workspace_dir}}", ws)
		cmd = strings.ReplaceAll(cmd, "{{vars.scratch_dir}}", scratch)
		cmd = strings.ReplaceAll(cmd, "{{vars.verify_timeout_s}}", "120")
		if strings.Contains(cmd, "{{") {
			t.Fatalf("an unsubstituted reference is left in the command")
		}
		c := exec.Command("sh", "-c", cmd)
		if len(cmd) > 100_000 {
			c = exec.Command("sh", "-s")
			c.Stdin = strings.NewReader(cmd)
		}
		c.Env = append(gittest.Env(), treenoise.TreeNoiseEnvVar+"="+treenoise.EnvValue())
		return c.Output()
	}
	run := func(t *testing.T, ws, scratch string) verifyResult {
		t.Helper()
		out, err := execute(t, ws, scratch)
		if err != nil {
			t.Fatalf("verify_run failed to execute: %v (out %q)", err, out)
		}
		var res verifyResult
		if uerr := json.Unmarshal(out, &res); uerr != nil {
			t.Fatalf("verify_run output is not verify_result JSON: %v (out %q)", uerr, out)
		}
		return res
	}
	write := func(t *testing.T, ws, rel, body string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T, p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return strings.TrimSpace(string(b))
	}

	// repo builds the PR branch: base commit, then the bump commit on
	// renovate/controller-tools — the head the run read.
	repo := func(t *testing.T) (ws, head string) {
		t.Helper()
		ws = t.TempDir()
		gittest.Run(t, ws, "init", "-q")
		write(t, ws, "gen/crd.yaml", "version: v0.21.0\n")
		write(t, ws, "devbox.lock", "plugin_version: old\n")
		write(t, ws, "Taskfile.yml", "vars:\n  CONTROLLER_GEN: v0.21.0\n")
		write(t, ws, ".github/workflows/ci.yml",
			"jobs:\n  test:\n    steps:\n      - run: |\n          task manifests\n          git diff --exit-code\n")
		gittest.Run(t, ws, "add", "-A")
		gittest.Run(t, ws, "commit", "-q", "-m", "base")
		gittest.Run(t, ws, "checkout", "-q", "-b", strings.TrimPrefix(branch, "refs/heads/"))
		write(t, ws, "Taskfile.yml", "vars:\n  CONTROLLER_GEN: v0.23.0\n")
		gittest.Run(t, ws, "commit", "-q", "-am", "chore(deps): update controller-tools to v0.23.0")
		return ws, gittest.Run(t, ws, "rev-parse", "HEAD")
	}
	// align leaves what align leaves: the regenerated output and a new file,
	// uncommitted, plus the engine's own noise — a rewritten devbox.lock and
	// the .claude/ mirror.
	align := func(t *testing.T, ws string) {
		t.Helper()
		write(t, ws, "gen/crd.yaml", "version: v0.23.0\n")
		write(t, ws, "gen/new.yaml", "kind: New\n")
		write(t, ws, "devbox.lock", "plugin_version: new\n")
		write(t, ws, ".claude/skills/x.md", "mirror\n")
	}
	type state struct{ head, branchSha, symbolic, index, staged string }
	snapshot := func(t *testing.T, ws string) state {
		t.Helper()
		sym, _ := gittest.Try(ws, "symbolic-ref", "-q", "HEAD")
		return state{
			head:      gittest.Run(t, ws, "rev-parse", "HEAD"),
			branchSha: gittest.Run(t, ws, "rev-parse", branch),
			symbolic:  sym,
			index:     gittest.Run(t, ws, "ls-files", "-s"),
			staged:    gittest.Run(t, ws, "diff", "--cached", "--name-only"),
		}
	}
	restored := func(t *testing.T, ws, scratch string, want state) {
		t.Helper()
		if got := snapshot(t, ws); got != want {
			t.Errorf("HEAD, the branch or the index were not put back:\n got %+v\nwant %+v", got, want)
		}
		if _, err := os.Stat(filepath.Join(scratch, "verify-baseline.json")); !os.IsNotExist(err) {
			t.Errorf("the baseline marker outlived a successful restore (stat err %v)", err)
		}
	}
	noise := treenoise.ShellPathspecs()
	// script mirrors CI's drift gate in both forms, and records what it saw.
	script := func(regen, extra string) func(t *testing.T, scratch string) {
		return func(t *testing.T, scratch string) {
			t.Helper()
			body := "#!/bin/sh\nset -e\n" + extra +
				"git rev-parse HEAD^ > " + filepath.Join(scratch, "seen-parent") + "\n" +
				"git symbolic-ref -q HEAD > " + filepath.Join(scratch, "seen-symbolic") + " || true\n" +
				"git show HEAD:devbox.lock > " + filepath.Join(scratch, "seen-devbox") + "\n" +
				"git ls-tree -r --name-only HEAD > " + filepath.Join(scratch, "seen-tree") + "\n" +
				"printf '" + regen + "\\n' > gen/crd.yaml\n" +
				"git diff --exit-code -- ':/' " + noise + "\n" +
				"test -z \"$(git status --porcelain -- ':/' " + noise + ")\"\n"
			if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("a regeneration that reproduces the alignment passes both gate forms", func(t *testing.T) {
		ws, head := repo(t)
		align(t, ws)
		scratch := t.TempDir()
		before := snapshot(t, ws)
		script("version: v0.23.0", "")(t, scratch)

		res := run(t, ws, scratch)
		if !res.Passed {
			t.Fatalf("the alignment itself was read as drift (exit %d):\n%s", res.ExitCode, res.LogTail)
		}
		if !strings.Contains(res.LogTail, "VERIFY BASELINE: verify.sh ran on ") {
			t.Errorf("the log must say the script ran on a baseline:\n%s", res.LogTail)
		}
		if got := read(t, filepath.Join(scratch, "seen-parent")); got != head {
			t.Errorf("the baseline's parent is %s, want the head the run read %s", got, head)
		}
		if got := read(t, filepath.Join(scratch, "seen-symbolic")); got != "" {
			t.Errorf("verify.sh ran with HEAD on %q; it must be detached, the branch ref untouched", got)
		}
		if got := read(t, filepath.Join(scratch, "seen-devbox")); got != "plugin_version: old" {
			t.Errorf("tree noise rode into the baseline: devbox.lock there reads %q", got)
		}
		tree := read(t, filepath.Join(scratch, "seen-tree"))
		if !strings.Contains(tree, "gen/new.yaml") {
			t.Errorf("align's new file is missing from the baseline:\n%s", tree)
		}
		if strings.Contains(tree, ".claude/") {
			t.Errorf("the .claude/ mirror rode into the baseline:\n%s", tree)
		}
		restored(t, ws, scratch, before)
		if got := read(t, filepath.Join(ws, "gen/crd.yaml")); got != "version: v0.23.0" {
			t.Errorf("the working tree changed: gen/crd.yaml reads %q", got)
		}
	})

	t.Run("a regeneration that departs from the alignment is still drift", func(t *testing.T) {
		ws, _ := repo(t)
		align(t, ws)
		scratch := t.TempDir()
		before := snapshot(t, ws)
		script("version: v0.24.0", "")(t, scratch)

		res := run(t, ws, scratch)
		if res.Passed || res.ExitCode != 1 {
			t.Fatalf("a regeneration that does not reproduce the alignment must fail the drift gate, got passed=%v exit=%d:\n%s",
				res.Passed, res.ExitCode, res.LogTail)
		}
		restored(t, ws, scratch, before)
	})

	t.Run("nothing pending: no baseline, HEAD stays on the branch", func(t *testing.T) {
		ws, _ := repo(t)
		scratch := t.TempDir()
		before := snapshot(t, ws)
		script("version: v0.21.0", "")(t, scratch)

		res := run(t, ws, scratch)
		if !res.Passed {
			t.Fatalf("a clean tree whose regeneration matches must pass (exit %d):\n%s", res.ExitCode, res.LogTail)
		}
		if strings.Contains(res.LogTail, "VERIFY BASELINE") {
			t.Errorf("a tree with nothing pending got a baseline:\n%s", res.LogTail)
		}
		if got := read(t, filepath.Join(scratch, "seen-symbolic")); got != branch {
			t.Errorf("verify.sh saw HEAD %q, want the branch %s", got, branch)
		}
		restored(t, ws, scratch, before)
	})

	t.Run("a SIGTERM mid-script puts HEAD and the index back", func(t *testing.T) {
		ws, _ := repo(t)
		align(t, ws)
		scratch := t.TempDir()
		before := snapshot(t, ws)
		script("version: v0.23.0", "kill -TERM \"$PPID\"\nexec sleep 30\n")(t, scratch)

		if out, err := execute(t, ws, scratch); err == nil {
			t.Fatalf("verify_run answered through a SIGTERM: %q", out)
		}
		restored(t, ws, scratch, before)
	})

	t.Run("after a SIGKILL the next call restores first; a restore that fails holds red", func(t *testing.T) {
		ws, _ := repo(t)
		align(t, ws)
		scratch := t.TempDir()
		before := snapshot(t, ws)
		script("version: v0.23.0", "kill -KILL \"$PPID\"\nexit 1\n")(t, scratch)

		if out, err := execute(t, ws, scratch); err == nil {
			t.Fatalf("verify_run answered through a SIGKILL: %q", out)
		}
		// The scenario itself: the kill left the tree on the baseline.
		if sym, _ := gittest.Try(ws, "symbolic-ref", "-q", "HEAD"); sym != "" {
			t.Fatalf("the SIGKILL left HEAD on %q; the scenario needs it on the baseline", sym)
		}
		if _, err := os.Stat(filepath.Join(scratch, "verify-baseline.json")); err != nil {
			t.Fatalf("the SIGKILL left no marker: %v", err)
		}

		t.Run("the saved index lost: held red, marker kept", func(t *testing.T) {
			saved := filepath.Join(scratch, "verify-baseline.index")
			keep, err := os.ReadFile(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(saved); err != nil {
				t.Fatal(err)
			}
			res := run(t, ws, scratch)
			if res.Passed || res.ExitCode != 6 || !strings.Contains(res.LogTail, "VERIFY BASELINE NOT RESTORED") {
				t.Errorf("a restore that failed must hold the gate red with exit 6, got passed=%v exit=%d:\n%s",
					res.Passed, res.ExitCode, res.LogTail)
			}
			if _, err := os.Stat(filepath.Join(scratch, "verify-baseline.json")); err != nil {
				t.Errorf("a failed restore dropped its marker: %v", err)
			}
			if err := os.WriteFile(saved, keep, 0o644); err != nil {
				t.Fatal(err)
			}
		})

		script("version: v0.23.0", "")(t, scratch)
		res := run(t, ws, scratch)
		if !res.Passed {
			t.Fatalf("the call after the kill must restore, re-baseline and pass (exit %d):\n%s", res.ExitCode, res.LogTail)
		}
		restored(t, ws, scratch, before)
	})

	t.Run("a masked exit status loops back to verify_build", func(t *testing.T) {
		ws, _ := repo(t)
		align(t, ws)
		scratch := t.TempDir()
		body := "#!/bin/sh\nset -e\ntask manifests\ndiff -u a b | head -40 >&2\ngit diff --exit-code\n"
		if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		res := run(t, ws, scratch)
		if res.Passed {
			t.Fatal("a masked script must not open the commit path")
		}
		if !res.PrecheckRejected || !strings.Contains(res.PrecheckReason, "MASKED EXIT STATUS") {
			t.Errorf("a masked exit status is an authorship defect: it must loop back with its reason, got rejected=%v reason=%q",
				res.PrecheckRejected, res.PrecheckReason)
		}
		if _, err := os.Stat(filepath.Join(scratch, "verify.sh.rejected")); err != nil {
			t.Errorf("the masked script was not moved aside: %v", err)
		}
	})
}
