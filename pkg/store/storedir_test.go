package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveStoreDir_OverrideWins(t *testing.T) {
	got := ResolveStoreDir("/some/start", "/explicit/store")
	if got != "/explicit/store" {
		t.Fatalf("override should be returned verbatim, got %q", got)
	}
}

func TestResolveStoreDir_EmptyStartFallsBack(t *testing.T) {
	got := ResolveStoreDir("", "")
	if got != StoreDirName {
		t.Fatalf("empty start should fall back to %q, got %q", StoreDirName, got)
	}
}

// withIterionHome redirects the global iterion data dir to a temp
// directory for the duration of the test. Without this, tests would
// either touch the real ~/.iterion or rely on the implicit OS
// fallback, both fragile.
func withIterionHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ITERION_HOME", dir)
	return dir
}

func TestResolveStoreDir_ProjectLocalOptInWins(t *testing.T) {
	iterionHome := withIterionHome(t)
	root := t.TempDir()
	storeAt := filepath.Join(root, StoreDirName)
	// A managed project-local store has at least one of the
	// well-known subdirs (runs/, dispatcher/) or the explicit sentinel
	// (.iterion-store). Create runs/ to mark it.
	if err := os.MkdirAll(filepath.Join(storeAt, "runs"), 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := ResolveStoreDir(root, "")
	if got != storeAt {
		t.Fatalf("project-local .iterion should win, expected %q got %q", storeAt, got)
	}
	// Sanity: nothing leaked into the global root.
	if _, err := os.Stat(filepath.Join(iterionHome, "projects")); err == nil {
		t.Fatalf("global projects dir was created despite local opt-in")
	}
}

// TestResolveStoreDir_StrayDotIterionDirIgnored guards F-NEW-10:
// when a workspace tool (e.g. the whats-next bot's emit_action
// writing .iterion/plans/whats-next-*.md) creates a bare .iterion/
// without any of the iterion-managed subdirs, ResolveStoreDir must
// NOT pick it up — falling back to the global per-project slot keeps
// CLI/daemon calls consistent with where the actual runs live.
func TestResolveStoreDir_StrayDotIterionDirIgnored(t *testing.T) {
	iterionHome := withIterionHome(t)
	root := t.TempDir()
	stray := filepath.Join(root, StoreDirName, "plans")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// No runs/ or dispatcher/ or .iterion-store marker → not managed.

	got := ResolveStoreDir(root, "")

	abs, _ := filepath.Abs(root)
	want := filepath.Join(iterionHome, "projects", EncodeWorkDirKey(abs))
	if got != want {
		t.Fatalf("stray .iterion/plans must not hijack the resolver: expected global %q got %q", want, got)
	}
}

// TestResolveStoreDir_SentinelFileMarker validates the .iterion-store
// sentinel path — operators (or RunStore.Init) can drop an empty
// .iterion-store file to opt-in without yet having runs/ or
// dispatcher/ on disk.
func TestResolveStoreDir_SentinelFileMarker(t *testing.T) {
	withIterionHome(t)
	root := t.TempDir()
	storeAt := filepath.Join(root, StoreDirName)
	if err := os.Mkdir(storeAt, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storeAt, ".iterion-store"), nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := ResolveStoreDir(root, "")
	if got != storeAt {
		t.Fatalf("sentinel file should mark store as managed, expected %q got %q", storeAt, got)
	}
}

func TestResolveStoreDir_DefaultsToGlobalProjectStore(t *testing.T) {
	iterionHome := withIterionHome(t)
	root := t.TempDir()

	got := ResolveStoreDir(root, "")

	abs, _ := filepath.Abs(root)
	want := filepath.Join(iterionHome, "projects", EncodeWorkDirKey(abs))
	if got != want {
		t.Fatalf("expected global per-project slot %q, got %q", want, got)
	}
}

// TestResolveStoreDir_DoesNotInheritParent guards the regression that
// motivated the redesign: even when an ancestor of `start` has a
// .iterion directory, ResolveStoreDir must not pick it up — the new
// model rejects walk-up entirely. The ancestor is opted IN for
// itself but NOT for its children.
func TestResolveStoreDir_DoesNotInheritParent(t *testing.T) {
	iterionHome := withIterionHome(t)
	root := t.TempDir()
	// Stray .iterion at the top — typical of a leftover ~/.iterion.
	stray := filepath.Join(root, StoreDirName)
	if err := os.Mkdir(stray, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := ResolveStoreDir(deep, "")

	absDeep, _ := filepath.Abs(deep)
	want := filepath.Join(iterionHome, "projects", EncodeWorkDirKey(absDeep))
	if got != want {
		t.Fatalf("must not inherit parent .iterion: expected %q got %q (would have leaked to %q)", want, got, stray)
	}
}

func TestResolveStoreDir_IgnoresFileNamedDotIterion(t *testing.T) {
	iterionHome := withIterionHome(t)
	root := t.TempDir()
	// A regular FILE named .iterion must NOT be confused with the
	// store dir. The opt-in is "directory exists", not "name exists".
	if err := os.WriteFile(filepath.Join(root, StoreDirName), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := ResolveStoreDir(root, "")

	abs, _ := filepath.Abs(root)
	want := filepath.Join(iterionHome, "projects", EncodeWorkDirKey(abs))
	if got != want {
		t.Fatalf("file (not dir) named .iterion must be skipped — expected global %q got %q", want, got)
	}
}

func TestResolveStoreDir_DistinctWorkDirsGetDistinctSlots(t *testing.T) {
	iterionHome := withIterionHome(t)
	a, _ := filepath.Abs(filepath.Join(t.TempDir(), "alpha"))
	b, _ := filepath.Abs(filepath.Join(t.TempDir(), "beta"))
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	gotA := ResolveStoreDir(a, "")
	gotB := ResolveStoreDir(b, "")
	if gotA == gotB {
		t.Fatalf("different workdirs must yield different stores, both got %q", gotA)
	}
	for _, g := range []string{gotA, gotB} {
		rel, err := filepath.Rel(iterionHome, g)
		if err != nil || rel == "." || rel == ".." {
			t.Fatalf("expected slot under iterion home %q, got %q", iterionHome, g)
		}
	}
}

func TestEncodeWorkDirKey(t *testing.T) {
	cases := map[string]string{
		"/home/jo/lab/devthefuture/modjo": "-home-jo-lab-devthefuture-modjo",
		"/":                               "-",
		"/a":                              "-a",
		// The drive ":" and the leading "\" both collapse to "-",
		// so the boundary surfaces as "--". This is deterministic
		// and means C:\foo\bar and C\foo\bar (no drive) yield
		// distinct keys, which is what we want.
		`C:\foo\bar`: "-C--foo-bar",
	}
	for in, want := range cases {
		if got := EncodeWorkDirKey(in); got != want {
			t.Errorf("EncodeWorkDirKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeWorkDirKey_DistinctPathsDistinctKeys(t *testing.T) {
	// Defensive guard against accidentally collision-prone encoding
	// changes: any two distinct absolute paths under common roots
	// must produce distinct keys.
	paths := []string{
		"/home/a",
		"/home/b",
		"/home/a/sub",
		"/etc/a",
		`C:\foo\bar`,
		`D:\foo\bar`,
	}
	seen := map[string]string{}
	for _, p := range paths {
		k := EncodeWorkDirKey(p)
		if other, ok := seen[k]; ok {
			t.Errorf("collision: %q and %q both encode to %q", other, p, k)
		}
		seen[k] = p
	}
}
