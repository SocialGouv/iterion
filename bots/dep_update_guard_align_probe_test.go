package bots

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// TestDepUpdateGuardAlignProbeReadsTheTree runs align_probe against real
// repositories. commit_check trusts its two readings to tell an alignment the
// branch lacks from one the head already carries (buildkit-operator#50), so
// every state it has to read is built here with git, not described.
func TestDepUpdateGuardAlignProbeReadsTheTree(t *testing.T) {
	for _, bin := range []string{"python3", "git", "bash"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	tpl := toolCommand(t, "dep-update-guard/main.bot", "align_probe")
	type probe struct {
		Pending     bool     `json:"pending"`
		Carried     bool     `json:"carried"`
		Paths       []string `json:"paths"`
		Unaccounted []string `json:"unaccounted"`
		Reason      string   `json:"reason"`
	}
	// word renders a value the way the engine does in a command body: one
	// single-quoted shell word.
	word := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
	// probeWith runs the command with the engine's substitutions, the way the
	// engine runs a tool command (a program past 100 000 bytes goes to `bash
	// -s` on stdin, pkg/backend/model/executor_tool.go), in the environment a
	// tool process gets (hermetic git config, the noise list), adjusted by env.
	probeWith := func(t *testing.T, ws, base, head, claimed string, env func([]string) []string) probe {
		t.Helper()
		s := strings.ReplaceAll(tpl, "{{vars.workspace_dir}}", word(ws))
		s = strings.ReplaceAll(s, "{{vars.base_ref}}", word(base))
		s = strings.ReplaceAll(s, "{{input.head_sha}}", word(head))
		s = strings.ReplaceAll(s, "{{input.claimed}}", word(claimed))
		if strings.Contains(s, "{{") {
			t.Fatalf("an unsubstituted reference is left in the command:\n%s", s)
		}
		cmd := exec.Command("bash", "-c", s)
		if len(s) > 100_000 {
			cmd = exec.Command("bash", "-s")
			cmd.Stdin = strings.NewReader(s)
		}
		cmd.Env = env(append(gittest.Env(), treenoise.TreeNoiseEnvVar+"="+treenoise.EnvValue()))
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("command failed: %v (out %q)", err, raw)
		}
		var p probe
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("output is not JSON: %v (out %q)", err, raw)
		}
		if !strings.HasPrefix(p.Reason, "Checked after align: ") {
			t.Fatalf("the reason must open with its own label, for the paragraph the hold comment gives it: %q", p.Reason)
		}
		return p
	}
	same := func(e []string) []string { return e }
	read := func(t *testing.T, ws, head string, claimed ...string) probe {
		t.Helper()
		js, err := json.Marshal(append([]string{}, claimed...))
		if err != nil {
			t.Fatal(err)
		}
		return probeWith(t, ws, "main", head, string(js), same)
	}
	write := func(t *testing.T, ws, rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const aligned = "package main\n// aligned for the bump\n"
	// bumpRepo is a Renovate PR as Vetty clones it: `main` with the
	// baseline, the PR branch one dependency bump ahead of where it forked,
	// `main` moved on since (upstream.go, which only main changes), and the
	// workspace carrying what the engine and the run tooling write on every
	// run — the .claude/ mirror (its settings.json tracked, then rewritten)
	// and a drifted devbox.lock.
	bumpRepo := func(t *testing.T) (ws, head string) {
		t.Helper()
		ws = scaffoldRepo(t)
		write(t, ws, "devbox.lock", "plugin_version: committed\n")
		write(t, ws, "legacy.go", "package main\n")
		gittest.Run(t, ws, "add", "-f", "devbox.lock", "legacy.go", filepath.Join(".claude", "settings.json"))
		gittest.Run(t, ws, "commit", "-q", "-m", "baseline tooling")
		gittest.Run(t, ws, "switch", "-q", "-c", "renovate/dep")
		write(t, ws, "go.mod", "module x\n\ngo 1.27\n")
		gittest.Run(t, ws, "add", "go.mod")
		gittest.Run(t, ws, "commit", "-q", "-m", "chore(deps): update go")
		head = strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
		gittest.Run(t, ws, "switch", "-q", "main")
		write(t, ws, "upstream.go", "package main\n")
		gittest.Run(t, ws, "add", "upstream.go")
		gittest.Run(t, ws, "commit", "-q", "-m", "main moves on")
		gittest.Run(t, ws, "switch", "-q", "renovate/dep")
		write(t, ws, filepath.Join(".claude", "settings.json"), "{\"hooks\": {}}\n")
		write(t, ws, "devbox.lock", "plugin_version: drifted\n")
		return ws, head
	}
	// alignedRepo is the head buildkit-operator#50's second pass read: the
	// bump, then the alignment an earlier pass committed on top of it —
	// main.go added, internal/x/y.go added, legacy.go removed.
	alignedRepo := func(t *testing.T) (ws, head string) {
		t.Helper()
		ws, _ = bumpRepo(t)
		write(t, ws, "main.go", aligned)
		write(t, ws, filepath.Join("internal", "x", "y.go"), "package x\n")
		gittest.Run(t, ws, "add", "main.go", filepath.Join("internal", "x", "y.go"))
		gittest.Run(t, ws, "rm", "-q", "legacy.go")
		gittest.Run(t, ws, "commit", "-q", "-m", "chore(deps): align code for the bump")
		head = strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
		return ws, head
	}

	t.Run("align re-applied what the head carries: nothing pending, claim carried", func(t *testing.T) {
		ws, head := alignedRepo(t)
		write(t, ws, "main.go", aligned)
		if p := read(t, ws, head, "main.go", "internal/x/y.go"); p.Pending || !p.Carried || len(p.Paths) != 0 {
			t.Fatalf("the #50 re-run must read nothing pending and the claim carried: %+v", p)
		}
	})

	t.Run("the engine writes staged by a blanket add are still not the alignment", func(t *testing.T) {
		// An agent's git add -A stages the mirror and the drifted lock along
		// with everything else: the index read carries the noise list too.
		ws, head := alignedRepo(t)
		write(t, ws, ".iterion-script-0a1b.py", "print(1)\n")
		gittest.Run(t, ws, "add", "-A")
		if p := read(t, ws, head, "main.go"); p.Pending || len(p.Paths) != 0 {
			t.Fatalf("engine writes staged by a blanket add read as pending: %+v", p)
		}
	})

	t.Run("a killed tool script's scratch is not the alignment", func(t *testing.T) {
		ws, head := alignedRepo(t)
		write(t, ws, ".iterion-script-0a1b.py", "print(1)\n")
		if p := read(t, ws, head, "main.go"); p.Pending || len(p.Paths) != 0 {
			t.Fatalf("the third canonical noise entry read as pending: %+v", p)
		}
	})

	t.Run("an edit the head lacks is pending, and named", func(t *testing.T) {
		ws, head := alignedRepo(t)
		write(t, ws, "main.go", aligned+"// and one more line\n")
		if p := read(t, ws, head, "main.go"); !p.Pending || len(p.Paths) != 1 || p.Paths[0] != "main.go" {
			t.Fatalf("an edit the head lacks must read pending and name only itself: %+v", p)
		}
	})

	t.Run("a new file the alignment adds is pending", func(t *testing.T) {
		ws, head := alignedRepo(t)
		write(t, ws, "adapter.go", "package main\n")
		if p := read(t, ws, head, "adapter.go"); !p.Pending || len(p.Paths) != 1 || p.Paths[0] != "adapter.go" {
			t.Fatalf("a new file git does not ignore is alignment work: %+v", p)
		}
	})

	t.Run("an edit staged then reverted in the tree is pending", func(t *testing.T) {
		// The commit carries the index, not the working tree.
		ws, head := alignedRepo(t)
		write(t, ws, "main.go", aligned+"// staged\n")
		gittest.Run(t, ws, "add", "main.go")
		write(t, ws, "main.go", aligned)
		if p := read(t, ws, head, "main.go"); !p.Pending || len(p.Paths) != 1 || p.Paths[0] != "main.go" {
			t.Fatalf("a staged edit the head lacks must read pending: %+v", p)
		}
	})

	t.Run("an alignment committed locally and never pushed is pending", func(t *testing.T) {
		// Measured against the commit the run READ, never the local HEAD.
		ws, head := alignedRepo(t)
		write(t, ws, "main.go", aligned+"// committed here\n")
		gittest.Run(t, ws, "add", "main.go")
		gittest.Run(t, ws, "commit", "-q", "-m", "local only")
		if p := read(t, ws, head, "main.go"); !p.Pending || len(p.Paths) != 1 || p.Paths[0] != "main.go" {
			t.Fatalf("a local commit the read head lacks must read pending: %+v", p)
		}
	})

	t.Run("work this run set aside in a stash is pending", func(t *testing.T) {
		// align's own prompt has it stash to test the base; a pop it skips
		// leaves a clean tree and the fix in the stash.
		ws, head := bumpRepo(t)
		write(t, ws, "main.go", aligned)
		gittest.Run(t, ws, "stash", "push", "-q", "--include-untracked", "--", "main.go")
		p := read(t, ws, head, "main.go")
		if !p.Pending || !strings.Contains(p.Reason, "stash") {
			t.Fatalf("a stash holding the alignment must read pending and say so: %+v", p)
		}
		if p.Carried {
			t.Fatalf("a claim the bare bump does not change must not read carried: %+v", p)
		}
	})

	t.Run("a stash of nothing but the engine's writes is not pending", func(t *testing.T) {
		// The #50 re-run whose align stashed to test the base and skipped the
		// pop: the stash holds the rewritten mirror and the drifted lock.
		ws, head := alignedRepo(t)
		gittest.Run(t, ws, "stash", "push", "-q")
		if p := read(t, ws, head, "main.go"); p.Pending {
			t.Fatalf("a stash of the tree noise alone must not read pending: %+v", p)
		}
	})

	t.Run("a stash of another commit is not this run's", func(t *testing.T) {
		// A local run's worktree shares refs/stash with the operator's checkout.
		ws, head := alignedRepo(t)
		gittest.Run(t, ws, "switch", "-q", "main")
		write(t, ws, "README.md", "the operator's own work in flight\n")
		gittest.Run(t, ws, "stash", "push", "-q", "--", "README.md")
		gittest.Run(t, ws, "switch", "-q", "renovate/dep")
		if p := read(t, ws, head, "main.go"); p.Pending {
			t.Fatalf("a stash based on another commit must not read pending: %+v", p)
		}
	})

	t.Run("a claim of edits that exist nowhere is not carried", func(t *testing.T) {
		// Reverted, reset away, or never written: the workspace matches the
		// head, and the head is the bare bump.
		ws, head := bumpRepo(t)
		p := read(t, ws, head, "main.go", "go.mod")
		if p.Pending || p.Carried || len(p.Unaccounted) != 1 || p.Unaccounted[0] != "main.go" {
			t.Fatalf("an edit claimed on a file the PR does not change must be unaccounted, the bump's own file accounted: %+v", p)
		}
	})

	t.Run("a file only the base changed after the PR forked is not carried", func(t *testing.T) {
		// The PR's range is merge-base..head, not base..head: main's own
		// change to upstream.go is not the PR's.
		ws, head := bumpRepo(t)
		if p := read(t, ws, head, "upstream.go"); p.Carried {
			t.Fatalf("a file only main changed must not read as carried by the PR: %+v", p)
		}
	})

	t.Run("a stale local base does not widen the range: the newest merge-base wins", func(t *testing.T) {
		// Local main left behind the PR's fork point, origin/main current:
		// the fork commit's own legacy.go must not read as the PR's.
		ws, head := bumpRepo(t)
		fresh := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "main"))
		gittest.Run(t, ws, "update-ref", "refs/remotes/origin/main", fresh)
		gittest.Run(t, ws, "branch", "-f", "main", "main~2")
		if p := read(t, ws, head, "legacy.go"); p.Carried {
			t.Fatalf("a stale local base must not carry a file the base itself changed: %+v", p)
		}
	})

	t.Run("a claim git C-quoted reads as the name it quotes", func(t *testing.T) {
		ws, _ := bumpRepo(t)
		name := "r" + string(rune(0xe9)) + "sum" + string(rune(0xe9)) + ".go"
		write(t, ws, name, "package main\n")
		gittest.Run(t, ws, "add", name)
		gittest.Run(t, ws, "commit", "-q", "-m", "chore(deps): align")
		head := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
		if p := read(t, ws, head, `"r\303\251sum\303\251.go"`); !p.Carried {
			t.Fatalf("a claim copied from git's quoted output must read carried: %+v", p)
		}
	})

	t.Run("a claim holding a NUL reads unaccounted, never crashes the probe", func(t *testing.T) {
		ws, head := alignedRepo(t)
		if p := read(t, ws, head, filepath.Join(ws, "main\x00.go"), "main.go"); p.Carried || len(p.Unaccounted) != 1 {
			t.Fatalf("a NUL claim must read unaccounted beside a carried one: %+v", p)
		}
	})

	t.Run("a stash --all of ignored files is not pending", func(t *testing.T) {
		ws, head := alignedRepo(t)
		if err := os.WriteFile(filepath.Join(ws, ".git", "info", "exclude"), []byte("build/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		write(t, ws, filepath.Join("build", "out.bin"), "artifact\n")
		gittest.Run(t, ws, "stash", "push", "-q", "--all")
		if p := read(t, ws, head, "main.go"); p.Pending {
			t.Fatalf("a stash of ignored build output and the tree noise must not read pending: %+v", p)
		}
	})

	t.Run("no merge-base with the base: nothing is carried", func(t *testing.T) {
		// A base branch with no history in common with the PR, built without
		// touching the workspace: one commit of git's empty tree.
		ws, head := alignedRepo(t)
		const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
		unrelated := gittest.Run(t, ws, "commit-tree", emptyTree, "-m", "unrelated history")
		gittest.Run(t, ws, "branch", "unrelated", unrelated)
		p := probeWith(t, ws, "unrelated", head, `["main.go"]`, same)
		if p.Carried || !strings.Contains(p.Reason, "no merge-base") {
			t.Fatalf("an approximate range must not carry a claim: %+v", p)
		}
	})

	t.Run("a claim the PR carries only in part is not carried", func(t *testing.T) {
		ws, head := alignedRepo(t)
		p := read(t, ws, head, "main.go", "other.go")
		if p.Carried || len(p.Unaccounted) != 1 || p.Unaccounted[0] != "other.go" {
			t.Fatalf("every claimed file must be changed by the PR: %+v", p)
		}
	})

	t.Run("a claimed directory is not carried: paths match exactly", func(t *testing.T) {
		// A directory claim would read carried through any file under it the
		// bump itself changed (a monorepo's package.json, a chart's
		// Chart.yaml); the contract is one file per entry.
		ws, head := alignedRepo(t)
		if p := read(t, ws, head, "internal/x/"); p.Carried || len(p.Unaccounted) != 1 || p.Unaccounted[0] != "internal/x" {
			t.Fatalf("a directory claim must read unaccounted: %+v", p)
		}
	})

	t.Run("claimed file paths are read as the agent may spell them", func(t *testing.T) {
		ws, head := alignedRepo(t)
		parent, name := filepath.Dir(ws), filepath.Base(ws)
		link := filepath.Join(t.TempDir(), "ws-link")
		if err := os.Symlink(ws, link); err != nil {
			t.Fatal(err)
		}
		for _, spelled := range []string{
			"./main.go",
			filepath.Join(ws, "main.go"),
			ws + "//main.go",
			parent + "/../" + filepath.Base(parent) + "/" + name + "/main.go",
			filepath.Join(link, "main.go"),
			"legacy.go -> main.go",
		} {
			js, _ := json.Marshal([]string{spelled})
			if p := probeWith(t, ws, "main", head, string(js), same); !p.Carried {
				t.Errorf("claim %q of a file the PR changes must read carried: %+v", spelled, p)
			}
		}
		// The workspace handed over as a symlink, the agent reporting the
		// resolved path (a tool's getcwd).
		js, _ := json.Marshal([]string{filepath.Join(ws, "main.go")})
		if p := probeWith(t, link, "main", head, string(js), same); !p.Carried {
			t.Errorf("a resolved-path claim under a symlinked workspace must read carried: %+v", p)
		}
	})

	t.Run("a claim spelled in the other Unicode normal form matches", func(t *testing.T) {
		ws, _ := bumpRepo(t)
		nfd := "re\u0301sume\u0301.go"
		write(t, ws, nfd, "package main\n")
		gittest.Run(t, ws, "add", nfd)
		gittest.Run(t, ws, "commit", "-q", "-m", "chore(deps): align")
		head := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
		if p := read(t, ws, head, "r\u00e9sum\u00e9.go"); !p.Carried {
			t.Fatalf("an NFC claim of an NFD-committed file must read carried: %+v", p)
		}
	})

	t.Run("a claimed engine write counts only if the PR changes it", func(t *testing.T) {
		ws, head := alignedRepo(t)
		if p := read(t, ws, head, "main.go", "devbox.lock", ".claude/settings.json"); !p.Carried {
			t.Fatalf("the mirror and the lock beside a carried claim must not hold the #50 re-run: %+v", p)
		}
		if p := read(t, ws, head, "devbox.lock"); p.Carried || !strings.Contains(p.Reason, "named only files the engine") {
			t.Fatalf("a claim of the engine's writes alone carries nothing: %+v", p)
		}
	})

	t.Run("an empty claim is not carried", func(t *testing.T) {
		ws, head := alignedRepo(t)
		if p := read(t, ws, head); p.Carried || !strings.Contains(p.Reason, "named no file") {
			t.Fatalf("an applied claim naming no file proves nothing on the branch: %+v", p)
		}
	})

	t.Run("a submodule counts when its recorded commit moves, not for dirt inside it", func(t *testing.T) {
		sub := t.TempDir()
		gittest.Run(t, sub, "init", "-q")
		write(t, sub, "lib.go", "package lib // v1\n")
		gittest.Run(t, sub, "add", "lib.go")
		gittest.Run(t, sub, "commit", "-q", "-m", "v1")
		v1 := strings.TrimSpace(gittest.Run(t, sub, "rev-parse", "HEAD"))
		write(t, sub, "lib.go", "package lib // v2\n")
		gittest.Run(t, sub, "commit", "-q", "-am", "v2")
		v2 := strings.TrimSpace(gittest.Run(t, sub, "rev-parse", "HEAD"))
		ws, _ := bumpRepo(t)
		gittest.Run(t, ws, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor/lib")
		gittest.Run(t, filepath.Join(ws, "vendor", "lib"), "checkout", "-q", v1)
		gittest.Run(t, ws, "config", "-f", ".gitmodules", "submodule.vendor/lib.ignore", "all")
		// git itself refuses to stage a submodule its .gitmodules ignores
		// unless forced: the PR records v1 only this way.
		gittest.Run(t, ws, "add", ".gitmodules")
		gittest.Run(t, ws, "add", "--force", "vendor/lib")
		gittest.Run(t, ws, "commit", "-q", "-m", "vendor the lib")
		head := strings.TrimSpace(gittest.Run(t, ws, "rev-parse", "HEAD"))
		if rec := gittest.Run(t, ws, "rev-parse", head+":vendor/lib"); rec != v1 {
			t.Fatalf("fixture: the PR must record the submodule at v1 %s, got %s", v1, rec)
		}
		// A build dirtying a file inside the submodule: no superproject commit
		// can carry that, so it is not work the branch lacks.
		write(t, filepath.Join(ws, "vendor", "lib"), "lib.go", "package lib // rebuilt\n")
		if p := read(t, ws, head, "vendor/lib"); p.Pending {
			t.Fatalf("dirt inside a submodule must not read pending: %+v", p)
		}
		gittest.Run(t, filepath.Join(ws, "vendor", "lib"), "checkout", "-q", "-f", v2)
		p := read(t, ws, head, "vendor/lib")
		if !p.Pending || len(p.Paths) != 1 || p.Paths[0] != "vendor/lib" {
			t.Fatalf("a submodule moved under ignore=all must read pending: %+v", p)
		}
		// The PR's own commits add the submodule: the range read must see it
		// through the same ignore setting.
		if !p.Carried {
			t.Fatalf("a submodule the PR adds under ignore=all must read carried: %+v", p)
		}
	})

	t.Run("an inherited git environment does not blind the probe", func(t *testing.T) {
		ws, head := alignedRepo(t)
		write(t, ws, "main.go", aligned+"// pending\n")
		p := probeWith(t, ws, "main", head, `["main.go"]`, func(e []string) []string {
			return append(e, "GIT_LITERAL_PATHSPECS=1", "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "index"))
		})
		if !p.Pending || len(p.Paths) != 1 || p.Paths[0] != "main.go" {
			t.Fatalf("an inherited GIT_LITERAL_PATHSPECS or GIT_INDEX_FILE changed what the probe reads: %+v", p)
		}
	})

	t.Run("a file name cannot break the comment the reason lands in", func(t *testing.T) {
		ws, head := alignedRepo(t)
		name := "x\n\n## Safe `bump` @octocat \u202eog.niam"
		write(t, ws, name, "x\n")
		p := read(t, ws, head, "main.go")
		if !p.Pending || len(p.Paths) != 1 || p.Paths[0] != name {
			t.Fatalf("the odd name must still read pending, verbatim in paths: %+v", p)
		}
		if strings.ContainsAny(p.Reason, "\n\r\u202e") || !strings.Contains(p.Reason, "`x??## Safe ?bump? @octocat ?og.niam`") {
			t.Fatalf("the reason must carry the name in one code span, control, format characters and backticks replaced: %q", p.Reason)
		}
	})

	t.Run("a claim too large for the environment still reads", func(t *testing.T) {
		// Past 128 KiB a single environment string stops python3 from
		// starting; the claim travels on stdin.
		ws, head := alignedRepo(t)
		claim := make([]string, 3000)
		for i := range claim {
			claim[i] = fmt.Sprintf("pkg/generated/file_%04d_with_a_long_enough_name.go", i)
		}
		js, err := json.Marshal(claim)
		if err != nil {
			t.Fatal(err)
		}
		p := probeWith(t, ws, "main", head, string(js), same)
		if p.Carried || len(p.Unaccounted) != 50 {
			t.Fatalf("a 3000-path claim must read, its unaccounted list capped at 50: carried=%v unaccounted=%d", p.Carried, len(p.Unaccounted))
		}
	})

	t.Run("a head git cannot resolve is undecided, and its stderr stays out of the reason", func(t *testing.T) {
		ws, _ := alignedRepo(t)
		p := read(t, ws, strings.Repeat("0", 40), "main.go")
		if !p.Pending || p.Carried || !strings.Contains(p.Reason, "undecided") || strings.Contains(p.Reason, "fatal") {
			t.Fatalf("an unreadable workspace must read pending and not carried, without git's stderr: %+v", p)
		}
	})

	t.Run("a claim that is not a list of paths is undecided", func(t *testing.T) {
		ws, head := alignedRepo(t)
		p := probeWith(t, ws, "main", head, `"main.go"`, same)
		if !p.Pending || p.Carried || !strings.Contains(p.Reason, "undecided") {
			t.Fatalf("a malformed claim must read pending and not carried: %+v", p)
		}
	})

	t.Run("without the noise list the workspace is not called clean", func(t *testing.T) {
		ws, head := alignedRepo(t)
		p := probeWith(t, ws, "main", head, `["main.go"]`, func(e []string) []string {
			var kept []string
			for _, kv := range e {
				if !strings.HasPrefix(kv, treenoise.TreeNoiseEnvVar+"=") {
					kept = append(kept, kv)
				}
			}
			return kept
		})
		if !p.Pending || p.Carried || !strings.Contains(p.Reason, treenoise.TreeNoiseEnvVar) {
			t.Fatalf("with no way to tell the engine's writes from the run's, the probe must hold and name the missing list: %+v", p)
		}
	})
}
