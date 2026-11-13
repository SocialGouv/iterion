package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// The guard's contract (iterion#2868): the exclusion FOLLOWS the paths a
// credential is actually read from — real layouts at any depth, the
// process's own env/home-derived locations, and .npmrc at a project
// position — so a same-named fixture or template outside those locations
// stays readable. Every case below runs under BOTH a workspace root and
// an extra root (the run scratch), the shape the host's witness pins.

func testGuardEnv(home string, extra map[string]string) func(string) string {
	return func(key string) string {
		if key == "HOME" {
			return home
		}
		return extra[key]
	}
}

func TestCredentialReadGuardRefusesRealLocations(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	scratch := t.TempDir()
	roots := []string{workspace, scratch}

	kube := filepath.Join(t.TempDir(), "cluster-kubeconfig")
	guard := NewCredentialReadGuard(testGuardEnv(home, map[string]string{
		"KUBECONFIG": kube,
	}))

	// Plant a real file at every location the guard must refuse, so the
	// verdict judges the path, not a missing file.
	refused := map[string]string{
		"home kubeconfig":                         filepath.Join(home, ".kube", "config"),
		"home npmrc":                              filepath.Join(home, ".npmrc"),
		"home codex auth":                         filepath.Join(home, ".codex", "auth.json"),
		"home aws credentials":                    filepath.Join(home, ".aws", "credentials"),
		"home docker config":                      filepath.Join(home, ".docker", "config.json"),
		"home claude credential":                  filepath.Join(home, ".claude", ".credentials.json"),
		"home gh hosts":                           filepath.Join(home, ".config", "gh", "hosts.yml"),
		"home gcloud adc":                         filepath.Join(home, ".config", "gcloud", "application_default_credentials.json"),
		"env kubeconfig":                          kube,
		"a kubeconfig layout under the workspace": filepath.Join(workspace, "runner-home-x", ".kube", "config"),
		"an npmrc at the workspace root":          filepath.Join(workspace, ".npmrc"),
		"an npmrc at the scratch root":            filepath.Join(scratch, ".npmrc"),
		"an aws layout under the scratch root":    filepath.Join(scratch, "deep", "nest", ".aws", "credentials"),
		"a codex layout under the scratch root":   filepath.Join(scratch, "runner-home-y", ".codex", "auth.json"),
		"an ssh key layout under the workspace":   filepath.Join(workspace, ".ssh", "id_rsa"),
	}
	for name, path := range refused {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("TOKEN=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !guard.Refused(path, roots...) {
			t.Errorf("%s: Refused(%q) = false, want the real-location refusal", name, path)
		}
	}
}

func TestCredentialReadGuardLetsFixturesAndTemplatesRead(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	scratch := t.TempDir()
	roots := []string{workspace, scratch}
	guard := NewCredentialReadGuard(testGuardEnv(home, nil))

	// The doctrine's other half: a file that merely CARRIES a
	// credential-looking name outside a real location is not a credential
	// this process reads — the frozen name list refused these, the derived
	// one does not.
	readable := map[string]string{
		"kubeconfig template":     filepath.Join(workspace, "deploy", "kubeconfig.tpl"),
		"bare kubeconfig fixture": filepath.Join(scratch, "fixtures", "kubeconfig"),
		"credentials fixture":     filepath.Join(scratch, "fixtures", "credentials"),
		"credentials directory":   filepath.Join(workspace, "fixtures", "credentials", "aws.ini"),
		"auth.json testdata":      filepath.Join(workspace, "testdata", "auth.json"),
		"secrets.json fixture":    filepath.Join(scratch, "testdata", "secrets.json"),
		"nested .npmrc fixture":   filepath.Join(workspace, "testdata", "npm", ".npmrc"),
		"renamed env example":     filepath.Join(workspace, "env.example"),
	}
	for name, path := range readable {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if guard.Refused(path, roots...) {
			t.Errorf("%s: Refused(%q) = true — a fixture/template outside every real credential location must stay readable", name, path)
		}
	}
}

func TestCredentialReadGuardFoldsCase(t *testing.T) {
	workspace := t.TempDir()
	guard := NewCredentialReadGuard(testGuardEnv(t.TempDir(), nil))
	for _, name := range []string{".ENV", ".Env", ".ENV.local", ".NETRC", "CLI-AUTH.JSON", "ID_RSA", "server.PEM"} {
		if !guard.Refused(filepath.Join(workspace, name), workspace) {
			t.Errorf("Refused(%q) = false — the fold must catch every spelling of an intrinsic carrier", name)
		}
	}
	// A folded name that is NOT a carrier still reads: the fold did not
	// turn the guard into "anything uppercase is a secret".
	if guard.Refused(filepath.Join(workspace, "ENVIRONMENT.md"), workspace) {
		t.Error("Refused(ENVIRONMENT.md) = true — the fold must not widen the list")
	}
}

func TestCredentialReadGuardDerivesFromEnv(t *testing.T) {
	dir := t.TempDir()
	npmrc := filepath.Join(dir, "user-npmrc")
	codexHome := filepath.Join(dir, "codex")
	dockerConfig := filepath.Join(dir, "docker")
	guard := NewCredentialReadGuard(testGuardEnv(t.TempDir(), map[string]string{
		"NPM_CONFIG_USERCONFIG": npmrc,
		"CODEX_HOME":            codexHome,
		"DOCKER_CONFIG":         dockerConfig,
	}))
	for _, path := range []string{
		npmrc,
		filepath.Join(codexHome, "auth.json"),
		filepath.Join(dockerConfig, "config.json"),
	} {
		if !guard.Refused(path) {
			t.Errorf("Refused(%q) = false — an env-derived credential location refuses under NO root at all", path)
		}
	}
	// The same names elsewhere are not the derived locations.
	if guard.Refused(filepath.Join(dir, "auth.json")) {
		t.Error("a bare auth.json beside CODEX_HOME is not the derived location and must read")
	}
}

func TestCredentialReadGuardFollowsAHomenSymlink(t *testing.T) {
	// The derived list holds BOTH spellings of a location: a read target
	// resolved through a symlinked home directory must meet the resolved
	// spelling, a raw spelling the lexical one.
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	guard := NewCredentialReadGuard(testGuardEnv(link, nil))
	if !guard.Refused(filepath.Join(real, ".npmrc")) {
		t.Error("Refused(<real home>/.npmrc) = false — the resolved spelling of the derived home location must refuse")
	}
	if !guard.Refused(filepath.Join(link, ".npmrc")) {
		t.Error("Refused(<home link>/.npmrc) = false — the lexical spelling of the derived home location must refuse")
	}
}

func TestCredentialReadGuardNilEnvReadsTheProcess(t *testing.T) {
	// A nil env falls back to os.Getenv: the derivation compiles and the
	// intrinsic carriers refuse without any environment at all.
	guard := NewCredentialReadGuard(nil)
	if !guard.Refused("/anywhere/.env") {
		t.Error("Refused(/anywhere/.env) = false with a nil env — intrinsic carriers do not depend on derivation")
	}
}
