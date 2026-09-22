package clilocate

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

// makeExecutable creates an executable file in dir with the given name.
// Returns the absolute path.
func makeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return p
}

func TestLocate_ExplicitPath_Found(t *testing.T) {
	dir := t.TempDir()
	bin := makeExecutable(t, dir, "fake-cli")

	got, ok := Locate(bin, Spec{Name: "ignored"})
	if !ok || got != bin {
		t.Fatalf("explicit path: got (%q, %v), want (%q, true)", got, ok, bin)
	}
}

func TestLocate_ExplicitPath_Missing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-there")

	// Even though Fallbacks contains a real file, explicit-miss must
	// return false — the caller asked for a specific path.
	real := makeExecutable(t, dir, "real")
	got, ok := Locate(missing, Spec{Name: "real", Fallbacks: []string{real}})
	if ok || got != "" {
		t.Fatalf("explicit miss should not consult fallbacks; got (%q, %v)", got, ok)
	}
}

func TestLocate_ExplicitPath_Directory(t *testing.T) {
	dir := t.TempDir()
	// Passing a directory as explicit should miss — isExecutable checks !IsDir.
	got, ok := Locate(dir, Spec{Name: "ignored"})
	if ok || got != "" {
		t.Fatalf("directory as explicit should miss; got (%q, %v)", got, ok)
	}
}

func TestLocate_PathLookup(t *testing.T) {
	dir := t.TempDir()
	bin := makeExecutable(t, dir, "uniq-test-bin")

	// Prepend dir to PATH and look up the bare name.
	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)

	got, ok := Locate("", Spec{Name: "uniq-test-bin"})
	if !ok || got != bin {
		t.Fatalf("PATH lookup: got (%q, %v), want (%q, true)", got, ok, bin)
	}
}

func TestLocate_FallbackUsedWhenPathMisses(t *testing.T) {
	dir := t.TempDir()
	bin := makeExecutable(t, dir, "fallback-bin")

	// Empty PATH so exec.LookPath misses.
	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	os.Setenv("PATH", "")

	got, ok := Locate("", Spec{
		Name:      "should-not-exist-anywhere-xyz",
		Fallbacks: []string{bin},
	})
	if !ok || got != bin {
		t.Fatalf("fallback resolution: got (%q, %v), want (%q, true)", got, ok, bin)
	}
}

func TestLocate_FallbackSkipsNonExecutable(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on windows")
	}
	dir := t.TempDir()
	nonExec := filepath.Join(dir, "non-exec")
	if err := os.WriteFile(nonExec, []byte("text"), 0o644); err != nil {
		t.Fatalf("write non-exec: %v", err)
	}
	exec := makeExecutable(t, dir, "real-bin")

	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	os.Setenv("PATH", "")

	got, ok := Locate("", Spec{
		Name:      "missing-name",
		Fallbacks: []string{nonExec, exec},
	})
	if !ok || got != exec {
		t.Fatalf("expected fallback to skip non-exec; got (%q, %v), want (%q, true)", got, ok, exec)
	}
}

func TestLocate_AllMiss(t *testing.T) {
	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	os.Setenv("PATH", "")

	got, ok := Locate("", Spec{
		Name:      "definitely-does-not-exist-abcxyz",
		Fallbacks: []string{"/definitely/not/here"},
	})
	if ok || got != "" {
		t.Fatalf("expected miss, got (%q, %v)", got, ok)
	}
}

func TestCommonBinaryCandidates(t *testing.T) {
	got := CommonBinaryCandidates("foo")
	if len(got) == 0 {
		t.Fatal("expected non-empty candidate list")
	}
	// All entries must contain the binary name as the last component.
	for _, p := range got {
		if filepath.Base(p) != "foo" {
			t.Errorf("candidate %q does not end in /foo", p)
		}
	}
}

// TestLocate_ExplicitSkipsNonExecutable: the explicit arm uses the same
// predicate as the fallback arm. Accepting a path the spawn fails on with
// EACCES makes the probe report a backend that cannot run.
func TestLocate_ExplicitSkipsNonExecutable(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on windows")
	}
	dir := t.TempDir()
	nonExec := filepath.Join(dir, "pinned-but-not-executable")
	if err := os.WriteFile(nonExec, []byte("text"), 0o644); err != nil {
		t.Fatalf("write non-exec: %v", err)
	}
	if got, ok := Locate(nonExec, Spec{Name: "irrelevant"}); ok {
		t.Fatalf("Locate accepted a non-executable explicit path: %q", got)
	}
	// Control: the same path, made executable, IS accepted.
	if err := os.Chmod(nonExec, 0o750); err != nil {
		t.Fatal(err)
	}
	if got, ok := Locate(nonExec, Spec{Name: "irrelevant"}); !ok || got != nonExec {
		t.Fatalf("Locate = (%q, %v), want the executable explicit path", got, ok)
	}
}

// TestClassifyPin_BothSeparatorSpellings: the classifier must answer the same
// question os/exec asks, on BOTH platforms. Windows accepts `/` as a path
// separator while `filepath.Separator` there is `\`, so a host-separator test
// classifies "./cli" as a bare name on Windows and hands it back to be joined
// with the command's Dir — the workspace. A test that only ever feeds the
// host's own separator cannot see that, which is how it shipped once.
func TestClassifyPin_BothSeparatorSpellings(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want PinKind
	}{
		{"", PinUnset},
		{"   ", PinUnset},
		{"opencode", PinBareName},
		{"opencode-nightly", PinBareName},
		{"opencode.sh", PinBareName},
		// The forward-slash spellings are the load-bearing ones: they must
		// classify as relative on EVERY platform, because Windows accepts
		// `/` as a separator while filepath.Separator there is `\`. Testing
		// only the host's own separator is what let "./cli" through as a
		// bare name on Windows.
		{"./opencode", PinRelative},
		{"bin/pi", PinRelative},
		{"../opencode", PinRelative},
	} {
		if got := ClassifyPin(tc.in); got != tc.want {
			t.Errorf("ClassifyPin(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// An absolute path in the host's own spelling.
	if got := ClassifyPin(filepath.Join(string(filepath.Separator), "opt", "cli")); got != PinAbsolute {
		t.Errorf("an absolute path classified as %v", got)
	}

	// Both separators, on EVERY platform — including this Unix host, which
	// is the only way the Windows hazard is falsifiable here: a
	// host-separator predicate would call these bare names on Windows and
	// hand them back to be joined with the workspace, and a Unix test could
	// never tell the two predicates apart on a forward-slash input.
	for _, v := range []string{`.\opencode`, `bin\pi`, `..\cli`} {
		if got := ClassifyPin(v); got != PinRelative {
			t.Errorf("ClassifyPin(%q) = %v, want PinRelative on every platform", v, got)
		}
	}
}

// TestLocatePinned_RelativeIsAMissEvenWhenItExists: the miss must come from
// the RULE, not from the file being absent. A relative pin is refused at
// spawn time, so a probe that resolves it reports a backend the run will
// refuse — and a test that only ever passes a non-existent relative path
// proves nothing about the rule.
func TestLocatePinned_RelativeIsAMissEvenWhenItExists(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on windows")
	}
	dir := t.TempDir()
	makeExecutable(t, dir, "cli")
	sub := filepath.Join(dir, "bin")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	makeExecutable(t, sub, "cli")
	t.Chdir(dir)

	for _, v := range []string{"./cli", "bin/cli", "../" + filepath.Base(dir) + "/cli"} {
		if got, ok := LocatePinned(v, Spec{Name: "cli"}); ok {
			t.Errorf("LocatePinned(%q) = %q; the spawn refuses this value", v, got)
		}
	}
}

// TestLocatePinned_UnsetKeepsTheCallersFallbacks: with nothing pinned, the
// caller's own candidate list must still be consulted — dropping it would
// make pi undetected on a host where it has always been found.
func TestLocatePinned_UnsetKeepsTheCallersFallbacks(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on windows")
	}
	dir := t.TempDir()
	fallback := makeExecutable(t, dir, "elsewhere-cli")

	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	os.Setenv("PATH", "")

	got, ok := LocatePinned("", Spec{Name: "no-such-cli-anywhere", Fallbacks: []string{fallback}})
	if !ok || got != fallback {
		t.Fatalf("LocatePinned(unset) = (%q, %v), want the caller's fallback %q", got, ok, fallback)
	}
}
