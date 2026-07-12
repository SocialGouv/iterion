package docker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestCapabilitiesAdvertisedFeatures(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	caps := d.Capabilities()

	if !caps.SupportsImage {
		t.Error("docker driver must advertise SupportsImage")
	}
	if !caps.SupportsMounts {
		t.Error("docker driver must advertise SupportsMounts")
	}
	if !caps.SupportsPostCreate {
		t.Error("docker driver must advertise SupportsPostCreate")
	}
	if !caps.SupportsRemoteUser {
		t.Error("docker driver must advertise SupportsRemoteUser")
	}
	if !caps.SupportsBuild {
		t.Error("V2-6 docker driver must advertise SupportsBuild (docker buildx)")
	}
	// Phase 1 explicitly defers these.
	if caps.SupportsNetworkPolicy {
		t.Error("Phase 1 docker driver must NOT advertise SupportsNetworkPolicy")
	}
}

func TestPrepareAcceptsBuild(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	prepared, err := d.Prepare(context.Background(), sandbox.Spec{
		Mode:  sandbox.ModeInline,
		Build: &sandbox.Build{Dockerfile: "Dockerfile"},
		User:  "1000:1000",
	})
	if err != nil {
		t.Fatalf("Prepare must accept sandbox.Build (V2-6): %v", err)
	}
	if prepared == nil {
		t.Fatal("expected non-nil PreparedSpec")
	}
}

func TestPrepareRejectsMissingImage(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	_, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.ModeInline})
	if err == nil {
		t.Fatal("expected rejection of inline spec without image")
	}
}

func TestPrepareValidatesSpec(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	_, err := d.Prepare(context.Background(), sandbox.Spec{Mode: sandbox.Mode("nonsense")})
	if err == nil {
		t.Fatal("expected validation error for invalid mode")
	}
}

func TestContainerNameDeterministic(t *testing.T) {
	a := containerNameFor("run_123")
	b := containerNameFor("run_123")
	if a != b {
		t.Errorf("containerNameFor not deterministic: %q vs %q", a, b)
	}
	if a != "iterion-run_123" {
		t.Errorf("name = %q, want iterion-run_123", a)
	}
}

func TestContainerNameTruncatesLong(t *testing.T) {
	long := "run_" + string(make([]byte, 200))
	for i := range long[4:] {
		long = long[:4+i] + "x" + long[4+i+1:]
	}
	got := containerNameFor(long)
	if len(got) > 64 {
		t.Errorf("name %q exceeds 64 chars", got)
	}
}

func TestContainerShortIDTruncates(t *testing.T) {
	full := "abcdef0123456789abcdef"
	got := containerShortID(full)
	if got != "abcdef012345" {
		t.Errorf("shortID = %q, want abcdef012345", got)
	}
}

func TestContainerShortIDPassesThroughShort(t *testing.T) {
	got := containerShortID("abc")
	if got != "abc" {
		t.Errorf("shortID = %q, want abc", got)
	}
}

func TestStartRejectsForeignPreparedSpec(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	// Use a noop-shaped PreparedSpec — DriverName != docker, must reject.
	_, err := d.Start(context.Background(), foreignPrepared{}, sandbox.RunInfo{})
	if err == nil {
		t.Fatal("expected rejection of foreign PreparedSpec")
	}
}

type foreignPrepared struct{}

func (foreignPrepared) DriverName() string { return "not-docker" }

// The docker driver must bind the network proxy on 0.0.0.0, not the
// engine-default 127.0.0.1, because the container reaches the proxy via
// host.docker.internal (resolved to host-gateway, e.g. 172.17.0.1) —
// not the host's loopback. A 127.0.0.1 bind silently lands every
// outbound HTTPS_PROXY connection in ECONNREFUSED, which surfaces in
// claude as "API Error: Unable to connect to API (ConnectionRefused)".
func TestProxyConfigBindsAllInterfaces(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	bind, advertise, err := d.ProxyConfig()
	if err != nil {
		t.Fatalf("ProxyConfig: %v", err)
	}
	if bind != "0.0.0.0:0" {
		t.Errorf("bind = %q, want 0.0.0.0:0 (loopback bind is unreachable from container via host.docker.internal)", bind)
	}
	if advertise != "host.docker.internal" {
		t.Errorf("advertise = %q, want host.docker.internal", advertise)
	}
}

// Run.Command must add `docker exec --interactive` whenever Stdin is
// attached OR KeepStdinOpen is set. The latter exists for callers that
// build the *exec.Cmd here and then attach stdin via cmd.StdinPipe()
// after the fact (claudesdk Session via CommandBuilder); without the
// flag, docker closes the child's stdin before the pipe is wired and
// the in-container CLI exits silently with no result.
func TestRunCommandKeepStdinOpenAddsInteractiveFlag(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	r := &Run{
		driver:      d,
		containerID: "deadbeef",
		prepared: &Prepared{
			workspace: "/workspace",
			spec:      sandbox.Spec{},
			runtime:   RuntimeDocker,
		},
	}

	cases := []struct {
		name string
		opts sandbox.ExecOpts
		want bool
	}{
		{"empty opts", sandbox.ExecOpts{}, false},
		{"keep stdin open", sandbox.ExecOpts{KeepStdinOpen: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := r.Command(context.Background(), []string{"true"}, tc.opts)
			got := false
			for _, a := range cmd.Args {
				if a == "--interactive" {
					got = true
					break
				}
			}
			if got != tc.want {
				t.Errorf("--interactive present=%v, want %v; args=%v", got, tc.want, cmd.Args)
			}
		})
	}
}

func TestIsContainerNameConflict(t *testing.T) {
	// Real docker daemon message (capture from a live `docker run --name X`
	// against an existing container). The substring match must hold
	// across docker / podman wording variations.
	dockerMsg := []byte(`docker: Error response from daemon: Conflict. The container name "/iterion-run_12345" is already in use by container "abcdef0123456789". You have to remove (or rename) that container to be able to reuse that name.

Run 'docker run --help' for more information
`)
	if !isContainerNameConflict(dockerMsg) {
		t.Error("expected docker conflict message to match")
	}
	// Podman-flavoured wording.
	podmanMsg := []byte(`Error: the container name "iterion-run_12345" is already in use by 0123... You have to remove that container to be able to reuse that name`)
	if !isContainerNameConflict(podmanMsg) {
		t.Error("expected podman conflict message to match")
	}
	// Unrelated docker error must NOT match — otherwise we'd silently
	// force-remove containers we shouldn't.
	other := []byte(`docker: Error response from daemon: pull access denied for iterion/missing`)
	if isContainerNameConflict(other) {
		t.Error("unrelated docker error must not match")
	}
	if isContainerNameConflict(nil) {
		t.Error("nil/empty must not match")
	}
}

func TestAppendSecretFileMountArgsMountsDefaultDirOnce(t *testing.T) {
	tempDirs := []string{}
	args, locs, err := appendSecretFileMountArgs([]string{"run"}, []sandbox.SecretFileMount{
		{Name: "kubeconfig", MountPath: "/run/iterion/secrets/kubeconfig", Value: []byte("one")},
		{Name: "nested", MountPath: "/run/iterion/secrets/nested/token", Value: []byte("two")},
	}, &tempDirs)
	defer cleanupTempDirs(tempDirs)
	if err != nil {
		t.Fatalf("appendSecretFileMountArgs: %v", err)
	}
	if len(tempDirs) != 1 {
		t.Fatalf("tempDirs = %+v, want one directory mount", tempDirs)
	}
	mounts := secretMountArgs(args)
	if len(mounts) != 1 {
		t.Fatalf("mount args = %+v, want one default directory mount", mounts)
	}
	if !strings.Contains(mounts[0], "target="+secrets.SecretFilesMountDir+",readonly") {
		t.Fatalf("default mount target mismatch: %s", mounts[0])
	}
	if got, err := os.ReadFile(filepath.Join(tempDirs[0], "kubeconfig")); err != nil || string(got) != "one" {
		t.Fatalf("default secret file = %q / %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(tempDirs[0], "nested", "token")); err != nil || string(got) != "two" {
		t.Fatalf("nested secret file = %q / %v", got, err)
	}
	// Both default-dir secrets must be recorded as dir-mounted host paths
	// so RefreshSecretFile can rewrite them mid-run.
	for _, name := range []string{"kubeconfig", "nested"} {
		loc, ok := locs[name]
		if !ok || !loc.dirMounted || loc.hostPath == "" {
			t.Fatalf("loc[%q] = %+v, want dir-mounted host path", name, loc)
		}
	}
}

func TestAppendSecretFileMountArgsMountsCustomFileDirectly(t *testing.T) {
	tempDirs := []string{}
	args, locs, err := appendSecretFileMountArgs([]string{"run"}, []sandbox.SecretFileMount{
		{Name: "kubeconfig", MountPath: "/root/.kube/config", Value: []byte("payload")},
	}, &tempDirs)
	defer cleanupTempDirs(tempDirs)
	if err != nil {
		t.Fatalf("appendSecretFileMountArgs: %v", err)
	}
	if len(tempDirs) != 1 {
		t.Fatalf("tempDirs = %+v, want one file mount temp dir", tempDirs)
	}
	mounts := secretMountArgs(args)
	if len(mounts) != 1 {
		t.Fatalf("mount args = %+v, want one direct file mount", mounts)
	}
	if !strings.Contains(mounts[0], "target=/root/.kube/config,readonly") {
		t.Fatalf("custom mount target mismatch: %s", mounts[0])
	}
	// A custom absolute mount_path is a single-FILE bind mount, recorded
	// not-dir-mounted so RefreshSecretFile rewrites it in place.
	if loc, ok := locs["kubeconfig"]; !ok || loc.dirMounted || loc.hostPath == "" {
		t.Fatalf("loc[kubeconfig] = %+v, want single-file host path", loc)
	}
}

func TestAppendSecretFileMountArgsRejectsDirtyPath(t *testing.T) {
	tempDirs := []string{}
	_, _, err := appendSecretFileMountArgs(nil, []sandbox.SecretFileMount{
		{Name: "bad", MountPath: "/run/iterion/secrets/../bad", Value: []byte("payload")},
	}, &tempDirs)
	defer cleanupTempDirs(tempDirs)
	if err == nil {
		t.Fatal("expected dirty mount_path to fail")
	}
}

// TestRefreshSecretFileRewritesBothMountShapes pins the mid-run refresh:
// a rotated token must reach the host bind-mount source for BOTH the
// default directory mount (atomic rename) and a single-file custom mount
// (in-place rewrite through the 0o400 read-only bit), and an unknown
// secret name is an error.
func TestRefreshSecretFileRewritesBothMountShapes(t *testing.T) {
	tempDirs := []string{}
	_, locs, err := appendSecretFileMountArgs(nil, []sandbox.SecretFileMount{
		{Name: "forge_token", MountPath: "/run/iterion/secrets/forge_token", Value: []byte("token-v1")},
		{Name: "kubeconfig", MountPath: "/root/.kube/config", Value: []byte("kube-v1")},
	}, &tempDirs)
	defer cleanupTempDirs(tempDirs)
	if err != nil {
		t.Fatalf("appendSecretFileMountArgs: %v", err)
	}
	r := &Run{secretFiles: locs}
	ctx := context.Background()

	if err := r.RefreshSecretFile(ctx, "forge_token", []byte("token-v2")); err != nil {
		t.Fatalf("refresh dir-mounted: %v", err)
	}
	if got, _ := os.ReadFile(locs["forge_token"].hostPath); string(got) != "token-v2" {
		t.Fatalf("dir-mounted secret = %q, want token-v2", got)
	}

	if err := r.RefreshSecretFile(ctx, "kubeconfig", []byte("kube-v2")); err != nil {
		t.Fatalf("refresh single-file: %v", err)
	}
	if got, _ := os.ReadFile(locs["kubeconfig"].hostPath); string(got) != "kube-v2" {
		t.Fatalf("single-file secret = %q, want kube-v2", got)
	}
	// Perm restored to read-only after the in-place rewrite.
	if fi, _ := os.Stat(locs["kubeconfig"].hostPath); fi.Mode().Perm() != 0o400 {
		t.Fatalf("single-file perm = %v, want 0400", fi.Mode().Perm())
	}
	// A second in-place rewrite must still succeed (perm was restored).
	if err := r.RefreshSecretFile(ctx, "kubeconfig", []byte("kube-v3")); err != nil {
		t.Fatalf("second refresh single-file: %v", err)
	}

	if err := r.RefreshSecretFile(ctx, "nope", []byte("x")); err == nil {
		t.Fatal("refresh of unknown secret must error")
	}
	if err := r.RefreshSecretFile(ctx, "forge_token", nil); err == nil {
		t.Fatal("refresh with empty value must error")
	}
}

func secretMountArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--mount" && strings.Contains(args[i+1], "iterion-secret") {
			out = append(out, args[i+1])
		}
	}
	return out
}

// Regression: a sandboxed `tool` node whose `sh -c <script>` snippet is
// large (e.g. Seki's majority_verdict interpolating three voter
// verdicts) trips the host kernel's ARG_MAX and the docker fork dies
// with "fork/exec /usr/bin/docker: argument list too long". The driver
// must route oversized scripts through stdin (`docker exec -i … sh -s`)
// so the script never enters the argv. Small scripts must keep the
// historical argv shape so behavior is byte-for-byte unchanged for the
// common case.
func TestRunCommandRoutesOversizedScriptViaStdin(t *testing.T) {
	d := &Driver{rt: RuntimeDocker}
	r := &Run{
		driver:      d,
		containerID: "deadbeef",
		prepared: &Prepared{
			workspace: "/workspace",
			spec:      sandbox.Spec{},
			runtime:   RuntimeDocker,
		},
		inContainerWorkspace: "/workspace",
	}

	t.Run("small script stays in argv", func(t *testing.T) {
		const script = "echo hello && ls -la"
		cmd := r.Command(context.Background(), []string{"sh", "-c", script}, sandbox.ExecOpts{})

		if cmd.Stdin != nil {
			t.Errorf("small script: Stdin must be nil (unchanged behavior); got %T", cmd.Stdin)
		}
		if !containsArg(cmd.Args, script) {
			t.Errorf("small script: argv must still carry the script; args=%v", cmd.Args)
		}
		if containsArg(cmd.Args, "-s") {
			t.Errorf("small script: must NOT use `sh -s` stdin shape; args=%v", cmd.Args)
		}
		if containsArg(cmd.Args, "--interactive") {
			t.Errorf("small script: must NOT add --interactive (no stdin attached); args=%v", cmd.Args)
		}
	})

	t.Run("oversized script streamed through stdin", func(t *testing.T) {
		// > maxInlineArgBytes (100_000) — the E2BIG trigger zone.
		big := strings.Repeat("x", maxInlineArgBytes+1)
		script := "cat <<'EOF'\n" + big + "\nEOF\n"
		cmd := r.Command(context.Background(), []string{"sh", "-c", script}, sandbox.ExecOpts{})

		// The script must NOT appear anywhere in the host argv — that's
		// the whole point of the fix.
		for _, a := range cmd.Args {
			if strings.Contains(a, big) {
				t.Fatalf("oversized script leaked into argv (E2BIG risk); arg snippet len=%d", len(a))
			}
		}
		// argv must end in `sh -s` (the stdin-read shell shape).
		if n := len(cmd.Args); n < 2 || cmd.Args[n-2] != "sh" || cmd.Args[n-1] != "-s" {
			t.Fatalf("oversized script: argv must terminate with `sh -s`; got %v", cmd.Args)
		}
		// --interactive must be present so docker keeps the child's
		// stdin open for the streamed script.
		if !containsArg(cmd.Args, "--interactive") {
			t.Fatalf("oversized script: --interactive required for streamed stdin; args=%v", cmd.Args)
		}
		// Stdin must yield exactly the original script.
		if cmd.Stdin == nil {
			t.Fatal("oversized script: Stdin must be wired")
		}
		got, err := io.ReadAll(cmd.Stdin)
		if err != nil {
			t.Fatalf("read Stdin: %v", err)
		}
		if string(got) != script {
			t.Fatalf("Stdin payload mismatch: got %d bytes, want %d", len(got), len(script))
		}
	})

	t.Run("oversized but caller-provided stdin keeps argv shape", func(t *testing.T) {
		// When the caller already wired Stdin (e.g. claudesdk Session),
		// we MUST NOT clobber it — fall back to the argv path even if
		// the script is large. (The caller knows what they're doing;
		// e.g. they may have set KeepStdinOpen + StdinPipe.)
		big := strings.Repeat("y", maxInlineArgBytes+1)
		callerStdin := strings.NewReader("caller payload")
		cmd := r.Command(context.Background(), []string{"sh", "-c", big}, sandbox.ExecOpts{Stdin: callerStdin})

		if cmd.Stdin != callerStdin {
			t.Errorf("caller Stdin must be preserved verbatim; got %T", cmd.Stdin)
		}
		if !containsArg(cmd.Args, big) {
			t.Error("caller-provided Stdin: argv must still carry the script (caller owns stdin)")
		}
	})

	t.Run("non sh -c cmd unaffected by threshold", func(t *testing.T) {
		// Any other cmd shape — `bash -lc`, a direct binary call, or a
		// custom shell — must keep the argv path so we don't silently
		// reinterpret a tool's chosen invocation.
		big := strings.Repeat("z", maxInlineArgBytes+1)
		cmd := r.Command(context.Background(), []string{"my-tool", big}, sandbox.ExecOpts{})

		if cmd.Stdin != nil {
			t.Error("non-sh cmd: Stdin must stay nil (we only special-case sh -c)")
		}
		if !containsArg(cmd.Args, big) {
			t.Error("non-sh cmd: argv must carry the original args")
		}
	})
}

// shouldStreamScriptViaStdin is the single source of truth for the
// argv-vs-stdin decision; assert the predicate directly so a future
// refactor that changes the trigger threshold (or its shape gate)
// fails this test loudly rather than masquerading as a Run.Command
// behavioural shift.
func TestShouldStreamScriptViaStdinPredicate(t *testing.T) {
	big := strings.Repeat("a", maxInlineArgBytes+1)
	cases := []struct {
		name string
		cmd  []string
		opts sandbox.ExecOpts
		want string
	}{
		{"small sh -c", []string{"sh", "-c", "echo ok"}, sandbox.ExecOpts{}, ""},
		{"oversized sh -c", []string{"sh", "-c", big}, sandbox.ExecOpts{}, big},
		{"oversized sh -c with caller Stdin", []string{"sh", "-c", big}, sandbox.ExecOpts{Stdin: strings.NewReader("x")}, ""},
		{"small bash -c", []string{"bash", "-c", "echo ok"}, sandbox.ExecOpts{}, ""},
		{"oversized bash -c (rerouted)", []string{"bash", "-c", big}, sandbox.ExecOpts{}, big},
		{"oversized bash -c with caller Stdin", []string{"bash", "-c", big}, sandbox.ExecOpts{Stdin: strings.NewReader("x")}, ""},
		{"oversized direct argv", []string{"my-tool", big}, sandbox.ExecOpts{}, ""},
		{"empty", nil, sandbox.ExecOpts{}, ""},
		{"sh with extra args", []string{"sh", "-c", big, "name", "arg"}, sandbox.ExecOpts{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldStreamScriptViaStdin(tc.cmd, tc.opts)
			if got != tc.want {
				t.Errorf("shouldStreamScriptViaStdin = %d bytes, want %d", len(got), len(tc.want))
			}
		})
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
