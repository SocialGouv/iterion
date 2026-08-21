package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// LegacyDefaultWorkspace is the historical devcontainer convention
// path. Kept exported for callers (tests + docs) that want to opt
// into the old "workspace lives at /workspace inside the container"
// behavior by setting [sandbox.Spec.WorkspaceFolder] = LegacyDefaultWorkspace.
//
// The new default — when WorkspaceFolder is empty — is to bind the
// host worktree at its own absolute path inside the container, so
// paths baked into bot prompts (via {{vars.workspace_dir}}, which
// resolves to the host workspace path) resolve identically in and
// out of the sandbox. This matches CLAUDE.md's "the sandbox bind-
// mounts the worktree at the host workspace's absolute path so
// Claude Code project keys match in/out container" guidance and
// fixes the 2026-05-20 dogfood bug where every dispatched bot
// burned its budget on Read errors because /home/.../studio/...
// (host) didn't exist inside the container — only /workspace did,
// but the prompt didn't say /workspace.
const LegacyDefaultWorkspace = "/workspace"

// nixVolumeNameFromID derives the persistent-/nix docker volume name from a
// container-image id (ADR-017 #1, opt-in). Keying on the image content id
// means a rebuilt image (different baked /nix) gets a FRESH, correctly-
// seeded volume rather than a stale one shadowing the new image's store.
// Returns "" when the id is too short to key on (caller then skips the
// volume and falls back to the ephemeral image /nix).
func nixVolumeNameFromID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) < 12 {
		return ""
	}
	return "iterion-nix-" + id[:12]
}

// nixStoreVolumeName resolves the image's content id via `image inspect` and
// returns its persistent-/nix volume name, or "" if it can't be resolved
// (the run then proceeds without the persistent store — non-fatal).
func nixStoreVolumeName(ctx context.Context, rt Runtime, image string) string {
	out, err := runtimeCmdContext(ctx, rt, "image", "inspect", "--format", "{{.Id}}", image).Output()
	if err != nil {
		return ""
	}
	return nixVolumeNameFromID(string(out))
}

// persistNixStore reports whether to mount the persistent /nix volume.
// Default ON; opt OUT with ITERION_SANDBOX_PERSIST_NIX in {0,false,off,no}.
func persistNixStore() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_SANDBOX_PERSIST_NIX"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// New returns a Docker driver bound to the given runtime, or an error
// when neither docker nor podman are on PATH. The constructor itself
// is cheap — no images are pulled, no containers created.
//
// The driver starts with a discard logger; callers (engine, doctor)
// install a real logger via [Driver.WithLogger] so sandbox events are
// interleaved with the rest of the run.
func New() (sandbox.Driver, error) {
	rt, err := Detect()
	if err != nil {
		return nil, &sandbox.ErrUnavailable{Driver: "docker", Reason: err.Error()}
	}
	return &Driver{rt: rt, logger: iterlog.New(iterlog.LevelInfo, io.Discard)}, nil
}

// Constructor is the [sandbox.DriverConstructor] hook for registration
// in [sandbox.Factory]. The factory falls back to the next candidate
// (podman, then noop) when this returns ErrUnavailable, so callers do
// not need to special-case Docker absence.
func Constructor() (sandbox.Driver, error) { return New() }

// Driver implements [sandbox.Driver] for the Docker / Podman runtimes.
type Driver struct {
	rt     Runtime
	logger *iterlog.Logger
}

// WithLogger returns a copy of the driver bound to a specific logger.
// The default logger discards output; engine integration installs the
// run's logger so sandbox events are interleaved with the rest of the
// run.
func (d *Driver) WithLogger(l *iterlog.Logger) *Driver {
	cp := *d
	if l != nil {
		cp.logger = l
	}
	return &cp
}

// Name returns the underlying runtime ("docker" or "podman").
func (d *Driver) Name() string { return string(d.rt) }

// Compile-time guard: Driver satisfies the optional ProxyConfigurer
// interface so the engine routes proxyAddressesForDriver to ProxyConfig
// instead of the loopback-only fallback (which is unreachable from a
// container via host.docker.internal).
var _ sandbox.ProxyConfigurer = (*Driver)(nil)

// ProxyConfig binds the network proxy on all interfaces and advertises
// the canonical "host.docker.internal" alias (which we wire via
// --add-host host-gateway in Start). The default bind 127.0.0.1 only
// listens on the host's loopback, but Docker's host-gateway resolves to
// the bridge IP (e.g. 172.17.0.1 on Linux), so the container's outbound
// CONNECTs land on a different interface and get ECONNREFUSED. Binding
// 0.0.0.0 covers both. The proxy enforces per-run bearer-token auth on
// every request, so this doesn't open it to unauthenticated host
// processes — only the sibling sandbox container that received the
// token in HTTPS_PROXY can use it.
func (d *Driver) ProxyConfig() (string, string, error) {
	return "0.0.0.0:0", "host.docker.internal", nil
}

// Capabilities advertises the features the driver supports today.
// Phase 1 implements image, mounts, env, remote user, and post-create.
// V2-6 adds Build (Dockerfile-at-run-start) via `docker buildx build`
// — BuildKit is already wired into the Docker daemon, no separate
// service needed.
func (d *Driver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		SupportsImage:          true,
		SupportsBuild:          true, // V2-6 — docker buildx build
		SupportsMounts:         true,
		SupportsHostBindMounts: true,  // shared host filesystem — bind-mounts the host iterion/rtk binary + worktree .git
		SupportsNetworkPolicy:  false, // Phase 3
		SupportsPostCreate:     true,
		SupportsRemoteUser:     true,
		SupportsTLSInspection:  true, // injects RunInfo.ProxyCACert via NODE_EXTRA_CA_CERTS
	}
}

// Prepare validates the spec, ensures the requested image is present
// on the host (pulling it if missing), and returns an opaque
// [PreparedSpec] consumed by [Driver.Start]. It is the ctx-aware
// "do all the slow IO before allocating a container" hook.
func (d *Driver) Prepare(ctx context.Context, spec sandbox.Spec) (sandbox.PreparedSpec, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Image == "" && spec.Build == nil {
		return nil, fmt.Errorf("docker: sandbox.image is required when mode=inline (or declare a build: block); use mode=auto with a .devcontainer/devcontainer.json to read it from there")
	}
	// workspace == "" is the sentinel "use the host absolute path
	// inside the container too" (the new default). Start() resolves
	// it from info.WorkspacePath once that's known. Explicit values
	// (e.g. "/workspace" for legacy compat, or "/repo" for custom
	// devcontainer setups) are honored as-is.
	workspace := spec.WorkspaceFolder

	// Pull is only meaningful when we have a pre-built image ref.
	// Build-driven specs materialize the image at Build() time and
	// always end up local — no pull required.
	if spec.Image != "" && !imageExists(ctx, d.rt, spec.Image) {
		d.logger.Info("sandbox: pulling image %s via %s", spec.Image, d.rt)
		if err := pullImage(ctx, d.rt, spec.Image); err != nil {
			return nil, err
		}
	}
	return &Prepared{
		spec:      spec,
		workspace: workspace,
		runtime:   d.rt,
	}, nil
}

// validateEnvVar rejects an env var whose name or value contains
// control characters that would break docker's --env arg parser.
// docker's parser tokenises by whitespace, so a value containing
// "\nDOCKER_HOST=tcp://attacker" would inject a second flag.
func validateEnvVar(k, v string) error {
	if strings.ContainsAny(k, "=\n\r\x00") {
		return fmt.Errorf("invalid env var name (contains '=', newline, or NUL): %q", k)
	}
	if strings.ContainsAny(v, "\n\r\x00") {
		return fmt.Errorf("env var value contains a newline, carriage return, or NUL — refusing to inject")
	}
	return nil
}

// Start creates and starts a long-lived container holding the run for
// its lifetime. The caller invokes [Run.Command] / [Run.Exec] for each
// delegate (claude_code, tool node) — startup cost is amortised across
// every invocation in the run.
// hostNetworkArgs returns the `docker run` args wiring the container to
// the host-side per-run services (egress proxy, board/ask-user MCP
// listeners).
//
// The `host.docker.internal → host-gateway` alias (+ NO_PROXY coverage)
// is DECOUPLED from the proxy's presence: iterion's per-run MCP
// listeners (board — C082 — and ask-user — ADR-082 Phase 3) advertise
// exactly that host, and under the default `network: open` policy no
// proxy runs — on Linux the alias then simply doesn't resolve and
// claude-code fails MCP tool registration at session start
// (AlwaysLoad servers fail loudly). The runtime signals a planned
// listener via [sandbox.RunInfo.HostGatewayAlias]; either that or a
// live proxy endpoint injects the alias. On Docker Desktop the alias
// is built-in; on Linux we have to opt in via --add-host.
//
// NO_PROXY rides along whenever the alias does: calls to
// host.docker.internal MUST go direct, never through an HTTP proxy
// (iterion's own, or one inherited from spec.Env) — the proxy runs on
// the host and cannot itself resolve host.docker.internal, so proxying
// an MCP call would fail the connect. It also lets inner localhost-only
// services (e.g. an inner devbox cache) bypass the proxy.
func hostNetworkArgs(info sandbox.RunInfo) []string {
	var args []string
	if info.ProxyEndpoint != "" {
		args = append(args, "--env", "HTTPS_PROXY="+info.ProxyEndpoint)
		args = append(args, "--env", "HTTP_PROXY="+info.ProxyEndpoint)
	}
	if info.ProxyEndpoint != "" || info.HostGatewayAlias {
		args = append(args, "--env", "NO_PROXY=localhost,127.0.0.1,host.docker.internal")
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	return args
}

func (d *Driver) Start(ctx context.Context, prepared sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	p, ok := prepared.(*Prepared)
	if !ok {
		return nil, fmt.Errorf("docker: PreparedSpec from driver %q passed to docker.Start", prepared.DriverName())
	}
	if info.WorkspacePath == "" {
		return nil, fmt.Errorf("docker: RunInfo.WorkspacePath is required (set to the host worktree path)")
	}
	absWorkspace, err := filepath.Abs(info.WorkspacePath)
	if err != nil {
		return nil, fmt.Errorf("docker: resolve workspace path: %w", err)
	}

	// Default-empty WorkspaceFolder means "bind at host path inside
	// container" — keeps workspace_dir baked into bot prompts (host
	// path) resolving identically in/out of the container. Explicit
	// overrides win (e.g. "/workspace" for legacy bots, "/repo" for
	// custom devcontainer schemas).
	inContainerWorkspace := p.workspace
	if inContainerWorkspace == "" {
		inContainerWorkspace = absWorkspace
	}

	containerName := containerNameFor(info.RunID)
	tempDirs := []string{}
	cleanupOnFailure := true
	defer func() {
		if cleanupOnFailure {
			cleanupTempDirs(tempDirs)
		}
	}()
	args := []string{"run", "--detach", "--rm",
		"--name", containerName,
		"--label", "iterion.io/managed=true",
		"--label", "iterion.io/run-id=" + info.RunID,
		"--workdir", inContainerWorkspace,
		"--mount", "type=bind,source=" + absWorkspace + ",target=" + inContainerWorkspace,
	}
	if info.FriendlyName != "" {
		args = append(args, "--label", "iterion.io/run-name="+info.FriendlyName)
	}
	if p.spec.User != "" {
		if err := validatePlainArg("docker --user", p.spec.User); err != nil {
			return nil, err
		}
		args = append(args, "--user", p.spec.User)
	}
	if err := validatePlainArg("docker image", p.spec.Image); err != nil {
		return nil, err
	}
	if err := validatePlainArg("docker --workdir", inContainerWorkspace); err != nil {
		return nil, err
	}
	for _, m := range p.spec.Mounts {
		if err := validateMount(m); err != nil {
			return nil, fmt.Errorf("docker driver: %w", err)
		}
		args = append(args, "--mount", m)
	}
	var secretLocs map[string]secretFileLoc
	if len(p.spec.SecretFiles) > 0 {
		var err error
		args, secretLocs, err = appendSecretFileMountArgs(args, p.spec.SecretFiles, &tempDirs)
		if err != nil {
			return nil, err
		}
	}
	// Persistent Nix store (ADR-017 #1): a named docker volume at /nix,
	// seeded from the image on first mount and reused across runs so
	// devbox-provisioned toolchains (the bot's devbox.json and, Tier-2, the
	// project's) resolve WARM instead of re-fetching every run. Default ON
	// (validated: warm-reuse cuts the per-run cold devbox install — the
	// dogfood remediation builds ran cold without it) — opt OUT with
	// ITERION_SANDBOX_PERSIST_NIX=0/false/off. Keyed on the image id so a
	// rebuilt image gets a fresh, correctly-seeded volume (a stale volume
	// would shadow the new image's /nix). Seeded store is consistent:
	// `nix-store --verify` + devbox both succeed on it.
	if persistNixStore() && p.spec.Image != "" {
		if vol := nixStoreVolumeName(ctx, d.rt, p.spec.Image); vol != "" {
			args = append(args, "--mount", "type=volume,source="+vol+",target=/nix")
		}
	}
	// Tmpfs entries (host_state's writable HOME). docker treats the value
	// after --tmpfs as a single arg ("/path:opts"), so the same
	// shell-injection guard as --mount/--user applies.
	for _, t := range p.spec.Tmpfs {
		if err := validatePlainArg("docker --tmpfs", t); err != nil {
			return nil, fmt.Errorf("docker driver: %w", err)
		}
		args = append(args, "--tmpfs", t)
	}
	// Iterate Env in sorted key order so validation errors and the
	// resulting `docker run` argv are deterministic — Go's map
	// iteration randomises order, which produced flaky tests when two
	// env vars were both invalid (the error message would refer to
	// whichever happened to be visited first).
	envKeys := make([]string, 0, len(p.spec.Env))
	for k := range p.spec.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		v := p.spec.Env[k]
		if err := validateEnvVar(k, v); err != nil {
			return nil, fmt.Errorf("docker driver: env %s: %w", k, err)
		}
		args = append(args, "--env", k+"="+v)
	}
	args = append(args, hostNetworkArgs(info)...)

	// Egress TLS-inspection CA (Layer 2 secret substitution): when the
	// proxy runs in inspection mode it mints leaves the in-container
	// clients must trust. Bind-mount the per-run CA cert and point every
	// common client's CA-bundle env var at it. This works WITHOUT root or
	// update-ca-certificates (the slim image runs as uid 1000): in
	// inspection mode the proxy terminates ALL egress TLS, so the only
	// certificate any in-container client ever sees is our leaf — pointing
	// the bundle at our CA alone is correct (NO_PROXY=localhost is the
	// only exception, and localhost rarely speaks TLS in a sandbox).
	// NODE_EXTRA_CA_CERTS is additive; the rest replace, which is fine
	// here. See docs/secrets.md.
	if len(info.ProxyCACert) > 0 {
		caDir, err := os.MkdirTemp("", "iterion-egress-ca-")
		if err != nil {
			return nil, fmt.Errorf("docker driver: egress CA dir: %w", err)
		}
		tempDirs = append(tempDirs, caDir)
		caHostPath := filepath.Join(caDir, "egress-ca.pem")
		if err := os.WriteFile(caHostPath, info.ProxyCACert, 0o644); err != nil {
			return nil, fmt.Errorf("docker driver: write egress CA: %w", err)
		}
		const caContainerPath = "/run/iterion/egress-ca.pem"
		args = append(args, "-v", caHostPath+":"+caContainerPath+":ro")
		for _, caEnv := range []string{
			"NODE_EXTRA_CA_CERTS", // Node / Claude Code (additive)
			"SSL_CERT_FILE",       // OpenSSL: python ssl, git-over-openssl, ruby, …
			"CURL_CA_BUNDLE",      // curl
			"GIT_SSL_CAINFO",      // git
			"REQUESTS_CA_BUNDLE",  // python requests
			"NIX_SSL_CERT_FILE",   // nix : sa propre variable, il n'honore pas SSL_CERT_FILE
		} {
			args = append(args, "--env", caEnv+"="+caContainerPath)
		}
		// JVMs read none of those — their trust is a keystore-typed file.
		// Same store as the kubernetes driver ships; see
		// netproxy.JavaTrustStorePKCS12.
		trustStore, _, tsErr := netproxy.JavaTrustStorePKCS12(info.ProxyCACert)
		if tsErr != nil {
			return nil, fmt.Errorf("docker driver: java truststore: %w", tsErr)
		}
		tsHostPath := filepath.Join(caDir, "egress-truststore.p12")
		if err := os.WriteFile(tsHostPath, trustStore, 0o644); err != nil {
			return nil, fmt.Errorf("docker driver: write java truststore: %w", err)
		}
		const tsContainerPath = "/run/iterion/egress-truststore.p12"
		args = append(args, "-v", tsHostPath+":"+tsContainerPath+":ro",
			"--env", "ITERION_JAVA_TRUSTSTORE="+tsContainerPath,
			"--env", "ITERION_JAVA_TRUSTSTORE_PASSWORD="+netproxy.JavaTrustStorePassword,
			"--env", "JAVA_TOOL_OPTIONS=-Djavax.net.ssl.trustStore="+tsContainerPath+
				" -Djavax.net.ssl.trustStorePassword="+netproxy.JavaTrustStorePassword)
	}

	// PID 1 is `sleep infinity` so the container stays alive while
	// the run streams in N `docker exec` calls. We deliberately do
	// not use the image's CMD/ENTRYPOINT — that would shadow our
	// "container as a long-lived ssh-like target" model.
	args = append(args, p.spec.Image, "sleep", "infinity")

	out, err := runtimeCmdContext(ctx, d.rt, args...).CombinedOutput()
	if err != nil && isContainerNameConflict(out) {
		// A leftover container with the same name (one per run-id) is
		// a recoverable state, not a hard failure. The two common
		// causes are both transient:
		//   - prior daemon was SIGTERM-killed while a sandbox was
		//     up; --rm cleanup raced with shutdown and lost.
		//   - prior run failed without reaching Cleanup (e.g. a panic
		//     between Start return and the engine's defer).
		// Force-remove the stale container and retry once. If the
		// retry also fails, surface the original error chain so the
		// operator sees the underlying cause.
		d.logger.Warn("sandbox: container %q already exists from a prior run — force-removing and retrying", containerName)
		if rmErr := forceRemoveContainer(ctx, d.rt, containerName); rmErr != nil {
			return nil, fmt.Errorf("docker: run: %w\noutput: %s\n(also tried force-remove of stale container: %v)", err, string(out), rmErr)
		}
		out, err = runtimeCmdContext(ctx, d.rt, args...).CombinedOutput()
	}
	if err != nil {
		return nil, fmt.Errorf("docker: run: %w\noutput: %s", err, string(out))
	}
	containerID := strings.TrimSpace(string(out))

	r := &Run{
		driver:               d,
		containerID:          containerID,
		containerName:        containerName,
		prepared:             p,
		info:                 info,
		inContainerWorkspace: inContainerWorkspace,
		tempDirs:             tempDirs,
		secretFiles:          secretLocs,
	}

	if p.spec.PostCreate != "" {
		if err := r.runPostCreate(ctx, p.spec.PostCreate); err != nil {
			// Best-effort cleanup on PostCreate failure. PostCreate
			// often fails *because* ctx is done (timeout, caller
			// cancel) — passing the same ctx to Cleanup would leave it
			// with no budget to actually remove the container we just
			// created. Strip cancellation so Cleanup's internal 10s
			// timeout governs.
			_ = r.Cleanup(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("docker: postCreate failed: %w", err)
		}
	}

	d.logger.Info("sandbox: container %s started (image=%s, workspace=%s)", containerShortID(containerID), p.spec.Image, inContainerWorkspace)
	cleanupOnFailure = false
	return r, nil
}

// Prepared is the docker driver's [sandbox.PreparedSpec] implementation.
type Prepared struct {
	spec      sandbox.Spec
	workspace string
	runtime   Runtime
}

// DriverName implements [sandbox.PreparedSpec].
func (p *Prepared) DriverName() string { return string(p.runtime) }

// Spec returns the spec the prepared was built from. Useful for tests
// and engine-side diagnostics.
func (p *Prepared) Spec() sandbox.Spec { return p.spec }

// Run is the live docker driver sandbox handle.
//
// All [Run] methods are safe to call concurrently — `docker exec` is
// itself concurrent-safe and the cleanup mutex serialises lifecycle
// transitions.
type Run struct {
	driver        *Driver
	containerID   string // full SHA returned by `docker run -d`
	containerName string // human-readable label, also stable across runtime restarts
	prepared      *Prepared
	info          sandbox.RunInfo

	// inContainerWorkspace is the workdir path inside the container —
	// either spec.WorkspaceFolder (legacy explicit override) or
	// info.WorkspacePath (the new "bind at host absolute path"
	// default). Used as the default --workdir for `docker exec`
	// when the caller doesn't override per-call.
	inContainerWorkspace string

	// tempDirs holds per-run host temp dirs used for mounted secret files
	// and other ephemeral driver files. They are removed in Cleanup.
	tempDirs []string

	// secretFiles maps each mounted file secret's name to the host path
	// backing its bind-mount, so RefreshSecretFile can rewrite it mid-run
	// (a rotated short-lived token). Nil when the run has no file secrets.
	secretFiles map[string]secretFileLoc

	mu      sync.Mutex
	stopped bool
	cleaned bool
}

// Driver returns the runtime name — "docker" or "podman".
func (r *Run) Driver() string { return string(r.driver.rt) }

// RefreshSecretFile rewrites the host file backing a mounted file
// secret's bind-mount so a rotated short-lived token propagates into the
// running container (docker bind-mounts follow the host inode, so a
// subsequent in-container `cat` reads the new value). Implements
// [sandbox.SecretFileRefresher].
//
// The value is never logged. A directory-mounted secret (the default
// /run/iterion/secrets case) is replaced atomically via temp-write +
// rename inside the mounted dir; a single-file bind mount is rewritten
// in place (rename would swap the inode the container has pinned).
func (r *Run) RefreshSecretFile(_ context.Context, name string, value []byte) error {
	if len(value) == 0 {
		return fmt.Errorf("docker driver: refresh file secret %s: empty value", name)
	}
	loc, ok := r.secretFiles[name]
	if !ok {
		return fmt.Errorf("docker driver: refresh: no mounted file secret %q", name)
	}
	if loc.dirMounted {
		// Atomic replace so a concurrent in-container `cat` never sees a
		// torn write; the temp lives in the same mounted dir so rename is
		// a same-filesystem move.
		tmp := loc.hostPath + ".tmp"
		if err := os.WriteFile(tmp, value, 0o400); err != nil {
			return fmt.Errorf("docker driver: refresh file secret %s: write temp: %w", name, err)
		}
		if err := os.Rename(tmp, loc.hostPath); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("docker driver: refresh file secret %s: rename: %w", name, err)
		}
		return nil
	}
	// Single-file bind mount: the container has the inode pinned, so we
	// must rewrite in place (a rename would swap the inode). The file was
	// created 0o400, and the owner cannot open a read-only file for
	// writing, so widen to 0o600 for the write then restore 0o400.
	//
	// Do NOT use os.WriteFile: its O_TRUNC zeroes the file BEFORE writing,
	// so a concurrent in-container `cat` in that window reads an empty
	// token. Instead open without O_TRUNC, write the new value at offset
	// 0 (one write(2) syscall for a small token — a reader sees old-or-new
	// bytes, not empty), then Truncate to the new length to drop any
	// trailing bytes when the value shrank.
	if err := os.Chmod(loc.hostPath, 0o600); err != nil {
		return fmt.Errorf("docker driver: refresh file secret %s: chmod: %w", name, err)
	}
	f, err := os.OpenFile(loc.hostPath, os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("docker driver: refresh file secret %s: open: %w", name, err)
	}
	if _, err := f.WriteAt(value, 0); err != nil {
		_ = f.Close()
		return fmt.Errorf("docker driver: refresh file secret %s: write: %w", name, err)
	}
	if err := f.Truncate(int64(len(value))); err != nil {
		_ = f.Close()
		return fmt.Errorf("docker driver: refresh file secret %s: truncate: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("docker driver: refresh file secret %s: close: %w", name, err)
	}
	if err := os.Chmod(loc.hostPath, 0o400); err != nil {
		return fmt.Errorf("docker driver: refresh file secret %s: restore perm: %w", name, err)
	}
	return nil
}

// maxInlineArgBytes caps the size of a single argv element passed to
// `docker exec`. Linux's ARG_MAX is typically 128 KiB–2 MiB for the
// total argv+env block; a single element well below that bound is
// always safe, while a multi-hundred-KB shell script interpolated into
// `docker exec … sh -c <script>` (e.g. Seki's majority_verdict tool
// node concatenating three large voter verdicts) overflows the kernel's
// E2BIG check and the docker fork fails with
// "fork/exec /usr/bin/docker: argument list too long". 100 KB leaves
// headroom for the rest of the argv (flags, env, container id) on the
// smallest realistic ARG_MAX and is far above any normal tool snippet.
const maxInlineArgBytes = 100_000

// shouldStreamScriptViaStdin reports whether the given cmd is the
// `sh -c <script>` shape and should be routed through stdin instead of
// argv to avoid ARG_MAX (E2BIG) overflow. Returns the script when so;
// the empty string otherwise.
//
// Conditions: cmd is exactly `["sh","-c", script]` or `["bash","-c",
// script]`, no stdin is already attached (so we don't clobber a
// caller-provided reader), and the script exceeds [maxInlineArgBytes].
// Both shells are matched because internal callers emit both: the
// tool-node executor runs recipes via `bash -c`, while RunPostCreate
// and the claw bash builtin use `sh -c`. Any other shell or argv shape
// falls through to the standard argv path so behavior is byte-for-byte
// unchanged. The reroute (see Command) re-uses cmd[0] for the `-s`
// invocation, so bash recipes keep bash semantics.
func shouldStreamScriptViaStdin(cmd []string, opts sandbox.ExecOpts) string {
	if len(cmd) != 3 || (cmd[0] != "sh" && cmd[0] != "bash") || cmd[1] != "-c" {
		return ""
	}
	if opts.Stdin != nil {
		return ""
	}
	if len(cmd[2]) <= maxInlineArgBytes {
		return ""
	}
	return cmd[2]
}

// Command returns an *exec.Cmd that, when started, runs cmd inside the
// container via `docker exec`. Stdin/Stdout/Stderr on the returned cmd
// are forwarded transparently to the in-container process by docker
// itself, so callers can drive streaming I/O exactly as they would for
// a host subprocess.
//
// Cwd defaults to the workspace folder (the bind-mount target);
// [ExecOpts.WorkDir] overrides per-call.
//
// Env vars are passed via `docker exec --env KEY=VAL` so the inner
// process sees them — setting them on the returned [exec.Cmd.Env]
// would only affect the host-side `docker exec` driver process, not
// the inner program.
//
// When cmd is `["sh","-c", script]` and the script is larger than
// [maxInlineArgBytes], the script is streamed through stdin via
// `sh -s` instead of being passed as a single argv element. This
// avoids the kernel's ARG_MAX (E2BIG) limit on the host `docker exec`
// fork — see [shouldStreamScriptViaStdin] for the trigger predicate.
func (r *Run) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	if len(cmd) == 0 {
		// Mirror noop's degenerate case: return a cmd that errors on
		// Start.
		return exec.CommandContext(ctx, "")
	}

	stdinScript := shouldStreamScriptViaStdin(cmd, opts)

	args := []string{"exec"}
	if opts.Stdin != nil || opts.KeepStdinOpen || stdinScript != "" {
		args = append(args, "--interactive")
	}
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = r.inContainerWorkspace
	}
	args = append(args, "--workdir", workDir)
	for k, v := range opts.Env {
		// Validate every per-call env var. Without this a newline (or
		// other control char) embedded in a value — e.g. coming from
		// an artifact field interpolated into a tool command — could
		// split the argument and inject arbitrary `docker exec` flags.
		// Driver.Start applies the same check at container-creation
		// time; the per-exec path here needs it too.
		if err := validateEnvVar(k, v); err != nil {
			r.driver.logger.Warn("docker exec: skipping invalid env %s: %v", k, err)
			continue
		}
		args = append(args, "--env", k+"="+v)
	}
	if r.prepared.spec.User != "" {
		args = append(args, "--user", r.prepared.spec.User)
	}
	args = append(args, r.containerID)
	if stdinScript != "" {
		// `<shell> -s` reads the script from stdin instead of taking it
		// as an argv element. Works identically on dash, bash, busybox
		// sh. cmd[0] is "sh" or "bash" (guaranteed by
		// shouldStreamScriptViaStdin), so bash recipes keep bash. The
		// script never enters the docker argv, sidestepping E2BIG.
		args = append(args, cmd[0], "-s")
	} else {
		args = append(args, cmd...)
	}

	c := exec.CommandContext(ctx, string(r.driver.rt), args...)
	switch {
	case opts.Stdin != nil:
		c.Stdin = opts.Stdin
	case stdinScript != "":
		c.Stdin = strings.NewReader(stdinScript)
	}
	proc.DetachProcessGroup(c)
	return c
}

// Exec is a buffered convenience wrapper around [Run.Command]. See
// [sandbox.Run.Exec] for semantics.
func (r *Run) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if len(cmd) == 0 {
		return sandbox.ExecResult{}, fmt.Errorf("docker.Exec: empty cmd")
	}
	return sandbox.ExecCmd(r.Command(ctx, cmd, opts), opts)
}

// stop sends SIGTERM via `docker stop` with a short grace period.
// Idempotent. Only invoked by Cleanup; no callers outside this file.
func (r *Run) stop(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	r.mu.Unlock()

	// Give the container 5s to exit gracefully, then docker SIGKILLs.
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := runtimeCmdContext(stopCtx, r.driver.rt, "stop", "--time", "5", r.containerID).CombinedOutput()
	if err != nil {
		// "no such container" means it already exited — treat as success.
		if strings.Contains(string(out), "No such container") || strings.Contains(string(out), "no such container") {
			return nil
		}
		return fmt.Errorf("docker: stop %s: %w\noutput: %s", containerShortID(r.containerID), err, string(out))
	}
	return nil
}

// Cleanup ensures the container is gone. Containers were created with
// `--rm` so a graceful Stop already removes them; Cleanup is the
// fallback for the failure-mode where the container is alive but
// orphaned (engine crash mid-run, etc.).
func (r *Run) Cleanup(ctx context.Context) error {
	r.mu.Lock()
	if r.cleaned {
		r.mu.Unlock()
		return nil
	}
	r.cleaned = true
	r.mu.Unlock()
	defer cleanupTempDirs(r.tempDirs)

	// Best-effort stop first to give --rm a chance to fire normally.
	_ = r.stop(ctx)

	// Then force-remove any lingering container with our run-id label,
	// independent of the captured containerID (covers the case where
	// the container died but didn't get auto-removed). Use the
	// caller's ctx as parent so a cancelled run aborts cleanup quickly
	// (10s is a cap on the cleanup itself, not an extension past the
	// caller's deadline). Falling back to context.Background when ctx
	// is nil keeps the prior behaviour for niche callers.
	parent := ctx
	if parent == nil {
		parent = context.Background()
	}
	cleanupCtx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	out, err := runtimeCmdContext(cleanupCtx, r.driver.rt, "rm", "--force", r.containerID).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No such container") || strings.Contains(string(out), "no such container") {
			return nil
		}
		// Don't propagate cleanup errors to the run — they are usually
		// "container already gone" from --rm racing with us. Log at
		// debug level for forensics.
		r.driver.logger.Debug("sandbox: cleanup of %s reported: %v (output: %s)", containerShortID(r.containerID), err, string(out))
	}
	return nil
}

func writeSecretFileTemp(sf sandbox.SecretFileMount) (hostPath, dir string, err error) {
	if len(sf.Value) == 0 {
		return "", "", fmt.Errorf("docker driver: file secret %s has empty payload", sf.Name)
	}
	if err := validateSecretFileMount(sf.MountPath); err != nil {
		return "", "", fmt.Errorf("docker driver: file secret %s: %w", sf.Name, err)
	}
	dir, err = os.MkdirTemp("", "iterion-secret-file-")
	if err != nil {
		return "", "", fmt.Errorf("docker driver: file secret temp dir: %w", err)
	}
	hostPath = filepath.Join(dir, "payload")
	if err := os.WriteFile(hostPath, sf.Value, 0o400); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("docker driver: write file secret %s: %w", sf.Name, err)
	}
	return hostPath, dir, nil
}

// secretFileLoc records, per file secret, the host path backing its
// bind-mount so [Run.RefreshSecretFile] can rewrite it mid-run.
type secretFileLoc struct {
	// hostPath is the bind-mount SOURCE on the host runner.
	hostPath string
	// dirMounted is true when the secret lives inside a bind-mounted
	// DIRECTORY (the default `/run/iterion/secrets` case): a refresh can
	// then atomically temp-write+rename within that dir. False when the
	// secret is a single-FILE bind mount — rename would swap the inode
	// and the container would keep the old one, so the refresh must
	// rewrite the file in place.
	dirMounted bool
}

func appendSecretFileMountArgs(args []string, files []sandbox.SecretFileMount, tempDirs *[]string) ([]string, map[string]secretFileLoc, error) {
	defaultDirFiles := make([]sandbox.SecretFileMount, 0, len(files))
	directFiles := make([]sandbox.SecretFileMount, 0, len(files))
	for _, sf := range files {
		if len(sf.Value) == 0 {
			return nil, nil, fmt.Errorf("docker driver: file secret %s has empty payload", sf.Name)
		}
		if err := validateSecretFileMount(sf.MountPath); err != nil {
			return nil, nil, fmt.Errorf("docker driver: file secret %s: %w", sf.Name, err)
		}
		if _, ok := secrets.RelativeToSecretFilesMountDir(sf.MountPath); ok {
			defaultDirFiles = append(defaultDirFiles, sf)
		} else {
			directFiles = append(directFiles, sf)
		}
	}

	locs := make(map[string]secretFileLoc, len(files))
	if len(defaultDirFiles) > 0 {
		hostDir, perFile, err := writeSecretFilesDirTemp(defaultDirFiles)
		if err != nil {
			return nil, nil, err
		}
		*tempDirs = append(*tempDirs, hostDir)
		args = append(args, "--mount", "type=bind,source="+hostDir+",target="+secrets.SecretFilesMountDir+",readonly")
		for name, p := range perFile {
			locs[name] = secretFileLoc{hostPath: p, dirMounted: true}
		}
	}

	for _, sf := range directFiles {
		hostPath, dir, err := writeSecretFileTemp(sf)
		if err != nil {
			return nil, nil, err
		}
		*tempDirs = append(*tempDirs, dir)
		args = append(args, "--mount", "type=bind,source="+hostPath+",target="+sf.MountPath+",readonly")
		locs[sf.Name] = secretFileLoc{hostPath: hostPath, dirMounted: false}
	}
	return args, locs, nil
}

func writeSecretFilesDirTemp(files []sandbox.SecretFileMount) (dir string, perFile map[string]string, err error) {
	dir, err = os.MkdirTemp("", "iterion-secret-files-")
	if err != nil {
		return "", nil, fmt.Errorf("docker driver: file secrets temp dir: %w", err)
	}
	seen := map[string]string{}
	perFile = make(map[string]string, len(files))
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	for _, sf := range files {
		rel, ok := secrets.RelativeToSecretFilesMountDir(sf.MountPath)
		if !ok {
			return "", nil, fmt.Errorf("docker driver: file secret %s mount_path %q is not under %s", sf.Name, sf.MountPath, secrets.SecretFilesMountDir)
		}
		if prev := seen[rel]; prev != "" {
			return "", nil, fmt.Errorf("docker driver: file secrets %s and %s both target %s/%s", prev, sf.Name, secrets.SecretFilesMountDir, rel)
		}
		seen[rel] = sf.Name
		hostPath := filepath.Join(dir, filepath.FromSlash(rel))
		cleanHostPath := filepath.Clean(hostPath)
		if cleanHostPath == dir || !strings.HasPrefix(cleanHostPath, dir+string(filepath.Separator)) {
			return "", nil, fmt.Errorf("docker driver: file secret %s mount_path escapes temp dir", sf.Name)
		}
		if err := os.MkdirAll(filepath.Dir(cleanHostPath), 0o700); err != nil {
			return "", nil, fmt.Errorf("docker driver: create file secret dir %s: %w", sf.Name, err)
		}
		if err := os.WriteFile(cleanHostPath, sf.Value, 0o400); err != nil {
			return "", nil, fmt.Errorf("docker driver: write file secret %s: %w", sf.Name, err)
		}
		perFile[sf.Name] = cleanHostPath
	}
	return dir, perFile, nil
}

func validateSecretFileMount(mountPath string) error {
	if mountPath == "" || !strings.HasPrefix(mountPath, "/") {
		return fmt.Errorf("mount_path %q must be absolute", mountPath)
	}
	if strings.ContainsAny(mountPath, "\n\r\x00,") {
		return fmt.Errorf("mount_path contains a control character or comma")
	}
	if path.Clean(mountPath) != mountPath || mountPath == "/" {
		return fmt.Errorf("mount_path %q must be a clean absolute file path", mountPath)
	}
	return nil
}

func cleanupTempDirs(dirs []string) {
	for _, dir := range dirs {
		_ = os.RemoveAll(dir)
	}
}

// runPostCreate runs the spec's post-create command inside the
// freshly started container. Stdout/stderr are streamed to the
// driver's logger so users see install progress.
func (r *Run) runPostCreate(ctx context.Context, snippet string) error {
	r.driver.logger.Info("sandbox: running postCreateCommand")
	return sandbox.RunPostCreate(ctx, r, snippet, r.driver.logger)
}

// containerNameFor maps a run ID to a deterministic container name.
// Stable across iterion server restarts so a `docker ps` listing keeps
// the same identifiers visible to the operator.
func containerNameFor(runID string) string {
	// Run IDs are already filesystem-safe — UUIDv7 strings for new
	// runs, "run_<ms>" for legacy ones. Length cap: docker truncates
	// names beyond 64 chars.
	name := "iterion-" + runID
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// containerShortID truncates a SHA to its first 12 chars, matching the
// convention `docker ps` uses for compact display.
func containerShortID(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// isContainerNameConflict matches the docker-daemon error message that
// `docker run --name X` produces when a container with name X already
// exists (running, stopped, or paused). The message is stable enough
// across docker / podman / runc versions to substring-match without
// over-fitting:
//
//	docker: Error response from daemon: Conflict. The container name
//	"/iterion-run_xxx" is already in use by container "<sha>". You have
//	to remove (or rename) that container to be able to reuse that name.
//
// `podman` produces the slightly different "the container name … is
// already in use by …" — covering both with a shared substring.
func isContainerNameConflict(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "is already in use")
}

// forceRemoveContainer issues `docker rm -f <name>` to evict a leftover
// container that's blocking a name reuse. Best-effort: returns the
// underlying error so the caller can decide whether to surface a
// combined diagnostic. Idempotent — removing a missing container is
// not treated as a hard error by the caller path.
func forceRemoveContainer(ctx context.Context, rt Runtime, name string) error {
	out, err := runtimeCmdContext(ctx, rt, "rm", "-f", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("rm -f %s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Compile-time interface checks.
var (
	_ sandbox.Driver       = (*Driver)(nil)
	_ sandbox.Run          = (*Run)(nil)
	_ sandbox.PreparedSpec = (*Prepared)(nil)
)
