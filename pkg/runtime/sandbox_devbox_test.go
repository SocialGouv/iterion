package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// writeDevboxConfig drops a minimal devbox.json in dir and returns dir.
func writeDevboxConfig(t *testing.T, dir string, packages ...string) string {
	t.Helper()
	quoted := make([]string, 0, len(packages))
	for _, p := range packages {
		quoted = append(quoted, `"`+p+`"`)
	}
	body := `{"packages":[` + strings.Join(quoted, ",") + `]}`
	if err := os.WriteFile(filepath.Join(dir, devboxConfigName), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s in %s: %v", devboxConfigName, dir, err)
	}
	return dir
}

// devboxFixture wires the two devbox sources for a test: a workspace dir
// and a bundle dir, each optionally carrying a devbox.json.
type devboxFixture struct {
	spec      *sandbox.Spec
	params    SandboxParams
	events    []store.EventType
	eventData []map[string]any
}

func newDevboxFixture(t *testing.T, repoDevbox, botDevbox bool) *devboxFixture {
	t.Helper()
	workspace := t.TempDir()
	bundle := t.TempDir()
	if repoDevbox {
		writeDevboxConfig(t, workspace, "go@1.26")
	}
	if botDevbox {
		writeDevboxConfig(t, bundle, "go-containerregistry@latest")
	}
	return &devboxFixture{
		spec: &sandbox.Spec{Mode: sandbox.ModeInline, Image: "example:tag"},
		params: SandboxParams{
			WorkspacePath: workspace,
			BundleHostDir: bundle,
		},
	}
}

// apply runs the provisioning with the bundle mounted at the standard
// container path (the mount the engine adds before calling us).
func (f *devboxFixture) apply(t *testing.T) {
	t.Helper()
	emit := func(ev store.EventType, data map[string]any) error {
		f.events = append(f.events, ev)
		f.eventData = append(f.eventData, data)
		return nil
	}
	applyDevboxProvisioning(f.spec, f.params, "/run/iterion/bundle", emit, iterlog.Nop())
}

// TestApplyDevboxProvisioning_BotOnly covers a bot shipping a devbox.json
// next to its main.bot against a repo that declares none. The bundle mount
// is read-only, so the config must be STAGED into a writable directory
// before `devbox install` can write its `.devbox/` profile beside it.
func TestApplyDevboxProvisioning_BotOnly(t *testing.T) {
	f := newDevboxFixture(t, false, true)
	f.apply(t)

	botBin := botDevboxDir + "/" + devboxProfileBin
	if !strings.HasPrefix(f.spec.Env["PATH"], botBin+":") {
		t.Errorf("bot devbox profile bin must lead PATH, got %q", f.spec.Env["PATH"])
	}
	if !strings.Contains(f.spec.PostCreate, "devbox install -c "+botDevboxDir) {
		t.Errorf("PostCreate must install the bot devbox project, got:\n%s", f.spec.PostCreate)
	}
	// Staged out of the read-only bundle mount, never installed in place.
	if !strings.Contains(f.spec.PostCreate, "cp /run/iterion/bundle/"+devboxConfigName+" "+botDevboxDir+"/"+devboxConfigName) {
		t.Errorf("PostCreate must stage the bundle config into a writable dir, got:\n%s", f.spec.PostCreate)
	}
	if strings.Contains(f.spec.PostCreate, "devbox install -c /run/iterion/bundle") {
		t.Errorf("PostCreate must not install from the read-only bundle mount, got:\n%s", f.spec.PostCreate)
	}
}

// TestApplyDevboxProvisioning_RepoOnly covers a target repo declaring a
// devbox.json at its workspace root with no bot devbox. It installs in
// place (relative package refs only resolve next to the config).
func TestApplyDevboxProvisioning_RepoOnly(t *testing.T) {
	f := newDevboxFixture(t, true, false)
	f.apply(t)

	repoBin := f.params.WorkspacePath + "/" + devboxProfileBin
	if !strings.HasPrefix(f.spec.Env["PATH"], repoBin+":") {
		t.Errorf("repo devbox profile bin must lead PATH, got %q", f.spec.Env["PATH"])
	}
	if !strings.Contains(f.spec.PostCreate, "devbox install -c "+f.params.WorkspacePath) {
		t.Errorf("PostCreate must install the repo devbox project in place, got:\n%s", f.spec.PostCreate)
	}
	if strings.Contains(f.spec.PostCreate, botDevboxDir) {
		t.Errorf("no bot devbox.json exists; PostCreate must not reference %s:\n%s", botDevboxDir, f.spec.PostCreate)
	}
}

// TestApplyDevboxProvisioning_BothSources is the composition guarantee:
// when a bot AND its target repo each declare a devbox.json, both are
// installed and both land on PATH — neither silently wins. Order is
// repo-then-bot so a repo's pinned toolchain stays authoritative for
// building itself.
func TestApplyDevboxProvisioning_BothSources(t *testing.T) {
	f := newDevboxFixture(t, true, true)
	f.apply(t)

	repoBin := f.params.WorkspacePath + "/" + devboxProfileBin
	botBin := botDevboxDir + "/" + devboxProfileBin

	entries := strings.Split(f.spec.Env["PATH"], ":")
	if len(entries) < 3 || entries[0] != repoBin || entries[1] != botBin {
		t.Fatalf("PATH must lead with repo then bot profile bins, got %q", f.spec.Env["PATH"])
	}
	for _, want := range []string{
		"devbox install -c " + f.params.WorkspacePath,
		"devbox install -c " + botDevboxDir,
	} {
		if !strings.Contains(f.spec.PostCreate, want) {
			t.Errorf("PostCreate missing %q, got:\n%s", want, f.spec.PostCreate)
		}
	}

	if len(f.events) != 1 || f.events[0] != store.EventSandboxDevboxProvisioned {
		t.Fatalf("want a single %s event, got %v", store.EventSandboxDevboxProvisioned, f.events)
	}
	sources, _ := f.eventData[0]["sources"].([]string)
	if len(sources) != 2 || sources[0] != "repo" || sources[1] != "bot" {
		t.Errorf("event sources = %v, want [repo bot]", sources)
	}
}

// TestApplyDevboxProvisioning_NoDevbox is the zero-cost guarantee: a run
// whose bot and repo declare no devbox.json must not gain a PostCreate
// step (a cold Nix realise costs minutes) nor a PATH entry.
func TestApplyDevboxProvisioning_NoDevbox(t *testing.T) {
	f := newDevboxFixture(t, false, false)
	f.spec.PostCreate = "npm ci"
	f.apply(t)

	if f.spec.PostCreate != "npm ci" {
		t.Errorf("PostCreate must be untouched when nothing declares a devbox.json, got %q", f.spec.PostCreate)
	}
	if _, ok := f.spec.Env["PATH"]; ok {
		t.Errorf("PATH must be untouched when nothing declares a devbox.json, got %q", f.spec.Env["PATH"])
	}
	if len(f.events) != 0 {
		t.Errorf("no devbox event expected, got %v", f.events)
	}
}

// TestApplyDevboxProvisioning_PATHIsPrependedNotReplaced guards the crux:
// the bot author's own `sandbox.env.PATH:` must survive as the suffix.
// Clobbering it would strip whatever the image or the author put there
// (node shims, a baked CLI) and break the run in a way that reads as an
// unrelated bug.
func TestApplyDevboxProvisioning_PATHIsPrependedNotReplaced(t *testing.T) {
	f := newDevboxFixture(t, true, true)
	const authored = "/opt/custom/bin:/usr/local/bin:/usr/bin:/bin"
	f.spec.Env = map[string]string{"PATH": authored, "CLAUDE_CONFIG_DIR": "/home/devbox/.claude"}
	f.apply(t)

	got := f.spec.Env["PATH"]
	if !strings.HasSuffix(got, ":"+authored) {
		t.Errorf("authored PATH must survive as the suffix; got %q, want it to end with %q", got, authored)
	}
	repoBin := f.params.WorkspacePath + "/" + devboxProfileBin
	if !strings.HasPrefix(got, repoBin+":") {
		t.Errorf("devbox bin dirs must be prepended; got %q", got)
	}
	// Unrelated env entries are not collateral damage.
	if f.spec.Env["CLAUDE_CONFIG_DIR"] != "/home/devbox/.claude" {
		t.Errorf("sibling env vars must be untouched, got %q", f.spec.Env["CLAUDE_CONFIG_DIR"])
	}
}

// TestApplyDevboxProvisioning_PATHFallbackBase covers the no-authored-PATH
// case: the devbox bins prepend to the FHS default the sandbox images
// ship, so the container keeps a usable base PATH.
func TestApplyDevboxProvisioning_PATHFallbackBase(t *testing.T) {
	f := newDevboxFixture(t, true, false)
	f.apply(t)

	if !strings.HasSuffix(f.spec.Env["PATH"], ":"+fallbackContainerPATH) {
		t.Errorf("PATH must fall back to the FHS base, got %q", f.spec.Env["PATH"])
	}
	for _, dir := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		if !strings.Contains(f.spec.Env["PATH"], dir) {
			t.Errorf("fallback PATH must retain %s, got %q", dir, f.spec.Env["PATH"])
		}
	}
}

// TestApplyDevboxProvisioning_PostCreateIsPrependedNotReplaced: the bot's
// own post_create must survive, and run AFTER the install so it can use
// what devbox provided. Its leading `set -e` must not govern the install
// prologue — a best-effort install cannot be allowed to abort the run.
func TestApplyDevboxProvisioning_PostCreateIsPrependedNotReplaced(t *testing.T) {
	f := newDevboxFixture(t, false, true)
	const authored = "set -e; claude --version >/dev/null 2>&1 || sudo npm install -g @anthropic-ai/claude-code@latest"
	f.spec.PostCreate = authored
	f.apply(t)

	if !strings.HasSuffix(f.spec.PostCreate, authored) {
		t.Errorf("authored post_create must survive as the tail, got:\n%s", f.spec.PostCreate)
	}
	installIdx := strings.Index(f.spec.PostCreate, "devbox install")
	authoredIdx := strings.Index(f.spec.PostCreate, authored)
	if installIdx < 0 || installIdx > authoredIdx {
		t.Errorf("devbox install must precede the authored post_create, got:\n%s", f.spec.PostCreate)
	}
	// The prologue is newline-separated so `set -e` can't reach backwards.
	if !strings.Contains(f.spec.PostCreate, "fi\n"+authored) {
		t.Errorf("prologue and authored snippet must be newline-separated, got:\n%s", f.spec.PostCreate)
	}
}

// TestApplyDevboxProvisioning_BotDevboxNeedsBundleMount: with no bundle
// bind-mount (a driver with no host filesystem, or a bare .bot with no
// bundle), the bot's devbox.json is unreachable in-container and must be
// skipped rather than producing a `cp` from a path that does not exist.
func TestApplyDevboxProvisioning_BotDevboxNeedsBundleMount(t *testing.T) {
	f := newDevboxFixture(t, false, true)
	emit := func(store.EventType, map[string]any) error { return nil }

	applyDevboxProvisioning(f.spec, f.params, "", emit, iterlog.Nop())

	if f.spec.PostCreate != "" {
		t.Errorf("bot devbox needs the bundle mount; PostCreate must stay empty, got:\n%s", f.spec.PostCreate)
	}
	if _, ok := f.spec.Env["PATH"]; ok {
		t.Errorf("PATH must stay untouched without the bundle mount, got %q", f.spec.Env["PATH"])
	}
}

// TestApplyDevboxProvisioning_ExplicitWorkspaceFolder: when the spec pins
// workspace_folder, the repo's profile bin must be computed against that
// in-container path, not the host one.
func TestApplyDevboxProvisioning_ExplicitWorkspaceFolder(t *testing.T) {
	f := newDevboxFixture(t, true, false)
	f.spec.WorkspaceFolder = "/workspace"
	f.apply(t)

	want := "/workspace/" + devboxProfileBin
	if !strings.HasPrefix(f.spec.Env["PATH"], want+":") {
		t.Errorf("PATH must use the in-container workspace folder %q, got %q", want, f.spec.Env["PATH"])
	}
	if strings.Contains(f.spec.PostCreate, f.params.WorkspacePath) {
		t.Errorf("PostCreate must target the container path, not the host one, got:\n%s", f.spec.PostCreate)
	}
}

// TestDevboxInstallSnippet_BestEffortAndPOSIX: the prologue must degrade
// loudly (every failure path echoes what is missing to stderr) and stay
// POSIX — PostCreate runs through `sh -c`, which is dash on the sandbox
// images.
func TestDevboxInstallSnippet_BestEffortAndPOSIX(t *testing.T) {
	snippet := devboxInstallSnippet([]devboxProject{
		{label: "repo", dir: "/workspace"},
		{label: "bot", dir: botDevboxDir, stageFrom: "/run/iterion/bundle"},
	})

	// One "what is now missing" message per project, plus the
	// devbox-absent branch.
	if n := strings.Count(snippet, ">&2"); n != 3 {
		t.Errorf("every failure path must report to stderr; got %d redirects in:\n%s", n, snippet)
	}
	if !strings.Contains(snippet, "command -v devbox") {
		t.Errorf("snippet must probe for devbox before using it:\n%s", snippet)
	}
	for _, bashism := range []string{"[[", "<<<", "${!"} {
		if strings.Contains(snippet, bashism) {
			t.Errorf("snippet must be POSIX sh; found %q in:\n%s", bashism, snippet)
		}
	}
	// A best-effort step must never abort the container's post-create.
	if strings.Contains(snippet, "set -e") || strings.Contains(snippet, "exit 1") {
		t.Errorf("snippet must not abort post-create:\n%s", snippet)
	}
}

// TestDevboxInstallSnippet_ParsesAsShell runs the generated prologue
// through `sh -n`. PostCreate is handed to `sh -c` inside the container,
// so a syntax slip would only surface as a failed post-create on a live
// sandboxed run — far from the code that generated it. Paths are quoted
// via shellquote, so a workspace with a space or a quote in its name must
// still parse.
func TestDevboxInstallSnippet_ParsesAsShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	for _, tc := range []struct {
		name     string
		projects []devboxProject
	}{
		{"repo only", []devboxProject{{label: "repo", dir: "/work/repo"}}},
		{"bot only", []devboxProject{{label: "bot", dir: botDevboxDir, stageFrom: "/run/iterion/bundle"}}},
		{"both", []devboxProject{
			{label: "repo", dir: "/work/repo"},
			{label: "bot", dir: botDevboxDir, stageFrom: "/run/iterion/bundle"},
		}},
		{"hostile paths", []devboxProject{
			{label: "repo", dir: "/work/my repo's dir; rm -rf /"},
			{label: "bot", dir: botDevboxDir, stageFrom: "/run/$(whoami)/bundle"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snippet := devboxInstallSnippet(tc.projects)
			cmd := exec.Command(sh, "-n")
			cmd.Stdin = strings.NewReader(snippet)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("generated snippet is not valid POSIX sh: %v\n%s\n--- snippet ---\n%s", err, out, snippet)
			}
		})
	}
}

// TestResolveDevboxProjects_RejectsPathPoisoningDir: a container project
// dir carrying a PATH separator would corrupt Spec.Env["PATH"] for every
// command in the run. It is dropped, not smuggled in.
func TestResolveDevboxProjects_RejectsPathPoisoningDir(t *testing.T) {
	workspace := t.TempDir()
	writeDevboxConfig(t, workspace, "go@1.26")
	spec := &sandbox.Spec{WorkspaceFolder: "/work:space"}
	p := SandboxParams{WorkspacePath: workspace}

	if got := resolveDevboxProjects(spec, p, "", iterlog.Nop()); len(got) != 0 {
		t.Errorf("a dir containing ':' must be dropped, got %+v", got)
	}
}
