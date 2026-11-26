package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ultra11y 9.6.1 exits 1 on two outcomes that still write their result: a
// `scan --sample` whose sample is partial (a page refused and named, the
// merged audit written) and a `verify --apply` fold the gate refused part of
// (the accepted verdicts written, `ok: false`). The bot's helper library
// reads both as data and everything else — exit 2, a signal, no JSON, an
// exit that contradicts `ok` — as a crash. Driven on the library alone, with
// the engine's result shapes, so it runs without the engine.
func TestRgaaAuditLibReadsTheEngineExitContract(t *testing.T) {
	scripts := rgaaBotScripts(t)
	run := t.TempDir()
	lib := rgaaRenderScript(t, "prepare", extractHelpers(t, scripts["prepare"]), map[string]any{
		"input.run_dir": run, "input.workspace_dir": run, "input.standard": "rgaa", "run.id": "exit-contract",
	})
	libPath := filepath.Join(run, "lib.js")
	if err := os.WriteFile(libPath, []byte("module.exports = ("+lib+")();\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	call := func(t *testing.T, fn string, r any) (code int, stdout, stderr string) {
		t.Helper()
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		js := "const L = require(" + strconvQuote(libPath) + ").init(" + strconvQuote(run) + "); process.stdout.write(JSON.stringify(L." + fn + "(" + string(raw) + ", 'the fold') ?? null));"
		cmd := exec.Command("node", "-e", js)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err = cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), out.String(), errb.String()
		} else if err != nil {
			t.Fatal(err)
		}
		return 0, out.String(), errb.String()
	}

	t.Run("scanOutcome", func(t *testing.T) {
		pages := func(recorded []string, refused ...map[string]any) map[string]any {
			if refused == nil {
				refused = []map[string]any{}
			}
			return map[string]any{"pages": map[string]any{"sample": 2, "recorded": recorded, "refused": refused}, "kind": "pack-audit"}
		}
		contact := map[string]any{"id": "contact", "reason": "http-status"}
		for _, c := range []struct {
			name        string
			r           map[string]any
			ok, partial bool
			refused     int
		}{
			{"whole sample", map[string]any{"code": 0, "json": pages([]string{"accueil", "contact"})}, true, false, 0},
			{"partial sample, audit written", map[string]any{"code": 1, "json": pages([]string{"accueil"}, contact)}, true, true, 1},
			{"no page recorded", map[string]any{"code": 1, "json": pages([]string{}, contact, map[string]any{"id": "accueil", "reason": "redirect"})}, false, false, 2},
			{"exit 1 without JSON", map[string]any{"code": 1, "json": nil}, false, false, -1},
			{"exit 1 without a refused page", map[string]any{"code": 1, "json": pages([]string{"accueil"})}, false, false, 0},
			{"usage", map[string]any{"code": 2, "json": pages([]string{"accueil"}, contact)}, false, false, 1},
		} {
			t.Run(c.name, func(t *testing.T) {
				code, stdout, stderr := call(t, "scanOutcome", c.r)
				if code != 0 {
					t.Fatalf("scanOutcome died (exit %d): %s", code, stderr)
				}
				var got struct {
					OK      bool              `json:"ok"`
					Partial bool              `json:"partial"`
					Refused []json.RawMessage `json:"refused"`
				}
				if err := json.Unmarshal([]byte(stdout), &got); err != nil {
					t.Fatalf("%v: %s", err, stdout)
				}
				refused := len(got.Refused)
				if got.Refused == nil {
					refused = -1
				}
				if got.OK != c.ok || got.Partial != c.partial || refused != c.refused {
					t.Fatalf("ok=%v partial=%v refused=%d, want %v/%v/%d", got.OK, got.Partial, refused, c.ok, c.partial, c.refused)
				}
			})
		}
	})

	t.Run("foldJSON", func(t *testing.T) {
		for _, c := range []struct {
			name string
			r    map[string]any
			dies string
		}{
			{"complete fold", map[string]any{"code": 0, "json": map[string]any{"ok": true, "applied": 2, "rejectedCriteria": []string{}}}, ""},
			{"incomplete fold, audit written", map[string]any{"code": 1, "json": map[string]any{"ok": false, "auditPath": "/run/audit-latest.json", "applied": 1, "rejectedCriteria": []string{"1.2"}}}, ""},
			{"nothing landed", map[string]any{"code": 1, "json": map[string]any{"ok": false, "applied": 0, "rejectedCriteria": []string{"1.2"}}}, ""},
			{"usage", map[string]any{"code": 2, "json": map[string]any{"ok": false}, "stderr": "--apply requires --in"}, "exit 2 (usage / invalid input)"},
			{"signal", map[string]any{"code": nil, "json": nil, "stderr": ""}, "killed by a signal"},
			{"crash without JSON", map[string]any{"code": 1, "json": nil, "stderr": "TypeError"}, "printed no JSON"},
			{"exit 1 claiming ok", map[string]any{"code": 1, "json": map[string]any{"ok": true}}, "contradicts"},
			{"exit 0 claiming a refusal", map[string]any{"code": 0, "json": map[string]any{"ok": false}}, "contradicts"},
		} {
			t.Run(c.name, func(t *testing.T) {
				code, _, stderr := call(t, "foldJSON", c.r)
				if c.dies == "" {
					if code != 0 {
						t.Fatalf("an expected fold outcome died (exit %d): %s", code, stderr)
					}
					return
				}
				if code == 0 || !strings.Contains(stderr, c.dies) || !strings.Contains(stderr, "the fold") {
					t.Fatalf("exit %d, stderr %q: want a non-zero death naming %q", code, stderr, c.dies)
				}
			})
		}
	})
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
