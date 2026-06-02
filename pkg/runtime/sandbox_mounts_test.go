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
	noopEmit := func(store.EventType, map[string]interface{}) error { return nil }

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
		func(store.EventType, map[string]interface{}) error { return nil }, iterlog.Nop())

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
