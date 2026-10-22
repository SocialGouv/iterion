package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every process that expands ${ITERION_*} installs the bot_vars overlay
// through platformcfg.EnvOverlayFor — the record read through
// platformcfg.BotVarsOverlay, the one lookup that re-checks a stored value
// against the write rule before handing it out, with the process's env pins
// ahead of it (TestEnvOverlayFor_ComposesTheCheckedOverlay holds that
// composition). A hand-rolled closure anywhere — a third cloud process, a
// helper re-installing after boot (the last install wins) — would hand out
// whatever the record holds. The scan covers every non-test Go file of the
// module, so a new installer is judged the day it lands; the two known ones
// must still be found.
func TestBotVarsOverlayIsInstalledThroughTheCheckedLookup(t *testing.T) {
	root := moduleRoot(t)
	const call = "SetEnvOverlay("
	installers := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(src)
		rel, _ := filepath.Rel(root, path)
		for i := strings.Index(body, call); i >= 0; {
			rest := body[i+len(call):]
			switch {
			case strings.HasPrefix(body[max(0, i-len("func ")):i], "func "):
				// The definition in pkg/dsl/ir.
			case strings.HasPrefix(rest, "nil)"):
				// Restoring env-only behaviour.
			case strings.HasPrefix(rest, "platformcfg.EnvOverlayFor("):
				// The pin list comes from the PROCESS environment: a stub
				// env argument would read no pin and hand a pool pod the
				// record on its own dials.
				if !strings.HasPrefix(rest, "platformcfg.EnvOverlayFor(botVarsResolver, os.Getenv,") {
					t.Errorf("%s installs EnvOverlayFor without os.Getenv as its env — the pin list would not be read from the pod: %.80q", rel, rest)
				}
				installers[filepath.ToSlash(rel)] = true
			default:
				t.Errorf("%s installs the env overlay without platformcfg.EnvOverlayFor — stored values would skip the read-side check, or the process's env pins: %.60q", rel, rest)
			}
			next := strings.Index(rest, call)
			if next < 0 {
				break
			}
			i += len(call) + next
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for _, want := range []string{"cmd/iterion/runner.go", "cmd/iterion/server.go"} {
		if !installers[want] {
			t.Errorf("%s no longer installs the checked overlay — found %v; the scan may be looking at the wrong tree", want, installers)
		}
	}
}

// moduleRoot walks up from the test's directory to the one holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}
