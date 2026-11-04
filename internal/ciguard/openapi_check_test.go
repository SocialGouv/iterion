package ciguard

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"gopkg.in/yaml.v3"
)

// Generators announce completion of their writes, then wait for the test.
// Source preservation is checked while both invocations are still alive.
const openAPIGenerator = `#!/usr/bin/env bash
set -eu
[ "$#" = 2 ]
printf '%s\n' "$GENERATED_SPEC" > "$1"
printf '%s\n' "$GENERATED_TYPES" > "$2"
printf 'generated\n'
IFS= read -r action
case "$action" in
  ok) exit 0 ;;
  fail) exit 37 ;;
  term) kill -TERM "$PPID"; exit 0 ;;
  *) exit 90 ;;
esac
`

type openAPICheck struct {
	cmd        *exec.Cmd
	input      io.WriteCloser
	output     *bufio.Reader
	stderr     bytes.Buffer
	dir        string
	originals  map[string]string
	identities map[string]os.FileInfo
	waited     bool
}

func startOpenAPICheck(t *testing.T, dir, temporary, name string, drift bool, setup ...func(string)) *openAPICheck {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		gittest.InitRepo(t, dir)
	} else if err != nil {
		t.Fatal(err)
	}
	var tasks taskfileTasks
	if err := yaml.Unmarshal(mustRead(t, taskfilePath), &tasks); err != nil {
		t.Fatal(err)
	}
	commands := tasks.Tasks["openapi:check:dry"].Cmds
	if len(commands) != 1 || commands[0].Kind != yaml.ScalarNode {
		t.Fatal("dry check must run one private generation scope")
	}
	script := scriptUnderTest(t, "../../scripts/ci/check-openapi.sh")
	writeFiles(t, dir, map[string]string{
		"scripts/ci/check-openapi.sh":    string(mustRead(t, script)),
		"scripts/ci/generate-openapi.sh": openAPIGenerator,
	})
	spec, types := "spec-"+name, "types-"+name
	commit(t, dir, "generated "+name, map[string]string{"openapi.json": spec + "\n", "studio/src/api/schema.ts": types + "\n"})
	c := &openAPICheck{dir: dir, originals: map[string]string{"openapi.json": "dirty-spec-" + name + "\n", "studio/src/api/schema.ts": "dirty-types-" + name + "\n"}, identities: map[string]os.FileInfo{}}
	writeFiles(t, dir, c.originals)
	for _, configure := range setup {
		configure(dir)
	}
	for path := range c.originals {
		info, err := os.Lstat(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		c.identities[path] = info
	}
	if drift {
		spec += "-drift"
	}
	c.cmd = exec.Command("bash", "-c", commands[0].Value)
	c.cmd.Dir = dir
	c.cmd.Env = append(scriptEnv(t), "TMPDIR="+temporary, "GENERATED_SPEC="+spec, "GENERATED_TYPES="+types)
	c.cmd.Stderr = &c.stderr
	pipe, err := c.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.output = bufio.NewReader(pipe)
	c.input, err = c.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !c.waited {
			_, _ = io.WriteString(c.input, "fail\n")
			_ = c.input.Close()
			_ = c.cmd.Wait()
		}
	})
	line, err := c.output.ReadString('\n')
	if err != nil || line != "generated\n" {
		_ = c.input.Close()
		_ = c.cmd.Wait()
		c.waited = true
		t.Fatalf("generator handshake: %q, %v; %s", line, err, c.stderr.String())
	}
	c.assertOriginals(t)
	return c
}

func (c *openAPICheck) assertOriginals(t *testing.T) {
	t.Helper()
	for path, want := range c.originals {
		full := filepath.Join(c.dir, path)
		got, err := os.ReadFile(full)
		if err != nil {
			t.Error(err)
			continue
		}
		if string(got) != want {
			t.Errorf("source %s changed to %q; want %q", path, got, want)
		}
		info, err := os.Lstat(full)
		if err != nil {
			t.Error(err)
			continue
		}
		before := c.identities[path]
		if !os.SameFile(before, info) || before.Mode() != info.Mode() || !before.ModTime().Equal(info.ModTime()) {
			t.Errorf("source %s identity/mode/mtime changed", path)
		}
	}
}

func (c *openAPICheck) finish(t *testing.T, action string, want int) {
	t.Helper()
	if _, err := io.WriteString(c.input, action+"\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.input.Close()
	out, readErr := io.ReadAll(c.output)
	err := c.cmd.Wait()
	c.waited = true
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	if readErr != nil || code != want {
		t.Errorf("check exit=%d want=%d read=%v\n%s%s", code, want, readErr, out, c.stderr.String())
	}
	c.assertOriginals(t)
}

func TestOpenAPICheckPreservesOverlappingWorktrees(t *testing.T) {
	temporary := t.TempDir()
	writeFiles(t, temporary, map[string]string{"unrelated": "keep\n"})
	first, second := t.TempDir(), t.TempDir()
	gittest.InitRepo(t, first)
	commit(t, first, "base", map[string]string{"seed": "base\n"})
	gittest.Run(t, first, "worktree", "add", "--detach", second, "HEAD")
	t.Cleanup(func() { gittest.Run(t, first, "worktree", "remove", "--force", second) })
	a := startOpenAPICheck(t, first, temporary, "alpha", false)
	b := startOpenAPICheck(t, second, temporary, "beta", false)
	a.finish(t, "ok", 0)
	b.finish(t, "ok", 0)
	entries, err := os.ReadDir(temporary)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "unrelated" || string(mustRead(t, filepath.Join(temporary, "unrelated"))) != "keep\n" {
		t.Errorf("cleanup leaked or removed foreign scratch: %v", entries)
	}
}

func TestOpenAPICheckPreservesOriginalsAndAliases(t *testing.T) {
	for _, link := range []string{"regular", "symlink", "hardlink"} {
		for _, path := range []string{"openapi.json", "studio/src/api/schema.ts"} {
			for _, tc := range []struct {
				name, action string
				drift        bool
				code         int
			}{{"fresh", "ok", false, 0}, {"drift", "ok", true, 1}, {"failure", "fail", false, 37}, {"cancelled", "term", false, 143}} {
				t.Run(fmt.Sprintf("%s/%s/%s", link, path, tc.name), func(t *testing.T) {
					dir, temporary := t.TempDir(), t.TempDir()
					var alias string
					var aliasInfo os.FileInfo
					c := startOpenAPICheck(t, dir, temporary, "alias", tc.drift, func(dir string) {
						source := filepath.Join(dir, path)
						alias = filepath.Join(dir, "foreign")
						switch link {
						case "symlink":
							if err := os.Rename(source, alias); err != nil {
								t.Fatal(err)
							}
							if err := os.Symlink(alias, source); err != nil {
								t.Fatal(err)
							}
						case "hardlink":
							if err := os.Link(source, alias); err != nil {
								t.Fatal(err)
							}
						}
						if link != "regular" {
							var err error
							aliasInfo, err = os.Stat(alias)
							if err != nil {
								t.Fatal(err)
							}
						}
					})
					c.finish(t, tc.action, tc.code)
					if link != "regular" {
						if got := string(mustRead(t, alias)); got != c.originals[path] {
							t.Errorf("foreign alias changed: %q", got)
						}
						after, err := os.Stat(alias)
						if err != nil {
							t.Fatal(err)
						}
						if !os.SameFile(aliasInfo, after) {
							t.Error("foreign alias inode changed")
						}
						local, err := os.Stat(filepath.Join(dir, path))
						if err != nil {
							t.Fatal(err)
						}
						if !os.SameFile(local, after) {
							t.Error("source no longer shares its original alias")
						}
					}
					entries, err := os.ReadDir(temporary)
					if err != nil {
						t.Fatal(err)
					}
					if len(entries) != 0 {
						t.Errorf("private generated files remain: %v", entries)
					}
				})
			}
		}
	}
}

func TestOpenAPICheckComparesTheIndexRatherThanHEAD(t *testing.T) {
	dir, temporary := t.TempDir(), t.TempDir()
	c := startOpenAPICheck(t, dir, temporary, "index", false, func(dir string) {
		original := mustRead(t, filepath.Join(dir, "openapi.json"))
		writeFiles(t, dir, map[string]string{"openapi.json": "a different staged spec\n"})
		gittest.Run(t, dir, "add", "openapi.json")
		writeFiles(t, dir, map[string]string{"openapi.json": string(original)})
	})
	c.finish(t, "ok", 1)
}

func TestOpenAPIGeneratorHonoursBothOutputPaths(t *testing.T) {
	var tasks taskfileTasks
	if err := yaml.Unmarshal(mustRead(t, taskfilePath), &tasks); err != nil {
		t.Fatal(err)
	}
	commands := tasks.Tasks["openapi:gen"].Cmds
	if len(commands) != 1 || commands[0].Kind != yaml.ScalarNode {
		t.Fatal("normal generation must use the shared recipe")
	}
	generator, err := filepath.Abs(scriptUnderTest(t, "../../scripts/ci/generate-openapi.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("relative=%t", relative), func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{
				"openapi.json": "unchanged spec\n", "studio/src/api/schema.ts": "unchanged types\n",
				"bin/go": `#!/usr/bin/env bash
set -eu
[ "$#" = 6 ]
[ "$1 $2 $3 $4 $5" = 'run -mod=vendor ./cmd/iterion openapi -o' ]
printf 'private spec\n' > "$6"
`,
				"bin/corepack": `#!/usr/bin/env bash
set -eu
[ "$#" = 8 ]
[ "$1 $2 $3 $4 $5" = 'pnpm -C studio exec openapi-typescript' ]
[ "$7" = '-o' ]
case "$6" in /*) ;; *) exit 42 ;; esac
case "$8" in /*) ;; *) exit 43 ;; esac
[ "$(cat "$6")" = 'private spec' ]
printf 'private types\n' > "$8"
`,
			})
			for _, tool := range []string{"go", "corepack"} {
				if err := os.Chmod(filepath.Join(dir, "bin", tool), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			outputs := filepath.Join(dir, "other path")
			if err := os.Mkdir(outputs, 0o755); err != nil {
				t.Fatal(err)
			}
			spec, types := filepath.Join(outputs, "spec.json"), filepath.Join(outputs, "types.ts")
			args := []string{generator, spec, types}
			if relative {
				args = []string{generator, "other path/spec.json", "other path/types.ts"}
			}
			cmd := exec.Command("bash", args...)
			cmd.Dir = dir
			cmd.Env = append(scriptEnv(t), "PATH="+filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("generation: %v\n%s", err, out)
			}
			for path, want := range map[string]string{spec: "private spec\n", types: "private types\n", filepath.Join(dir, "openapi.json"): "unchanged spec\n", filepath.Join(dir, "studio/src/api/schema.ts"): "unchanged types\n"} {
				if got := string(mustRead(t, path)); got != want {
					t.Errorf("%s: %q want %q", path, got, want)
				}
			}
			// The normal Taskfile entry calls this same helper with its defaults.
			writeFiles(t, dir, map[string]string{"scripts/ci/generate-openapi.sh": string(mustRead(t, generator))})
			cmd = exec.Command("bash", "-c", commands[0].Value)
			cmd.Dir = dir
			cmd.Env = append(scriptEnv(t), "PATH="+filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("normal generation: %v\n%s", err, out)
			}
			if got := string(mustRead(t, filepath.Join(dir, "openapi.json"))); !strings.HasPrefix(got, "private spec") {
				t.Errorf("default spec not generated: %q", got)
			}
			if got := string(mustRead(t, filepath.Join(dir, "studio/src/api/schema.ts"))); !strings.HasPrefix(got, "private types") {
				t.Errorf("default types not generated: %q", got)
			}
		})
	}
}
