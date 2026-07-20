package docker

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// runDockerProbe is the indirection unit tests overwrite to mock the
// docker/podman CLI invocations the strict-doctor probes issue.
// Production reuses [runtimeCmdContext] — same LC_ALL=C locale scrub
// (stable English stderr substrings) AND the detached process group, so
// a SIGTERM to the iterion/studio PGID (e.g. a watchexec rebuild while a
// preflight probe is in flight) doesn't kill the probe and surface a
// spurious "daemon unreachable". The caller's extraEnv (e.g.
// DOCKER_CLI_EXPERIMENTAL=enabled for `docker manifest inspect`) is
// layered on top.
var runDockerProbe = func(ctx context.Context, rt Runtime, extraEnv []string, args ...string) ([]byte, error) {
	cmd := runtimeCmdContext(ctx, rt, args...)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd.CombinedOutput()
}

// PingDaemon reports whether the container runtime's daemon is live and
// reachable — the strict-doctor "Docker daemon liveness" check.
//
// It runs `<runtime> version --format {{.Server.Version}}`: querying the
// SERVER version (not the client) forces an actual round-trip to the
// daemon, so a stopped Docker Desktop / `dockerd` surfaces here in ~1s
// instead of 30s into a run with a cryptic "Cannot connect to the Docker
// daemon at unix:///var/run/docker.sock". Returns the server version
// string on success.
func PingDaemon(ctx context.Context) (serverVersion string, err error) {
	rt, err := Detect()
	if err != nil {
		return "", err
	}
	out, runErr := runDockerProbe(ctx, rt, nil, "version", "--format", "{{.Server.Version}}")
	trimmed := strings.TrimSpace(string(out))
	if runErr != nil {
		return "", fmt.Errorf("%s daemon unreachable: %v\noutput: %s", rt, runErr, trimmed)
	}
	if trimmed == "" {
		return "", fmt.Errorf("%s daemon returned an empty server version (is the daemon running?)", rt)
	}
	return trimmed, nil
}

// ImageResolveError categorises why an image reference could not be
// resolved against its registry, so the doctor can decide whether the
// failure is fatal (the tag genuinely does not exist → fail the run
// pre-flight) or merely advisory (auth / network — we cannot prove
// resolvability offline, but the run may still succeed with daemon-side
// credentials → warn only).
type ImageResolveError struct {
	Ref       string
	Transient bool // true = auth/network (warn); false = not-found (fail)
	Output    string
	Err       error
}

// Error implements error.
func (e *ImageResolveError) Error() string {
	kind := "not found in its registry"
	if e.Transient {
		kind = "could not be verified (registry auth or network)"
	}
	msg := fmt.Sprintf("image %q %s: %v", e.Ref, kind, e.Err)
	if e.Output != "" {
		msg += "\noutput: " + e.Output
	}
	return msg
}

// ResolveImageRef checks that an image reference resolves WITHOUT pulling
// its layers — the strict-doctor "image pull-ability" check. Order:
//
//  1. If the image is already in the local cache it will run; return nil.
//  2. Otherwise `<runtime> manifest inspect <ref>` reads just the
//     registry manifest (no blob download). DOCKER_CLI_EXPERIMENTAL is
//     forced on for older docker CLIs that gate `manifest` behind it.
//
// A genuine "manifest unknown / not found" is returned as a
// non-transient *[ImageResolveError]; auth/network errors are returned
// transient so the doctor warns rather than fails.
func ResolveImageRef(ctx context.Context, ref string) error {
	rt, err := Detect()
	if err != nil {
		return err
	}
	if imageExists(ctx, rt, ref) {
		return nil
	}
	out, runErr := runDockerProbe(ctx, rt, []string{"DOCKER_CLI_EXPERIMENTAL=enabled"}, "manifest", "inspect", ref)
	return classifyImageResolve(ref, out, runErr)
}

// classifyImageResolve turns a `manifest inspect` result into either nil
// (resolved) or an *[ImageResolveError]. Pure (no exec) so the
// classification heuristic is unit-tested in isolation from the CLI
// round-trip.
//
// Auth/permission/network failures are checked FIRST and classed as
// transient (warn): the daemon may hold credentials this offline probe
// doesn't, and registries phrase those failures with the same "does not
// exist" / "not found" wording a genuine missing tag uses (Docker Hub:
// "pull access denied ... repository does not exist or may require
// 'docker login': denied"). Only a not-found signal with NO auth/network
// marker is treated as fatal; anything unrecognised defaults to the safe
// (transient) bucket so a quirky registry message never hard-fails a run
// that would actually pull.
func classifyImageResolve(ref string, out []byte, runErr error) error {
	if runErr == nil {
		return nil
	}
	lower := strings.ToLower(string(out))
	var transient bool
	switch {
	case containsAny(lower,
		"denied", "unauthorized", "authentication", "forbidden", "login",
		"401", "403", "timeout", "timed out", "no such host", "dial tcp",
		"connection refused", "tls", "i/o timeout", "temporary failure"):
		transient = true // auth/network — cannot disprove the image offline
	case containsAny(lower, "manifest unknown", "no such manifest", "not found"):
		transient = false // the tag/manifest is genuinely absent
	default:
		transient = true // unknown failure — default to the warn bucket
	}
	return &ImageResolveError{
		Ref:       ref,
		Transient: transient,
		Output:    strings.TrimSpace(string(out)),
		Err:       runErr,
	}
}

// containsAny reports whether s contains any of subs.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ValidateSpecMounts runs the docker driver's spec-arg safety validation
// over a resolved spec WITHOUT starting a container — the strict-doctor
// safety check. It mirrors EVERY guard [Driver.Start] applies (sensitive
// host paths like /var/run/docker.sock, control characters, leading-dash
// flag injection on image/user/workdir, and env-var name/value injection)
// so the doctor catches a dangerous spec up-front instead of 30s into the
// run. Pure: no exec, no daemon contact.
func ValidateSpecMounts(spec sandbox.Spec) error {
	if err := validatePlainArg("docker image", spec.Image); err != nil {
		return err
	}
	if err := validatePlainArg("docker --user", spec.User); err != nil {
		return err
	}
	if err := validatePlainArg("docker --workdir", spec.WorkspaceFolder); err != nil {
		return err
	}
	for _, m := range spec.Mounts {
		if err := validateMount(m); err != nil {
			return err
		}
	}
	// Env vars are injected via `docker run --env K=V`; the same
	// newline/NUL/`=`-in-key checks Driver.Start applies must hold here,
	// else a malformed value (e.g. from a devcontainer containerEnv)
	// passes the doctor green but fails at container start. Sorted so the
	// reported offender is deterministic across map iterations.
	envKeys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		if err := validateEnvVar(k, spec.Env[k]); err != nil {
			return fmt.Errorf("env %s: %w", k, err)
		}
	}
	return nil
}
