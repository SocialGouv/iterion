//go:build live

package delegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLive_Feat_OpenCodeProjectScreen is a CONFORMANCE bench, not a unit
// test: for each workspace layout it asks the REAL opencode CLI whether it
// loads a resource from that layout, and asks iterion's screen whether it
// refuses it — then requires that iterion never ACCEPT a layout the CLI
// loads from.
//
// It exists because three consecutive adversarial rounds found the screen's
// BOUNDS wrong (an unwalked ancestor, a lexical instead of physical path, a
// stop predicate stronger than the CLI's) while every spelling-level test
// stayed green. A guard that enumerates what the CLI reads cannot converge by
// enumeration; comparing against the CLI itself does, including for a
// divergence a future opencode release introduces.
//
// It needs no credential: opencode imports project plugins during startup,
// before any model turn, so a run that dies on a missing provider has already
// answered whether the plugin executed.
//
// Every row must OBSERVE the CLI loading something, or the row proves
// nothing: a stub named `opencode` first on PATH, a broken install or a
// sandbox with no network would otherwise turn the whole bench into six
// silent no-ops that report PASS.
func TestLive_Feat_OpenCodeProjectScreen(t *testing.T) {
	bin := findOpenCodeForBench(t)
	// One probe for the whole bench, under a scratch home of its own.
	major := openCodeMajorVersion(t, bin, openCodeBenchEnv(t))

	cases := []struct {
		name string
		// files are workspace-relative; a .ts is a plugin that marks on import.
		files []string
		// workRel is the directory the node would run in.
		workRel string
		// symlinkWork reaches workRel through a symlink, the shape a lexical
		// walk screens wrongly.
		symlinkWork bool
		// danglingGit plants a `.git` symlink pointing nowhere in workRel:
		// it exists to Lstat and not to Stat, so a stop predicate stronger
		// than the CLI's ends the walk where the CLI keeps climbing.
		danglingGit bool
		// wantLoaded pins what the CLI is expected to do, keyed by its MAJOR
		// version: the loader changed between the lines, and a row that
		// stops exercising the CLI must fail instead of passing vacuously.
		// A version this bench has not measured is refused rather than
		// guessed.
		wantLoaded map[int]bool
		// wantRefused pins ITERION's answer for the same layout, so the
		// bench compares two pinned sides: a screen that silently stopped
		// reaching a level reddens here, whichever way the CLI moved.
		wantRefused bool
	}{
		{name: "plugin at the workdir", files: []string{".opencode/plugin/pwn.ts"}, workRel: ".", wantLoaded: map[int]bool{1: true, 2: true}, wantRefused: true},
		// 2.x imports neither a `tool/` module nor a config `plugin:` list at
		// startup (measured on 2.2.5, with the default model as well as a
		// refused one: no marker, and no "loading plugin" in its own log).
		// iterion refuses these layouts anyway, which is fail-CLOSED.
		{name: "tool at the workdir", files: []string{".opencode/tool/pwn.ts"}, workRel: ".", wantLoaded: map[int]bool{1: true, 2: false}, wantRefused: true},
		{name: "root opencode.json with a plugin list", files: []string{"opencode.json", "evil.ts"}, workRel: ".", wantLoaded: map[int]bool{1: true, 2: false}, wantRefused: true},
		{name: "root opencode.jsonc with a plugin list", files: []string{"opencode.jsonc", "evil.ts"}, workRel: ".", wantLoaded: map[int]bool{1: true, 2: false}, wantRefused: true},
		{name: "plugin in an ancestor", files: []string{".opencode/plugin/pwn.ts"}, workRel: "sub/pkg", wantLoaded: map[int]bool{1: true, 2: true}, wantRefused: true},
		{name: "plugin at the git root", files: []string{".opencode/plugin/pwn.ts", ".git/HEAD"}, workRel: "sub", wantLoaded: map[int]bool{1: true, 2: true}, wantRefused: true},
		{name: "workdir reached through a symlink", files: []string{".opencode/plugin/pwn.ts"}, workRel: "sub", symlinkWork: true, wantLoaded: map[int]bool{1: true, 2: true}, wantRefused: true},
		{name: "ancestor hidden behind a dangling .git", files: []string{".opencode/plugin/pwn.ts", ".git/HEAD"}, workRel: "sub", danglingGit: true, wantLoaded: map[int]bool{1: true, 2: true}, wantRefused: true},
		// 1.x stopped at the git root; 2.x climbs STRAIGHT PAST it (measured
		// on 2.2.5: the marker is written, and the server subscribes to every
		// ancestor up to `/`). iterion's screen still stops at the git root,
		// so this row is the one layout where the CLI loads what iterion
		// accepts — the bench says so by failing, and the bound is the
		// operator's call, not this row's.
		{name: "plugin above the git root", files: []string{".opencode/plugin/pwn.ts", "repo/.git/HEAD"}, workRel: "repo/sub", wantLoaded: map[int]bool{1: false, 2: true}, wantRefused: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(t.TempDir(), "executed")
			writeHostileWorkspace(t, root, marker, tc.files)

			work := filepath.Join(root, filepath.FromSlash(tc.workRel))
			if err := os.MkdirAll(work, 0o750); err != nil {
				t.Fatal(err)
			}
			if tc.danglingGit {
				if err := os.Symlink(filepath.Join(root, "no", "such", "git"), filepath.Join(work, ".git")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			if tc.symlinkWork {
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(work, link); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				work = link
			}

			want, measured := tc.wantLoaded[major]
			if !measured {
				t.Fatalf("opencode %d.x is not measured by this row: re-measure it before trusting any verdict against this build", major)
			}

			loaded := openCodeLoadsFrom(t, bin, work, marker)
			if loaded != want {
				t.Fatalf("the CLI (%d.x) loaded=%v, want %v — this row no longer exercises what it names, so its verdict proves nothing", major, loaded, want)
			}

			refused := refuseUntrustedOpenCodeProject(work) != nil
			if refused != tc.wantRefused {
				t.Fatalf("iterion refused=%v, want %v — the screen no longer answers this layout the way the bench measured it", refused, tc.wantRefused)
			}
			if loaded && !refused {
				// The screen's bound, stated rather than implied: it stops at
				// the git worktree root, so a resource ABOVE the checkout is
				// the HOST's own tree — the line the home exclusion already
				// draws — and not a repository iterion was handed. 1.x did
				// not read there either; 2.x does (measured), so the gap is
				// real, and widening the screen is a decision for #2915's
				// owner, not for this row. Any layout AT or BELOW the
				// checkout carries wantRefused: true, so a fail-open there
				// reddens on the assertion above.
				t.Logf("opencode %d.x loaded a resource from %q, above the checkout, where iterion's screen does not reach", major, work)
			}
			if !loaded && refused {
				t.Logf("iterion refuses a layout the CLI did not load from (%q): fail-closed, but worth knowing", work)
			}
		})
	}
}

func findOpenCodeForBench(t *testing.T) string {
	t.Helper()
	if bin, err := exec.LookPath("opencode"); err == nil {
		return bin
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".opencode", "bin", "opencode")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skip("opencode CLI not installed")
	return ""
}

// writeHostileWorkspace materialises the layout, wiring every plugin/tool to
// touch marker at import time.
func writeHostileWorkspace(t *testing.T, root, marker string, files []string) {
	t.Helper()
	plugin := "import fs from \"fs\"\nfs.writeFileSync(" + strconv.Quote(marker) + ", \"x\")\nexport const P = async () => ({})\n"
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		content := plugin
		switch {
		case strings.HasSuffix(rel, "opencode.json"), strings.HasSuffix(rel, "opencode.jsonc"):
			content = `{"plugin":["./evil.ts"]}` + "\n"
		case strings.HasSuffix(rel, ".git/HEAD"):
			content = "ref: refs/heads/main\n"
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// openCodeBenchModel is a model reference no provider serves. The bench's
// question is whether the CLI LOADED the workspace's resources, which happens
// during startup, before any model turn — so refusing the model right after
// that is what keeps every row free, offline and instant. Measured on 2.2.5:
// exit 1 in 0.7 s with `{"error":{"type":"provider.no-route","message":"Model
// unavailable: …"}}` on stdout, the plugin's marker already written.
const openCodeBenchModel = "iterion-bench/no-such-model"

// openCodeLoadsFrom runs the real CLI in work and reports whether a workspace
// resource executed. The prompt goes on stdin: opencode blocks forever on an
// open stdin, and a run with NO message exits before loading anything, which
// would make every answer a false negative.
//
// argv comes from the PRODUCTION path — opencodeProtocol through buildArgs —
// so the bench cannot drift into a dialect the backend does not speak, and a
// change that breaks the argv breaks this bench too.
func openCodeLoadsFrom(t *testing.T, bin, work, marker string) bool {
	t.Helper()
	b := &CLIAgentBackend{Protocol: opencodeProtocol}
	task := Task{UserPrompt: "say hi", Model: openCodeBenchModel}
	args, stdinPrompt := b.buildArgs(opencodeProtocol, task, task.UserPrompt, "")
	// `--standalone` is a ROOT flag, so it precedes the subcommand: it gives
	// the invocation a PRIVATE server instead of 2.x's shared background
	// service, which the bench needs and a node does not. Measured on 2.2.5:
	// two runs carrying different scratch homes fight over the service's
	// fixed port ("Managed service port 49374 on 127.0.0.1 is already in use
	// by PID …") and the loser loads nothing — and a service started on a
	// scratch home would outlive the bench holding that port. It does not
	// change what the project loader reads: the plugin executes either way.
	args = append([]string{"--standalone"}, args...)
	cmd := exec.Command(bin, args...) // #nosec G204 — the located CLI, argv from the protocol.
	cmd.Stdin = strings.NewReader(stdinPrompt)
	cmd.Env = openCodeBenchEnv(t)
	// The same gesture the production spawn makes: opencode resolves its
	// project from PWD, so the bench would otherwise screen one directory
	// and ask the CLI about another.
	setSpawnWorkDir(cmd, work)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out

	done := make(chan struct{})
	go func() { _ = cmd.Run(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("opencode did not return")
	}
	// A CLI that refused its argv loaded NOTHING, and the absent marker
	// would read as "the layout is safe". Measured on 2.2.5: a flag before
	// the subcommand exits 1 with "Unrecognized flag: … in command opencode"
	// on stderr and the whole usage text on stdout — both of which this
	// buffer holds, so the refusal is what the bench must fail on.
	if text := out.String(); strings.Contains(text, "Unrecognized flag:") || strings.Contains(text, "USAGE\n  opencode") {
		t.Fatalf("opencode refused the bench's argv %v in %q — the bench is not exercising the CLI: %s", args, work, truncate(text, 600))
	}
	// A CLI that never started a turn cannot answer the security question:
	// require an event of its own stream, carrying the session it minted.
	// Read with the production parser, so what counts as an event is what
	// the backend itself reads.
	if parseOpenCodeOutput(out.String()).SessionID == "" {
		t.Fatalf("opencode started no session in %q — the bench is not exercising a working CLI: %s", work, truncate(out.String(), 600))
	}
	_, err := os.Stat(marker)
	return err == nil
}

// openCodeBenchEnv is the environment the bench hands the CLI: scratch HOME
// and XDG roots under the test's own directories, so the operator's opencode
// configuration, session database and CREDENTIALS never reach the run — a
// bench that borrowed them could spend real money, and a global config could
// decide what loads.
//
// The operator's own OPENCODE_* variables are DROPPED, never blanked: an
// empty value is still a value, and 2.x derives its managed service from
// those variables — measured, a run carrying `OPENCODE_DB=`/
// `OPENCODE_CONFIG_DIR=` fails with "Managed service port … is already in
// use", loads nothing and would answer the bench's question with silence.
// The run-level pins of the production path come from opencodeResolveEnv, so
// the bench never states them twice.
func openCodeBenchEnv(t *testing.T) []string {
	t.Helper()
	root := t.TempDir()
	scratch := map[string]string{}
	for _, home := range []struct{ env, dir string }{
		{"HOME", "home"}, {"XDG_CONFIG_HOME", "config"}, {"XDG_DATA_HOME", "data"},
		{"XDG_CACHE_HOME", "cache"}, {"XDG_STATE_HOME", "state"},
	} {
		dir := filepath.Join(root, home.dir)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		scratch[home.env] = dir
	}
	// No model fetch and no self-update: the bench is offline.
	scratch["OPENCODE_DISABLE_MODELS_FETCH"] = "1"
	scratch["OPENCODE_DISABLE_AUTOUPDATE"] = "1"
	// Cut the operator's ~/.claude out of the run: a malformed skill there
	// aborts the CLI before it loads anything, which reads as a false
	// negative (it silently faked two verdicts during the manual runs).
	for k, v := range opencodeResolveEnv(context.Background()) {
		scratch[k] = v
	}

	var env []string
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replaced := scratch[name]; replaced || strings.HasPrefix(name, "OPENCODE_") {
			continue
		}
		env = append(env, entry)
	}
	for name, value := range scratch {
		env = append(env, name+"="+value)
	}
	return env
}
