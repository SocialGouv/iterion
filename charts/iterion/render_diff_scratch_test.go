package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/testfs"
)

func TestChartRenderDiffKeepsScratchOutsideTheCheckoutAndCleansIt(t *testing.T) {
	scripts, err := filepath.Abs(filepath.Join("..", "..", "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct {
		name string
		code int
	}{
		{"equal", 0},
		{"different", 0},
		{"render-failed", 73},
		{"tmpdir-unset", 0},
		{"tmpdir-empty", 0},
	} {
		t.Run(mode.name, func(t *testing.T) {
			repo := gittest.SourceRepo(t)
			for _, dir := range []string{"charts/iterion", "scripts"} {
				if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range []string{"Chart.yaml", "values-dev.yaml"} {
				if err := os.WriteFile(filepath.Join(repo, "charts/iterion", file), []byte("fixture: true\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			deps, err := os.ReadFile(filepath.Join(scripts, "chart-check-deps.sh"))
			if err != nil {
				t.Fatal(err)
			}
			testfs.WriteExecutable(t, filepath.Join(repo, "scripts/chart-check-deps.sh"), deps)
			gittest.Run(t, repo, "add", ".")
			gittest.Run(t, repo, "commit", "-m", "chart fixture")
			bin := t.TempDir()
			scratch := filepath.Join(t.TempDir(), "scratch with spaces")
			if err := os.Mkdir(scratch, 0o700); err != nil {
				t.Fatal(err)
			}
			trace := filepath.Join(t.TempDir(), "rendered-paths")
			testfs.WriteExecutable(t, filepath.Join(bin, "helm"), []byte(`#!/bin/sh
set -eu
case "$1" in
  dependency) printf 'NAME\tVERSION\tREPOSITORY\tSTATUS\n' ;;
  template)
    test -f "$3/Chart.yaml"
    printf '%s\n' "$3" >> "$RENDER_PATHS"
    case "$RENDER_MODE" in
      render-failed) exit 73 ;;
      different) case "$3" in */after/*) echo changed; exit 0 ;; esac ;;
    esac
    echo baseline
    ;;
  *) exit 74 ;;
esac
`))
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(scripts, "chart-render-diff.sh"), "HEAD")
			cmd.Dir = repo
			cmd.Env = slices.DeleteFunc(gittest.Env(), func(value string) bool { return strings.HasPrefix(value, "TMPDIR=") })
			wantRoot := scratch
			switch mode.name {
			case "tmpdir-unset":
				wantRoot = "/tmp"
			case "tmpdir-empty":
				cmd.Env = append(cmd.Env, "TMPDIR=")
				wantRoot = "/tmp"
			default:
				cmd.Env = append(cmd.Env, "TMPDIR="+scratch)
			}
			cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"RENDER_PATHS="+trace, "RENDER_MODE="+mode.name)
			out, err := cmd.CombinedOutput()
			if code := exitCode(err); code != mode.code {
				t.Fatalf("render diff exit = %d, want %d: %s", code, mode.code, out)
			}
			if mode.name == "different" && !strings.Contains(string(out), "+changed") {
				t.Fatalf("the changed render was not reported: %s", out)
			}
			paths, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			wantRenders := 2
			if mode.name == "render-failed" {
				wantRenders = 1
			}
			rendered := strings.Split(strings.TrimSpace(string(paths)), "\n")
			if len(rendered) != wantRenders {
				t.Fatalf("rendered paths = %q, want %d renders", paths, wantRenders)
			}
			for _, path := range rendered {
				// .../<scratch>/<before|after>/charts/iterion
				work := filepath.Dir(filepath.Dir(filepath.Dir(path)))
				if filepath.Dir(work) == "/tmp" && strings.HasPrefix(filepath.Base(work), "chart-render-diff.") {
					// This run's observed scratch is outside t.TempDir when
					// TMPDIR is absent. Reclaim it even if the trap regresses.
					t.Cleanup(func() { testfs.RemoveAll(t, work, work) })
				}
				if filepath.Dir(work) != wantRoot {
					t.Errorf("render scratch %q is not in the selected temp root %q", work, wantRoot)
				}
				if _, err := os.Stat(work); !os.IsNotExist(err) {
					t.Errorf("render scratch %q survived exit %d: %v", work, mode.code, err)
				}
			}
		})
	}
}
