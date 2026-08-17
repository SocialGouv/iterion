package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// TestResolveRepoDevbox_PrecedenceChain pins the chain the rest of the
// engine uses — CLI/launch override → workflow block → ITERION_REPO_DEVBOX
// → on — including the layer-skipping that makes it a chain rather than a
// cascade of ifs: an unset layer must not answer for the next one.
func TestResolveRepoDevbox_PrecedenceChain(t *testing.T) {
	cases := []struct {
		name     string
		override string
		workflow string
		env      string
		want     bool
	}{
		{name: "nothing set defaults on", want: true},
		{name: "workflow off", workflow: "off", want: false},
		{name: "workflow on", workflow: "on", want: true},
		{name: "env off", env: "off", want: false},
		{name: "env 0 spelling", env: "0", want: false},
		{name: "workflow beats env", workflow: "on", env: "off", want: true},
		{name: "override beats workflow", override: "on", workflow: "off", want: true},
		{name: "override off beats a workflow on", override: "off", workflow: "on", want: false},
		{name: "override beats env", override: "off", env: "on", want: false},
		// An unreadable layer is not a decision: it defers, it does not
		// silently mean "off".
		{name: "garbage override defers to workflow", override: "maybe", workflow: "off", want: false},
		{name: "garbage everywhere defaults on", override: "maybe", workflow: "sometimes", env: "?", want: true},
		{name: "case and space tolerated", workflow: "  OFF ", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ITERION_REPO_DEVBOX", tc.env)
			var wf *ir.Workflow
			if tc.workflow != "" {
				wf = &ir.Workflow{RepoDevbox: tc.workflow}
			}
			if got := resolveRepoDevbox(tc.override, wf); got != tc.want {
				t.Errorf("resolveRepoDevbox(%q, %q) with env %q = %v, want %v",
					tc.override, tc.workflow, tc.env, got, tc.want)
			}
		})
	}
}

// TestValidateRepoDevboxMode: a typo must be refused at the flag boundary.
// Accepting it would read as "inherit" — the default, ON — so an operator
// who typed `--repo-devbox of` would pay the very install they meant to
// skip, and nothing would say so.
func TestValidateRepoDevboxMode(t *testing.T) {
	for _, ok := range []string{"", "on", "off", "ON", " off "} {
		if err := ValidateRepoDevboxMode(ok); err != nil {
			t.Errorf("ValidateRepoDevboxMode(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"of", "yes", "true", "1", "none"} {
		if err := ValidateRepoDevboxMode(bad); err == nil {
			t.Errorf("ValidateRepoDevboxMode(%q) = nil, want an error", bad)
		}
	}
}

// TestApplyDevboxProvisioning_RepoDevboxOffSkipsOnlyTheRepo is the
// composition guarantee on the sandbox path: with `repo_devbox: off` the
// target repo's toolchain is NOT installed, while the BOT's own devbox.json
// still is. Declining one source must not disarm the other — a bot that
// declares `crane` needs `crane` whatever the repo it is pointed at.
func TestApplyDevboxProvisioning_RepoDevboxOffSkipsOnlyTheRepo(t *testing.T) {
	f := newDevboxFixture(t, true, true)
	f.params.Workflow = &ir.Workflow{RepoDevbox: "off"}
	f.apply(t)

	if strings.Contains(f.spec.PostCreate, "devbox install -c "+f.params.WorkspacePath) {
		t.Errorf("repo_devbox off must not install the repo project, got:\n%s", f.spec.PostCreate)
	}
	if strings.Contains(f.spec.Env["PATH"], f.params.WorkspacePath+"/"+devboxProfileBin) {
		t.Errorf("repo_devbox off must keep the repo profile off PATH, got %q", f.spec.Env["PATH"])
	}
	if !strings.Contains(f.spec.PostCreate, "devbox install -c "+botDevboxDir) {
		t.Errorf("the bot's own devbox.json must still be installed, got:\n%s", f.spec.PostCreate)
	}
	if !strings.HasPrefix(f.spec.Env["PATH"], botDevboxDir+"/"+devboxProfileBin+":") {
		t.Errorf("the bot profile bin must lead PATH, got %q", f.spec.Env["PATH"])
	}
}

// TestApplyDevboxProvisioning_RepoDevboxOffIsReported: a source that was
// declared and deliberately not installed must be VISIBLE. Otherwise the
// only trace of the decision is a binary that is missing later, which reads
// as an agent bug — the failure mode this whole package logs against.
func TestApplyDevboxProvisioning_RepoDevboxOffIsReported(t *testing.T) {
	t.Run("with a bot source still installing", func(t *testing.T) {
		f := newDevboxFixture(t, true, true)
		f.params.Workflow = &ir.Workflow{RepoDevbox: "off"}
		f.apply(t)
		assertRepoSkipReported(t, f)
	})
	// The harder half: nothing is left to install, so the provisioning
	// returns early — the early return is exactly where a skip goes
	// unreported if nobody wired it.
	t.Run("with nothing left to install", func(t *testing.T) {
		f := newDevboxFixture(t, true, false)
		f.params.Workflow = &ir.Workflow{RepoDevbox: "off"}
		f.apply(t)
		assertRepoSkipReported(t, f)
		if f.spec.PostCreate != "" {
			t.Errorf("nothing should be installed, got PostCreate:\n%s", f.spec.PostCreate)
		}
	})
}

func assertRepoSkipReported(t *testing.T, f *devboxFixture) {
	t.Helper()
	for _, data := range f.eventData {
		sources, _ := data["skipped_sources"].([]string)
		if len(sources) == 1 && sources[0] == "repo" {
			configs, _ := data["skipped_configs"].([]string)
			if len(configs) != 1 || !strings.HasPrefix(configs[0], f.params.WorkspacePath) {
				t.Errorf("the skip must name the config it declined, got %v", configs)
			}
			return
		}
	}
	t.Fatalf("no event reported the declined repo source; events=%v data=%v", f.events, f.eventData)
}

// TestApplyDevboxProvisioning_RepoDevboxOnIsTheDefault guards the direction
// of the switch: absent any declaration, a repo that pins its toolchain
// still gets it. The escape hatch must not become the behaviour.
func TestApplyDevboxProvisioning_RepoDevboxOnIsTheDefault(t *testing.T) {
	t.Setenv("ITERION_REPO_DEVBOX", "")
	f := newDevboxFixture(t, true, false)
	f.params.Workflow = &ir.Workflow{}
	f.apply(t)

	if !strings.Contains(f.spec.PostCreate, "devbox install -c "+f.params.WorkspacePath) {
		t.Errorf("with no repo_devbox declared the repo project must install, got:\n%s", f.spec.PostCreate)
	}
	for _, data := range f.eventData {
		if _, ok := data["skipped_sources"]; ok {
			t.Errorf("nothing was declined; the event must not claim a skip: %v", data)
		}
	}
}

// TestResolveDevboxProjects_CLIOverrideBeatsTheWorkflow proves the operator
// keeps the last word on the sandbox path too: a bot shipping
// `repo_devbox: off` is overridable per run, so an operator who DOES need
// the target's toolchain is never fenced out by the bot's default.
func TestResolveDevboxProjects_CLIOverrideBeatsTheWorkflow(t *testing.T) {
	workspace := t.TempDir()
	writeDevboxConfig(t, workspace, "go@1.26")
	spec := &sandbox.Spec{WorkspaceFolder: "/work"}
	p := SandboxParams{
		WorkspacePath:      workspace,
		Workflow:           &ir.Workflow{RepoDevbox: "off"},
		RepoDevboxOverride: "on",
	}

	got, skipped := resolveDevboxProjects(spec, p, "", iterlog.Nop())
	if len(got) != 1 || got[0].label != "repo" {
		t.Fatalf("--repo-devbox on must re-enable the repo source, got %+v", got)
	}
	if skipped != "" {
		t.Errorf("nothing was declined, got skipped=%q", skipped)
	}
}

// TestEngineRun_HostDevbox_RepoDevboxOffSkipsOnlyTheRepo is the same
// guarantee on the HOST path — the one EVERY cloud run takes, since the
// runner pod is the isolation boundary and no sandbox starts. It drives the
// real engine, not the resolver: the two paths have already diverged once
// (the feature was asserted downstream of a precondition never true in
// prod), so each is locked against the engine that actually runs it.
func TestEngineRun_HostDevbox_RepoDevboxOffSkipsOnlyTheRepo(t *testing.T) {
	t.Setenv("ITERION_REPO_DEVBOX", "")
	rec := stubHostDevbox(t, nil)
	b, botDir := writeBotBundleDir(t, false)

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")

	wf := devboxTestWorkflow()
	wf.RepoDevbox = "off"
	eng := New(wf, s, execRec,
		WithWorkDir(workDir),
		WithBundle(b),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-repo-devbox-off"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	for _, dir := range rec.installs {
		if dir == workDir {
			t.Errorf("repo_devbox off must not install %s, installs=%v", workDir, rec.installs)
		}
	}
	if len(rec.installs) != 1 {
		t.Fatalf("the bot's own devbox.json must still install, installs=%v (bot dir %s)", rec.installs, botDir)
	}
	if len(execRec.runExtraEnv) != 1 ||
		strings.Contains(execRec.runExtraEnv[0], filepath.Join(workDir, filepath.FromSlash(devboxProfileBin))) {
		t.Errorf("the repo profile must stay off the run's PATH, got %v", execRec.runExtraEnv)
	}
	data := devboxEvent(t, s, runID)
	if data == nil {
		t.Fatal("no sandbox_devbox_provisioned event emitted")
	}
	if got := stringsFromAny(data["sources"]); len(got) != 1 || got[0] != "bot" {
		t.Errorf("event sources = %v, want [bot]", got)
	}
	if got := stringsFromAny(data["skipped_sources"]); len(got) != 1 || got[0] != "repo" {
		t.Errorf("event skipped_sources = %v, want [repo] — a declined source must be visible", got)
	}
}

// TestEngineRun_HostDevbox_RepoDevboxOffWithNothingElseStillReports covers
// the host early return: with the repo declined and no bot source, there is
// nothing to install and the function bails — which is precisely where the
// decision would vanish unreported.
func TestEngineRun_HostDevbox_RepoDevboxOffWithNothingElseStillReports(t *testing.T) {
	t.Setenv("ITERION_REPO_DEVBOX", "")
	rec := stubHostDevbox(t, nil)

	s := tmpStore(t)
	execRec := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	workDir := writeDevboxConfig(t, t.TempDir(), "go@1.26")

	wf := devboxTestWorkflow()
	wf.RepoDevbox = "off"
	eng := New(wf, s, execRec,
		WithWorkDir(workDir),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)

	runID := "run-repo-devbox-off-alone"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	if len(rec.installs) != 0 {
		t.Errorf("nothing should have been installed, got %v", rec.installs)
	}
	data := devboxEvent(t, s, runID)
	if data == nil {
		t.Fatal("the declined repo source must still be reported; no event emitted")
	}
	if got := stringsFromAny(data["skipped_sources"]); len(got) != 1 || got[0] != "repo" {
		t.Errorf("event skipped_sources = %v, want [repo]", got)
	}
}
