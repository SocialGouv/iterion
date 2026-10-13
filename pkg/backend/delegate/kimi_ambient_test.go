package delegate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// fakeKimiReportingItsHome writes a kimi stand-in that records the home it
// was pointed at (<report>.home), that home's entries (<report>.ls) and its
// config.toml (<report>.config), then — as kimi-code does with
// workspaces.json — replaces a state file there by rename.
func fakeKimiReportingItsHome(t *testing.T) (bin, report string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI is POSIX-only")
	}
	dir := t.TempDir()
	bin, report = filepath.Join(dir, "kimi"), filepath.Join(dir, "report")
	script := `#!/bin/sh
printf '%s\n' "$KIMI_CODE_HOME" > '` + report + `.home'
ls -A "$KIMI_CODE_HOME" > '` + report + `.ls'
cat "$KIMI_CODE_HOME/config.toml" > '` + report + `.config' 2>/dev/null
printf 'rotated\n' > "$KIMI_CODE_HOME/workspaces.json.tmp" && mv "$KIMI_CODE_HOME/workspaces.json.tmp" "$KIMI_CODE_HOME/workspaces.json"
printf '%s\n' '{"role":"assistant","content":"done"}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { // #nosec G306 — test fixture must be executable
		t.Fatal(err)
	}
	return bin, report
}

// kimiAmbientHome is an operator's kimi home holding an AGENTS.md.
func kimiAmbientHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	mustWriteFile(t, filepath.Join(home, "AGENTS.md"), "operator instructions\n")
	mustWriteFile(t, filepath.Join(home, "config.toml"), "default_model = \"k\"\n")
	mustWriteFile(t, filepath.Join(home, "workspaces.json"), "old\n")
	return home
}

// isolateUserHome points HOME at an empty directory, so the operator's own
// ~/.agents never decides a test.
func isolateUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func readReport(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake kimi left no report %s: %v", path, err)
	}
	return string(b)
}

func listsEntry(listing, name string) bool {
	return strings.Contains("\n"+listing, "\n"+name+"\n")
}

func TestKimiWorkspaceServesAHomeWithoutItsAGENTSmd(t *testing.T) {
	isolateUserHome(t)
	home := kimiAmbientHome(t)
	bin, report := fakeKimiReportingItsHome(t)
	task := Task{NodeID: "n", UserPrompt: "hi", AmbientContext: ambient.Workspace, AmbientContextSource: "node", ExtraEnv: []string{"KIMI_CODE_HOME=" + home}}
	if _, err := NewKimiBackend(testLogger(), bin).Execute(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	seen := strings.TrimSpace(readReport(t, report+".home"))
	if seen == home || !strings.HasPrefix(seen, filepath.Join(home, ambientOverlayDirName)+string(filepath.Separator)) {
		t.Fatalf("KIMI_CODE_HOME=%s, want an overlay inside %s", seen, home)
	}
	listing := readReport(t, report+".ls")
	if listsEntry(listing, "AGENTS.md") {
		t.Errorf("the operator's AGENTS.md reached kimi:\n%s", listing)
	}
	for _, want := range []string{"config.toml", "workspaces.json"} {
		if !listsEntry(listing, want) {
			t.Errorf("%s is missing from the home kimi was served:\n%s", want, listing)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, "workspaces.json")); string(b) != "rotated\n" {
		t.Errorf("home workspaces.json = %q: the file kimi replaced was not carried back", b)
	}
	if _, err := os.Stat(filepath.Join(home, ambientOverlayDirName)); !os.IsNotExist(err) {
		t.Errorf("the overlay outlived the invocation: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "AGENTS.md")); string(b) != "operator instructions\n" {
		t.Errorf("the operator's AGENTS.md was touched: %q", b)
	}
}

func TestKimiAllLeavesTheHomeAlone(t *testing.T) {
	isolateUserHome(t)
	home := kimiAmbientHome(t)
	bin, report := fakeKimiReportingItsHome(t)
	task := Task{NodeID: "n", UserPrompt: "hi", AmbientContext: ambient.All, AmbientContextSource: "node", ExtraEnv: []string{"KIMI_CODE_HOME=" + home}}
	if _, err := NewKimiBackend(testLogger(), bin).Execute(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if seen := strings.TrimSpace(readReport(t, report+".home")); seen != home {
		t.Errorf("KIMI_CODE_HOME=%s, want the operator's home %s", seen, home)
	}
	if listing := readReport(t, report+".ls"); !listsEntry(listing, "AGENTS.md") {
		t.Errorf("all must keep the operator's AGENTS.md:\n%s", listing)
	}
}

func TestKimiRefusesADeclaredPolicyAHostFileDefeats(t *testing.T) {
	userHome := isolateUserHome(t)
	generic := filepath.Join(userHome, ".agents", "agents.md")
	mustWriteFile(t, generic, "generic instructions\n")
	home := kimiAmbientHome(t)
	bin, report := fakeKimiReportingItsHome(t)
	task := Task{NodeID: "n", UserPrompt: "hi", AmbientContext: ambient.Workspace, AmbientContextSource: "workflow", ExtraEnv: []string{"KIMI_CODE_HOME=" + home}}
	_, err := NewKimiBackend(testLogger(), bin).Execute(context.Background(), task)
	var refused *ErrCapabilityUnsupported
	if !errors.As(err, &refused) {
		t.Fatalf("a declared workspace kimi cannot apply here must be refused, got %v", err)
	}
	for _, want := range []string{"ambient_context: workspace (from workflow)", generic, "declare ambient_context: all"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
	if _, err := os.Stat(report + ".home"); !os.IsNotExist(err) {
		t.Errorf("kimi ran despite the refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ambientOverlayDirName)); !os.IsNotExist(err) {
		t.Errorf("an overlay was built for a refused node: %v", err)
	}
}

func TestKimiDefaultReportsAHostFileOnceAndWithholdsTheRest(t *testing.T) {
	userHome := isolateUserHome(t)
	mustWriteFile(t, filepath.Join(userHome, ".agents", "AGENTS.md"), "generic instructions\n")
	home := kimiAmbientHome(t)
	bin, report := fakeKimiReportingItsHome(t)
	var log bytes.Buffer
	b := NewKimiBackend(iterlog.New(iterlog.LevelInfo, &log), bin)
	task := Task{NodeID: "n", UserPrompt: "hi", AmbientContext: ambient.Workspace, ExtraEnv: []string{"KIMI_CODE_HOME=" + home}}
	for range 2 {
		if _, err := b.Execute(context.Background(), task); err != nil {
			t.Fatalf("the default must run: %v", err)
		}
	}
	if n := strings.Count(log.String(), "cannot be applied on this host"); n != 1 {
		t.Errorf("the host file was reported %d times, want once:\n%s", n, log.String())
	}
	if listing := readReport(t, report+".ls"); listsEntry(listing, "AGENTS.md") {
		t.Errorf("the home's AGENTS.md must still be withheld:\n%s", listing)
	}
}

func TestKimiPermissionShadowIsBuiltFromTheWithheldHome(t *testing.T) {
	isolateUserHome(t)
	iterionBin := filepath.Join(t.TempDir(), "iterion")
	if err := os.WriteFile(iterionBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { // #nosec G306 -- executable test fixture.
		t.Fatal(err)
	}
	t.Setenv("ITERION_BIN", iterionBin)
	home := kimiAmbientHome(t)
	bin, report := fakeKimiReportingItsHome(t)
	policy, err := permission.NewPolicy(permission.ModeDeny, []string{"Read(**)"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := Task{NodeID: "n", UserPrompt: "hi", StoreDir: t.TempDir(), Permission: policy, AmbientContext: ambient.Workspace, AmbientContextSource: "node", ExtraEnv: []string{"KIMI_CODE_HOME=" + home}}
	if _, err := NewKimiBackend(testLogger(), bin).Execute(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if listing := readReport(t, report+".ls"); listsEntry(listing, "AGENTS.md") {
		t.Errorf("the permission shadow linked the withheld AGENTS.md back in:\n%s", listing)
	}
	config := readReport(t, report+".config")
	for _, want := range []string{`default_model = "k"`, "[[hooks]]"} {
		if !strings.Contains(config, want) {
			t.Errorf("shadow config.toml lacks %q:\n%s", want, config)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ambientOverlayDirName)); !os.IsNotExist(err) {
		t.Errorf("the overlay outlived the gated invocation: %v", err)
	}
}

func TestKimiSandboxedRunKeepsTheContainersHome(t *testing.T) {
	userHome := isolateUserHome(t)
	mustWriteFile(t, filepath.Join(userHome, ".agents", "AGENTS.md"), "host generic instructions\n")
	home := kimiAmbientHome(t)
	run := &recordingRun{script: `printf '%s\n' '{"role":"assistant","content":"done"}'`}
	task := Task{NodeID: "n", UserPrompt: "hi", Sandbox: run, AmbientContext: ambient.Workspace, AmbientContextSource: "node", ExtraEnv: []string{"KIMI_CODE_HOME=" + home}}
	if _, err := NewKimiBackend(testLogger(), "").Execute(context.Background(), task); err != nil {
		t.Fatalf("a containerised kimi reads the image's home, not the host's: %v", err)
	}
	if got := run.gotOpts.Env["KIMI_CODE_HOME"]; got != home {
		t.Errorf("KIMI_CODE_HOME=%q, want the run's own value %q: a host overlay means nothing in a container", got, home)
	}
	if _, err := os.Stat(filepath.Join(home, ambientOverlayDirName)); !os.IsNotExist(err) {
		t.Errorf("a host overlay was built for a containerised run: %v", err)
	}
}

func TestKimiNoopSandboxIsServedTheOverlay(t *testing.T) {
	isolateUserHome(t)
	home := kimiAmbientHome(t)
	run := &recordingRun{driver: "noop", script: `printf '%s\n' '{"role":"assistant","content":"done"}'`}
	task := Task{NodeID: "n", UserPrompt: "hi", Sandbox: run, AmbientContext: ambient.Workspace, AmbientContextSource: "node", ExtraEnv: []string{"KIMI_CODE_HOME=" + home}}
	if _, err := NewKimiBackend(testLogger(), "").Execute(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if got := run.gotOpts.Env["KIMI_CODE_HOME"]; !strings.HasPrefix(got, filepath.Join(home, ambientOverlayDirName)+string(filepath.Separator)) {
		t.Errorf("KIMI_CODE_HOME=%q: the noop driver runs on the host and must be served the overlay", got)
	}
}

// When the user's home IS the workspace's repo root (dotfiles repos, CI
// images whose HOME sits in a repository), kimi's walk starts at the home and
// its root-level files load: they are operator files, reported so a declared
// policy refuses and the default warns. Outside a repository, or with the
// workspace rooted elsewhere, none of that applies.
func TestKimiReportsItsRootFilesWhenTheHomeIsTheRepoRoot(t *testing.T) {
	home := isolateUserHome(t)
	mustWriteFile(t, filepath.Join(home, ".kimi-code", "AGENTS.md"), "root marker\n")
	mustWriteFile(t, filepath.Join(home, "AGENTS.md"), "root agents\n")
	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := Task{NodeID: "n", UserPrompt: "hi", WorkDir: filepath.Join(home, "project")}
	got := kimiHostInstructions(task)
	root := filepath.Join(home, ".kimi-code", "AGENTS.md")
	generic := filepath.Join(home, "AGENTS.md")
	if len(got) != 2 || got[0] != root || got[1] != generic {
		t.Errorf("kimiHostInstructions with HOME as the repo root = %v, want [%s %s]", got, root, generic)
	}

	// The same layout outside any repository reports nothing: the walk never
	// starts there.
	if err := os.RemoveAll(filepath.Join(home, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := kimiHostInstructions(task); got != nil {
		t.Errorf("kimiHostInstructions with HOME outside a repository = %v, want none", got)
	}
}
