package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDetectOpenCodeNeedsTheBinary: no CLI, no backend — a report that
// claims one would feed ITERION_BACKEND_PREFERENCE a backend that dies at
// exec.
func TestDetectOpenCodeNeedsTheBinary(t *testing.T) {
	isolateEnv(t)
	st := detectOpenCode(nil)
	if st.Available {
		t.Fatal("opencode reported available with no binary")
	}
	if st.Name != BackendOpenCode {
		t.Fatalf("Name = %q, want %q", st.Name, BackendOpenCode)
	}
}

// TestDetectOpenCodeOrsItsCredentialShapes: opencode reads BOTH its own auth
// store and the provider environment. Gating on the store alone reports it
// unavailable on the common host that drives it from a provider key — and
// the model catalogue's opencode arm, which only runs for an AVAILABLE
// backend, would be dead exactly there.
func TestDetectOpenCodeOrsItsCredentialShapes(t *testing.T) {
	t.Run("own auth store alone", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findOpenCodeBinary, "/fake/opencode")
		data := t.TempDir()
		t.Setenv("XDG_DATA_HOME", data)
		writeOpenCodeAuth(t, data, `{"anthropic":{"type":"api"}}`)
		st := detectOpenCode(nil)
		if !st.Available {
			t.Fatalf("not available with a populated auth store: %+v", st)
		}
	})

	t.Run("provider environment alone", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findOpenCodeBinary, "/fake/opencode")
		t.Setenv("XDG_DATA_HOME", t.TempDir()) // no auth.json at all
		t.Setenv("ANTHROPIC_API_KEY", "sk-test")
		st := detectOpenCode([]ProviderStatus{
			{Name: "anthropic", Available: true, Source: "ANTHROPIC_API_KEY"},
		})
		if !st.Available {
			t.Fatalf("not available with a provider key: %+v", st)
		}
	})

	t.Run("an EMPTY auth store is not a credential", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findOpenCodeBinary, "/fake/opencode")
		data := t.TempDir()
		t.Setenv("XDG_DATA_HOME", data)
		writeOpenCodeAuth(t, data, `{}`)
		st := detectOpenCode(nil)
		if st.Available {
			t.Fatal("a fresh install's empty store was counted as a credential")
		}
	})
}

// TestOpenCodeIsReportedNeverAutoSelected: surfacing opencode must not change
// what a host with no explicit preference resolves to — that is what keeps
// every existing workflow, and C018's auto-resolution, untouched.
func TestOpenCodeIsReportedNeverAutoSelected(t *testing.T) {
	for _, name := range DefaultPreferenceOrder {
		if name == BackendOpenCode {
			t.Fatal("opencode entered DefaultPreferenceOrder; it would be auto-selected")
		}
	}
	got := Resolve(DefaultPreferenceOrder, []BackendStatus{
		{Name: BackendOpenCode, Available: true},
	})
	if got != "" {
		t.Fatalf("Resolve picked %q from an opencode-only report, want none", got)
	}
}

// writeOpenCodeAuth materialises opencode's own credential store under an XDG
// data root — the path `opencode auth login` actually writes.
func writeOpenCodeAuth(t *testing.T, xdgData, body string) {
	t.Helper()
	dir := filepath.Join(xdgData, "opencode")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDetectOpenCodeRefusesCredentialsItCannotRead: iterion and opencode do
// not probe the same variables. Counting iterion's would report opencode
// available — and pair it with every model of that provider — on a host where
// every call fails for want of a credential.
func TestDetectOpenCodeRefusesCredentialsItCannotRead(t *testing.T) {
	// zai is a first-class iterion provider sourced from ZAI_API_KEY;
	// opencode reads ZHIPU_API_KEY. foundry: AZURE_OPENAI_API_KEY vs
	// opencode's AZURE_API_KEY.
	for _, p := range []ProviderStatus{
		{Name: "zai", Available: true, Source: "ZAI_API_KEY"},
		{Name: "foundry", Available: true, Source: "AZURE_OPENAI_API_KEY"},
		{Name: "openai", Available: true, Source: "AWS_DEFAULT_REGION"},
	} {
		t.Run(p.Source, func(t *testing.T) {
			isolateEnv(t)
			stubBinary(t, &findOpenCodeBinary, "/fake/opencode")
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Setenv(p.Source, "set-but-unreadable-by-opencode")
			if st := detectOpenCode([]ProviderStatus{p}); st.Available {
				t.Fatalf("%s counted as an opencode credential: %+v", p.Source, st)
			}
		})
	}

	// Controls: variables opencode DOES read still count, so the assertions
	// above are not a test that cannot fail. AWS_REGION and
	// GOOGLE_CLOUD_PROJECT are here on measurement, not on the catalogue
	// file: `opencode models` under each one alone lists amazon-bedrock and
	// google-vertex respectively.
	for _, p := range []ProviderStatus{
		{Name: "anthropic", Available: true, Source: "ANTHROPIC_API_KEY"},
		{Name: "bedrock", Available: true, Source: "AWS_REGION"},
		{Name: "vertex", Available: true, Source: "GOOGLE_CLOUD_PROJECT"},
	} {
		t.Run(p.Source+" counts", func(t *testing.T) {
			isolateEnv(t)
			stubBinary(t, &findOpenCodeBinary, "/fake/opencode")
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Setenv(p.Source, "set")
			if st := detectOpenCode([]ProviderStatus{p}); !st.Available {
				t.Fatalf("a credential opencode reads was refused: %+v", st)
			}
		})
	}

	// Looking like a variable name is not evidence the variable holds
	// anything: iterion labels a provider row with a variable name even when
	// another source made the row available.
	t.Run("an allowlisted variable that is EMPTY is not a credential", func(t *testing.T) {
		isolateEnv(t)
		stubBinary(t, &findOpenCodeBinary, "/fake/opencode")
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		t.Setenv("ANTHROPIC_API_KEY", "")
		if st := detectOpenCode([]ProviderStatus{
			{Name: "anthropic", Available: true, Source: "ANTHROPIC_API_KEY"},
		}); st.Available {
			t.Fatalf("an unset variable counted as a credential: %+v", st)
		}
	})
}

// TestDetectOpenCodeFindsOnlyWhatTheDelegateCanSpawn: the delegate resolves
// argv[0] through os/exec against PATH (plus ITERION_OPENCODE_BIN). A probe
// that looks in more places reports a backend that dies at spawn.
func TestDetectOpenCodeFindsOnlyWhatTheDelegateCanSpawn(t *testing.T) {
	isolateEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	installer := filepath.Join(home, ".opencode", "bin")
	if err := os.MkdirAll(installer, 0o750); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(installer, "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil { // #nosec G306 — a test stub that must be executable.
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	if path, ok := locateOpenCodeBinary(); ok {
		t.Fatalf("probe found %q off PATH; the delegate could not spawn it", path)
	}
	// The documented escape hatch makes the same binary reachable to BOTH.
	t.Setenv("ITERION_OPENCODE_BIN", bin)
	if _, ok := locateOpenCodeBinary(); !ok {
		t.Fatal("ITERION_OPENCODE_BIN did not make the binary reachable")
	}
}

// TestOpenCodeProbeAndSpawnAgreeOnTheSameString: the probe and the delegate
// read ONE variable. A probe that resolves it differently reports a backend
// the run cannot use — the class this variable was introduced to close.
func TestOpenCodeProbeAndSpawnAgreeOnTheSameString(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	abs := filepath.Join(dir, "opencode")
	if err := os.WriteFile(abs, []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil { // #nosec G306 — a test stub that must be executable.
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // nothing named opencode on PATH

	t.Run("a relative path is a miss here because it is a refusal there", func(t *testing.T) {
		t.Setenv("ITERION_OPENCODE_BIN", "./opencode")
		if got, ok := locateOpenCodeBinary(); ok {
			t.Fatalf("probe resolved %q for a value the delegate refuses", got)
		}
	})

	t.Run("an absolute path is honoured by both", func(t *testing.T) {
		t.Setenv("ITERION_OPENCODE_BIN", abs)
		if got, ok := locateOpenCodeBinary(); !ok || got != abs {
			t.Fatalf("probe = (%q, %v), want the absolute path", got, ok)
		}
	})

	t.Run("a bare name is resolved through PATH by both", func(t *testing.T) {
		t.Setenv("PATH", dir)
		t.Setenv("ITERION_OPENCODE_BIN", "opencode")
		if _, ok := locateOpenCodeBinary(); !ok {
			t.Fatal("probe missed a bare name the delegate would PATH-resolve")
		}
	})

	t.Run("a non-executable pin is a miss, and the hint says why", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "opencode")
		if err := os.WriteFile(plain, []byte("text"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ITERION_OPENCODE_BIN", plain)
		if got, ok := locateOpenCodeBinary(); ok {
			t.Fatalf("probe accepted a non-executable pin %q; the spawn would EACCES", got)
		}
		st := detectOpenCode(nil)
		if len(st.Hints) == 0 || !strings.Contains(st.Hints[0], "ITERION_OPENCODE_BIN") {
			t.Fatalf("hint = %v, want it to name the pinned variable", st.Hints)
		}
	})
}
