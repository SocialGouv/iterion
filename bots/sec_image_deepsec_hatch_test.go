package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #2350 — the sec image's deepsec assertion is fail-closed by DEFAULT and
// keeps one greppable escape hatch, `--build-arg REQUIRE_DEEPSEC=0`, for
// the case the refusal would otherwise take hostage: a local rebuild of
// this same Dockerfile (docs/agents/ops/security-selfaudit.md,
// scripts/bench/ci-build-bench.sh) on a machine that cannot reach npm or
// NodeSource, where the Layer 4 install degrades behind its `|| echo`
// while every MANDATORY scanner is installed. A bound with no override is
// a defect (docs/philosophy.md); a refusal that cannot be overridden on
// purpose turns a documented local recipe into no image at all.
//
// The witness RUNS the Dockerfile's own sanity block under /bin/sh,
// against a stub `deepsec` on an otherwise empty PATH, so each verdict is
// the shell's and not a reading of it.

// deepsecSanityScript extracts the deepsec branch of the Layer 5 sanity
// RUN — the text the image really executes, located by its first and last
// line rather than by a line number.
func deepsecSanityScript(t *testing.T) (script, pin string) {
	t.Helper()
	body, err := os.ReadFile(secImageDockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", secImageDockerfile, err)
	}
	src := string(body)

	m := deepsecPinArgRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s declares no ARG DEEPSEC_VERSION", secImageDockerfile)
	}
	pin = m[1]

	const firstMarker = `DEEPSEC_FOUND="$(deepsec --version`
	start := strings.Index(src, firstMarker)
	if start == -1 {
		t.Fatalf("%s has no deepsec sanity step (%q)", secImageDockerfile, firstMarker)
	}
	if lineStart := strings.LastIndexByte(src[:start], '\n'); lineStart != -1 {
		start = lineStart + 1
	}
	rest := src[start:]
	end := strings.Index(rest, "\n    fi")
	if end == -1 {
		t.Fatalf("the deepsec sanity step is not a closed `if ... fi` block")
	}
	script = strings.TrimSpace(rest[:end+len("\n    fi")])
	script = strings.TrimPrefix(script, "&& ")
	if !strings.Contains(script, "exit 1") {
		t.Fatalf("the extracted sanity block carries no refusal: %q", script)
	}
	return script, pin
}

// stubDeepsecPATH writes a `deepsec` that prints version (empty = install
// no stub at all, the ABSENT case) and returns a PATH holding only it.
func stubDeepsecPATH(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	if version != "" {
		stub := filepath.Join(dir, "deepsec")
		if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \""+version+"\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSecImageDeepsecSanityIsFailClosedWithOneHatch(t *testing.T) {
	script, pin := deepsecSanityScript(t)

	for _, tc := range []struct {
		name     string
		served   string // what the stub `deepsec --version` prints; "" = absent
		require  string // REQUIRE_DEEPSEC
		wantFail bool
		wantSays string
	}{
		{"pinned/required", pin, "1", false, "present at the pinned " + pin},
		{"absent/required", "", "1", true, "ABSENT"},
		{"drift/required", "9.9.9", "1", true, "version drift"},
		{"absent/hatch", "", "0", false, "allowed by --build-arg REQUIRE_DEEPSEC=0"},
		{"drift/hatch", "9.9.9", "0", false, "allowed by --build-arg REQUIRE_DEEPSEC=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = []string{
				"PATH=" + stubDeepsecPATH(t, tc.served),
				"DEEPSEC_VERSION=" + pin,
				"REQUIRE_DEEPSEC=" + tc.require,
			}
			out, err := cmd.CombinedOutput()
			failed := err != nil
			if failed != tc.wantFail {
				t.Fatalf("deepsec=%q REQUIRE_DEEPSEC=%s: build failed=%v (want %v); output:\n%s",
					tc.served, tc.require, failed, tc.wantFail, out)
			}
			if !strings.Contains(string(out), tc.wantSays) {
				t.Fatalf("deepsec=%q REQUIRE_DEEPSEC=%s: output does not name %q:\n%s",
					tc.served, tc.require, tc.wantSays, out)
			}
		})
	}
}

// The hatch is an ARG, and its default is the refusal: a build that
// passes nothing — every CI build of the published image — gets the
// fail-closed assertion.
func TestSecImageDeepsecHatchDefaultsToRequired(t *testing.T) {
	body, err := os.ReadFile(secImageDockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", secImageDockerfile, err)
	}
	src := string(body)
	if !strings.Contains(src, "\nARG REQUIRE_DEEPSEC=1\n") {
		t.Fatalf("%s must declare `ARG REQUIRE_DEEPSEC=1` — without the ARG docker rejects the --build-arg, and without the =1 default the published build would not be the fail-closed one", secImageDockerfile)
	}
	// One hatch, not a second spelling of it: the two refusal branches
	// read the same variable, and nothing else turns the assertion off.
	if got := strings.Count(src, `[ "${REQUIRE_DEEPSEC}" = "0" ] || exit 1`); got != 2 {
		t.Fatalf("the REQUIRE_DEEPSEC guard appears %d times, want exactly 2 (the absent and the drifted branch)", got)
	}
}
