package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	// The path a gate actually takes, which neither the direct call nor the
	// selftest covered: `verify-oracle.sh` runs the selftest as a BLOCKING
	// step with the environment inherited, so an ambient GM_CONFIG would make
	// the one documented invocation of a second environment die red before
	// judging anything — accusing the decision rules of what is the fixtures'
	// shape. The selftest judges its own fixtures; it must not read the
	// operator's choice about their net.
	t.Run("the selftest is hermetic to every operator input", func(t *testing.T) {
		// Not GM_CONFIG alone: an operator input meant for the NET applied to
		// the doubles is one class, and the same count must come out of each
		// run — a guard that made fixtures vanish instead of hermetic would
		// pass this loop while testing less. The count is read from the
		// unset run rather than written here: pinning a number would fail
		// every branch that adds a check, and the claim is EQUALITY.
		want := ""
		for _, env := range [][]string{
			nil,
			{"GM_CONFIG=config-pg.json"},
			{"GM_CONFIG="},
			{"GM_SEAL_COMMITTED=1"},
			{"GM_SEALED_DIR=" + filepath.Join(t.TempDir(), "pile")},
			{"GM_MUTATION_FLOOR=1"},
			{"GM_CONFIG=config-pg.json", "GM_SEAL_COMMITTED=1"},
			// Not only the judge's own variables: git's identity ENV
			// outranks every config source, so a host that exports it made
			// every fixture commit under the caller's name and turned the
			// author checks red — in a step the gate wrapper runs BLOCKING.
			{"GIT_AUTHOR_EMAIL=ambient@host", "GIT_COMMITTER_EMAIL=ambient@host",
				"GIT_AUTHOR_NAME=ambient", "GIT_COMMITTER_NAME=ambient"},
		} {
			cmd := exec.Command("python3", harness)
			cmd.Dir = t.TempDir()
			cmd.Env = append(append(os.Environ(), "GM_MODE=selftest"), env...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v made the selftest fail — the gate wrapper runs it as a blocking step: %v\n%s", env, err, out)
			}
			m := regexp.MustCompile(`harnais : (\d+) verifications passent`).FindStringSubmatch(string(out))
			if m == nil {
				t.Fatalf("%v: no check count in the selftest output:\n%s", env, out)
			}
			if want == "" {
				want = m[1]
				continue
			}
			if m[1] != want {
				t.Fatalf("%v changed WHICH checks ran: %s instead of %s", env, m[1], want)
			}
		}
	})

	// And the same thing through the wrapper the workflow EMITS, which is the
	// only invocation the skill documents: `GM_CONFIG=<name> sh
	// .golden-master/verify-oracle.sh`. The wrapper runs the selftest as a
	// blocking step before judging, so this is the path a gate takes and the
	// one neither the direct call nor the selftest could see.
	t.Run("the emitted wrapper survives its selftest step and refuses for the right reason", func(t *testing.T) {
		requireModernizeTools(t)
		ws := t.TempDir()
		gm := filepath.Join(ws, ".golden-master")
		if err := os.MkdirAll(filepath.Join(gm, "canon"), 0o755); err != nil {
			t.Fatal(err)
		}
		real, err := os.ReadFile(harness)
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string][]byte{
			"canon/test_rules.py": []byte("print('ok')\n"),
			"harness.py":          real,
			"config.json":         []byte(`{"up": "true", "base_url": "http://127.0.0.1:1"}`),
			"corpus.json":         []byte(`{"entries": []}`),
		} {
			if werr := os.WriteFile(filepath.Join(gm, name), body, 0o644); werr != nil {
				t.Fatal(werr)
			}
		}
		runner := emitRunner(t, ws)
		cmd := exec.Command("sh", runner)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GM_CONFIG=config-pg.json", "GM_WORKSPACE="+ws)
		out, _ := cmd.CombinedOutput()
		if strings.Contains(string(out), "RÈGLE DE DÉCISION") {
			t.Fatalf("the gate died on its own selftest under GM_CONFIG — the documented invocation of a second environment never reaches a verdict:\n%s", out)
		}
		if !strings.Contains(string(out), "config-pg.json") {
			t.Fatalf("the refusal must name the declaration that is missing, not something else:\n%s", out)
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
