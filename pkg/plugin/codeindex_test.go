package plugin

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The codeindex builtin is the broadest contributor shipped (mcp_servers +
// rewriters + lifecycle + skills + commands + agents), so it is also the one
// most able to rot silently: a renamed markdown file still parses, and a
// version pin that drifts out of lockstep with the runner image still runs —
// it just quietly downloads a second copy at cold start. These tests pin the
// invariants that no schema check would catch.

func TestCodeindexBuiltinContributions(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := reg.Get("codeindex")
	if !ok {
		t.Fatal("builtin \"codeindex\" missing")
	}

	// A knowledge-graph explorer is opt-in, like its peers.
	if reg.IsEnabled("codeindex") {
		t.Error("codeindex should be disabled by default")
	}
	if !p.Manifest.AutoIndex {
		t.Error("auto_index should be true: priming .codeindex/ turns MCP activation into a load, not a rebuild")
	}

	// The MCP server must pin the workspace, otherwise every tool call needs an
	// absolute path the agent has no way to know.
	mcps := p.Manifest.Contributes.MCPServers
	if len(mcps) != 1 || mcps[0].Name != "codeindex" {
		t.Fatalf("mcp_servers = %+v, want one named codeindex", mcps)
	}
	args := strings.Join(mcps[0].Args, " ")
	for _, want := range []string{"mcp", "--repo", "{{workspace}}"} {
		if !strings.Contains(args, want) {
			t.Errorf("mcp args %q missing %q", args, want)
		}
	}
	if mcps[0].Command != "npx" {
		t.Errorf("mcp command = %q, want npx", mcps[0].Command)
	}

	// The rewriter must locate a real binary rather than shelling out to npx:
	// it runs on EVERY shell command, where npx startup would dominate.
	rw := p.Manifest.Contributes.Rewriters
	if len(rw) != 1 || rw[0].ID != "codeindex" {
		t.Fatalf("rewriters = %+v, want one with id codeindex", rw)
	}
	if rw[0].Locate.Bin != "codeindex" || rw[0].Locate.Env == "" || len(rw[0].Locate.Paths) == 0 {
		t.Errorf("rewriter locate = %+v, want env + bin + paths", rw[0].Locate)
	}
	// Deliberately NO sandbox_mount: codeindex's bin entry imports ./engine.mjs
	// as a sibling, so bind-mounting the bin path alone yields a module that
	// cannot resolve its own import — and it would shadow the working global the
	// runner image installs at that very path. Asserted so a future "every
	// rewriter should mount its binary" cleanup cannot silently reintroduce it.
	if rw[0].SandboxMount != "" {
		t.Errorf("sandbox_mount = %q, want empty: a Node bundle is not a self-contained binary", rw[0].SandboxMount)
	}

	if lc := p.Manifest.Contributes.Lifecycle; lc == nil || lc.Index == "" || lc.Refresh == "" {
		t.Fatalf("lifecycle = %+v, want index and refresh", lc)
	}

	// Every declared markdown path must actually resolve in the embedded FS.
	for _, kind := range MirrorKinds {
		files, err := p.MirrorFiles(kind)
		if err != nil {
			t.Fatalf("MirrorFiles(%s): %v", kind.Name, err)
		}
		if len(files) == 0 {
			t.Errorf("codeindex contributes no %s files", kind.Name)
		}
		for _, f := range files {
			if len(f.Content) == 0 {
				t.Errorf("%s %q is empty", kind.Name, f.Name)
			}
		}
	}
}

// repoRoot walks up from the package dir to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return dir
}

// Every @maxgfr/codeindex pin — the three in plugin.yaml and the runner image's
// ARG — must agree. A drifted pin is not a build failure: the runtime `npx`
// silently downloads a different version than the one baked into the image,
// which is exactly the cold-start race the pre-install exists to prevent.
func TestCodeindexVersionPinsInLockstep(t *testing.T) {
	root := repoRoot(t)
	pinRe := regexp.MustCompile(`@maxgfr/codeindex@(\d+\.\d+\.\d+)`)
	argRe := regexp.MustCompile(`ARG CODEINDEX_VERSION=(\d+\.\d+\.\d+)`)

	manifest, err := os.ReadFile(filepath.Join(root, "pkg/plugin/builtin/codeindex/plugin.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	pins := pinRe.FindAllStringSubmatch(string(manifest), -1)
	if len(pins) < 3 {
		t.Fatalf("expected the mcp arg + both lifecycle commands to pin a version, got %d", len(pins))
	}
	want := pins[0][1]
	for _, p := range pins {
		if p[1] != want {
			t.Errorf("plugin.yaml pins both %s and %s", want, p[1])
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile.runner-devbox"))
	if err != nil {
		t.Fatalf("read Dockerfile.runner-devbox: %v", err)
	}
	m := argRe.FindStringSubmatch(string(dockerfile))
	if m == nil {
		t.Fatal("Dockerfile.runner-devbox has no CODEINDEX_VERSION ARG — the runner would cold-download on every pod")
	}
	if m[1] != want {
		t.Errorf("Dockerfile pins %s but plugin.yaml pins %s — runtime npx would fetch a second copy", m[1], want)
	}
}

// A renovate regex-manager pin only updates the version on the line FOLLOWING
// its comment. An occurrence without its own comment goes stale silently, and
// a file absent from managerFilePatterns is never scanned at all.
func TestCodeindexPinsAreRenovateTracked(t *testing.T) {
	root := repoRoot(t)
	manifestPath := filepath.Join(root, "pkg/plugin/builtin/codeindex/plugin.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	// Mirrors renovate.json's matchStrings.
	tracked := regexp.MustCompile(`# renovate: datasource=\S+ depName=\S+(?: versioning=\S+)?\s+[^\n]*?(\d+\.\d+\.\d+)`)
	nTracked := len(tracked.FindAllString(string(manifest), -1))
	nPins := len(regexp.MustCompile(`@maxgfr/codeindex@\d+\.\d+\.\d+`).FindAllString(string(manifest), -1))
	if nTracked != nPins {
		t.Errorf("%d version pins but %d renovate comments — every pin needs its own", nPins, nTracked)
	}

	renovate, err := os.ReadFile(filepath.Join(root, "renovate.json"))
	if err != nil {
		t.Fatalf("read renovate.json: %v", err)
	}
	if !strings.Contains(string(renovate), "pkg/plugin/builtin/codeindex/plugin") {
		t.Error("renovate.json managerFilePatterns does not cover the codeindex plugin manifest")
	}
}
