package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// One net can declare a second ENVIRONMENT for the same corpus — a second
// database engine, a second runtime — and judging it is replaying the same
// references against the app booted the other way. GM_CONFIG names the
// declaration to judge; the verdict carries it; a named declaration that is
// absent refuses instead of falling back.
//
// Measured on a campaign: an outcome check ran `GM_CONFIG=config-pg.json` for
// three lots against a harness that read config.json and would have reported
// the second engine met the moment the first was green.
func TestGoldenMasterHarnessNamesTheConfig(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	harness, err := filepath.Abs("golden-master/oracle-harness.py")
	if err != nil {
		t.Fatal(err)
	}
	// A net with two declarations and NO corpus: the gate bails before
	// booting anything, and the report it prints is what an operator reads.
	newNet := func(t *testing.T) string {
		t.Helper()
		ws := t.TempDir()
		gm := filepath.Join(ws, ".golden-master")
		if err := os.MkdirAll(gm, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"config.json":    `{"up": "app-up.sh", "seal_committed": true}`,
			"config-pg.json": `{"up": "pg-up.sh"}`,
		} {
			if err := os.WriteFile(filepath.Join(gm, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return ws
	}
	run := func(t *testing.T, ws string, env ...string) map[string]any {
		t.Helper()
		cmd := exec.Command("python3", harness)
		cmd.Dir = ws
		cmd.Env = append(append(os.Environ(),
			"GM_MODE=gate", "GM_WORKSPACE="+ws, "GM_DIR=.golden-master"), env...)
		out, _ := cmd.Output()
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		var report map[string]any
		if uerr := json.Unmarshal([]byte(lines[len(lines)-1]), &report); uerr != nil {
			t.Fatalf("the harness printed no report: %v (out %q)", uerr, out)
		}
		return report
	}

	t.Run("the verdict carries the declaration it judged", func(t *testing.T) {
		ws := newNet(t)
		if got := run(t, ws)["config"]; got != "config.json" {
			t.Fatalf("default config not named in the verdict: %v", got)
		}
		r := run(t, ws, "GM_CONFIG=config-pg.json")
		if got := r["config"]; got != "config-pg.json" {
			t.Fatalf("a green from the second environment and one from the first would read the same: %v", got)
		}
	})

	t.Run("a named declaration that is absent refuses, and says so", func(t *testing.T) {
		r := run(t, newNet(t), "GM_CONFIG=config-absent.json")
		tail, _ := r["log_tail"].(string)
		if !strings.Contains(tail, "config-absent.json") || !strings.Contains(tail, "nobody asked about") {
			t.Fatalf("the refusal must name the missing declaration instead of judging the other one: %q", tail)
		}
		if got := r["config"]; got != "config-absent.json" {
			t.Fatalf("the report must still name what was asked for: %v", got)
		}
	})

	t.Run("a path is refused: the declaration lives with the corpus", func(t *testing.T) {
		ws := newNet(t)
		cmd := exec.Command("python3", harness)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GM_MODE=gate", "GM_WORKSPACE="+ws,
			"GM_DIR=.golden-master", "GM_CONFIG=../config.json")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("a path outside the net's directory was accepted: %s", out)
		}
		if !strings.Contains(string(out), "is a path") {
			t.Fatalf("the refusal must say why: %s", out)
		}
	})
}
