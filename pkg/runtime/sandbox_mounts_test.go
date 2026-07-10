package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tmpfsDir reports whether spec.Tmpfs contains an entry for dir that is
// user-owned (uid=<getuid>) and mounted exec — the shape devbox/npm/pip/go
// need to create siblings under a HOME-nested bind's parent.
func tmpfsDir(tmpfs []string, dir string) (string, bool) {
	prefix := dir + ":"
	for _, tm := range tmpfs {
		if strings.HasPrefix(tm, prefix) {
			return tm, true
		}
	}
	return "", false
}

// TestApplyHostStateMounts_HomeTmpfsIsExec guards a regression: the
// writable HOME tmpfs that host_state lays down MUST be mounted `exec`.
// docker defaults --tmpfs to noexec, which blocks anything executable
// that lands in $HOME — notably go's auto-downloaded toolchain
// ($HOME/go/pkg/mod/golang.org/toolchain@.../bin/go) — making a
// sandboxed `go build` die with "cannot execute". A run hit exactly that
// and had to hand-relocate GOPATH to /tmp. See sandbox_mounts.go.
func TestApplyHostStateMounts_HomeTmpfsIsExec(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("host_state HOME tmpfs is laid down on Linux only")
	}
	if os.Getuid() == 0 {
		t.Skip("the uid-owned HOME tmpfs is only added for a non-root host user")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	spec := &sandbox.Spec{}
	// Empty workflow + empty params → pickHostState defaults to auto
	// (active), which is the path that adds the HOME tmpfs.
	wf := &ir.Workflow{}
	p := SandboxParams{WorkspacePath: t.TempDir()}
	noopEmit := func(store.EventType, map[string]any) error { return nil }

	applyHostStateMounts(spec, wf, p, noopEmit, iterlog.Nop())

	var homeEntry string
	for _, tm := range spec.Tmpfs {
		if strings.HasPrefix(tm, home+":") {
			homeEntry = tm
			break
		}
	}
	if homeEntry == "" {
		t.Fatalf("host_state active but no HOME tmpfs entry for %q in spec.Tmpfs=%v", home, spec.Tmpfs)
	}

	opts := homeEntry[strings.Index(homeEntry, ":")+1:]
	hasExec := false
	for _, o := range strings.Split(opts, ",") {
		if o == "exec" {
			hasExec = true
			break
		}
	}
	if !hasExec {
		t.Errorf("HOME tmpfs %q lacks the `exec` option; docker defaults --tmpfs to noexec, which breaks the go toolchain auto-download in $HOME", homeEntry)
	}
}

// TestApplyHostStateMounts_WarmGoCaches guards that the host's Go build +
// module caches are bind-mounted into the sandbox when present, so fresh
// worktrees reuse the warm cache (and the auto-downloaded toolchain under
// $HOME/go/pkg/mod) instead of a cold full compile every run.
func TestApplyHostStateMounts_WarmGoCaches(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("host_state mounts are Linux + docker only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Create the caches under the fake HOME so the mount fires.
	for _, rel := range []string{".cache/go-build", "go/pkg/mod"} {
		if err := os.MkdirAll(filepath.Join(home, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()},
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())

	for _, rel := range []string{".cache/go-build", "go/pkg/mod"} {
		want := filepath.Join(home, rel)
		found := false
		for _, m := range spec.Mounts {
			if strings.Contains(m, "source="+want+",") && strings.Contains(m, "target="+want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected a bind mount for the go cache %q; spec.Mounts=%v", want, spec.Mounts)
		}
	}
}

// TestApplyHostStateMounts_ClaudeConfigFile guards native:221edac8's root
// cause: `~/.claude.json` (Claude Code's top-level config, a SIBLING of the
// ~/.claude directory) must ride along with the ~/.claude mount. Without it
// the in-container CLI sees the host's config backups (inside the mounted
// ~/.claude/backups/) but no config, demands a manual restore, and in
// --print stream-json mode hangs with zero stdout — every sandboxed
// claude_code attempt then dies on the 90s cold-phase timeout.
func TestApplyHostStateMounts_ClaudeConfigFile(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("host_state mounts are Linux + docker only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("mounted when present", func(t *testing.T) {
		cfg := filepath.Join(home, ".claude.json")
		if err := os.WriteFile(cfg, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		spec := &sandbox.Spec{}
		applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()},
			func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
		found := false
		for _, m := range spec.Mounts {
			if strings.Contains(m, "source="+cfg+",") && strings.Contains(m, "target="+cfg) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected a bind mount for %q (claude CLI top-level config); spec.Mounts=%v", cfg, spec.Mounts)
		}
		if err := os.Remove(cfg); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("absent host file skipped silently", func(t *testing.T) {
		spec := &sandbox.Spec{}
		applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()},
			func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
		for _, m := range spec.Mounts {
			if strings.Contains(m, ".claude.json") {
				t.Errorf("no host ~/.claude.json exists, yet a mount references it: %q", m)
			}
		}
	})
}

// TestApplyHostStateMounts_HomeNestedBindParentsWritable guards the
// devbox-first-class fix: the Go-cache binds nest under HOME
// ($HOME/.cache/go-build, $HOME/go/pkg/mod), and docker creates their
// missing parents ($HOME/.cache, $HOME/go) as root:root — shadowing the
// writable HOME tmpfs so `devbox run` can't mkdir $HOME/.cache/devbox. The
// fix lays a user-owned exec tmpfs at each such parent too. Assert both
// parents are present.
func TestApplyHostStateMounts_HomeNestedBindParentsWritable(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("host_state HOME tmpfs is laid down on Linux only")
	}
	if os.Getuid() == 0 {
		t.Skip("the uid-owned HOME tmpfs is only added for a non-root host user")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Create the caches under the fake HOME so the nested binds fire.
	for _, rel := range []string{".cache/go-build", "go/pkg/mod"} {
		if err := os.MkdirAll(filepath.Join(home, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()},
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())

	for _, parent := range []string{filepath.Join(home, ".cache"), filepath.Join(home, "go")} {
		entry, ok := tmpfsDir(spec.Tmpfs, parent)
		if !ok {
			t.Errorf("expected a user-owned tmpfs for the nested-bind parent %q so devbox/go can write siblings; spec.Tmpfs=%v", parent, spec.Tmpfs)
			continue
		}
		opts := entry[strings.Index(entry, ":")+1:]
		hasExec, hasUID := false, false
		for _, o := range strings.Split(opts, ",") {
			switch {
			case o == "exec":
				hasExec = true
			case strings.HasPrefix(o, "uid="):
				hasUID = true
			}
		}
		if !hasExec || !hasUID {
			t.Errorf("tmpfs %q must be user-owned (uid=) and exec; got opts %q", parent, opts)
		}
	}
}

// TestHomeNestedBindParents unit-tests the helper that decides which
// HOME-nested bind parents need a user-owned tmpfs. Direct children of
// $HOME (.claude/.iterion) need none — their parent is $HOME itself, which
// is already a tmpfs; strictly-nested binds need EVERY strict ancestor
// under $HOME re-laid (.local/share/pnpm → .local AND .local/share),
// shallowest-first so the tmpfs layers stack in mount order.
func TestHomeNestedBindParents(t *testing.T) {
	home := "/home/jo"
	mounts := []string{
		"source=/h/.iterion,target=/home/jo/.iterion,type=bind",          // direct child → skip
		"source=/h/.claude,target=/home/jo/.claude,type=bind",            // direct child → skip
		"source=/h/.gitconfig,target=/home/jo/.gitconfig,type=bind",      // direct child file → skip
		"source=/h/gb,target=/home/jo/.cache/go-build,type=bind",         // nested → .cache
		"source=/h/mod,target=/home/jo/go/pkg/mod,type=bind",             // nested → go, go/pkg
		"source=/h/x,target=/home/jo/.cache/other,type=bind",             // nested, same parent → dedup
		"source=/h/pnpm,target=/home/jo/.local/share/pnpm,type=bind",     // deep-nested → .local, .local/share
		"source=/etc/foo,target=/etc/foo,type=bind",                      // outside HOME → skip
		"source=/h/bin,target=/usr/local/bin/iterion,type=bind,readonly", // outside HOME → skip
	}
	got := homeNestedBindParents(home, mounts)
	want := []string{
		"/home/jo/.cache", "/home/jo/.local", "/home/jo/go",
		"/home/jo/.local/share", "/home/jo/go/pkg",
	}
	if len(got) != len(want) {
		t.Fatalf("homeNestedBindParents = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("homeNestedBindParents = %v, want %v", got, want)
		}
	}

	if r := homeNestedBindParents("", mounts); r != nil {
		t.Errorf("empty homeDir must yield nil, got %v", r)
	}
}
