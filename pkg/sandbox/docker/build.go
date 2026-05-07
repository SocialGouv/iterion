package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Compile-time check: the docker driver implements [sandbox.Builder]
// so the runtime can route `sandbox.build:` workflows through
// `docker buildx build` (BuildKit lives inside the Docker daemon).
var _ sandbox.Builder = (*Driver)(nil)

// buildImageRepo is the local image-cache repository all sandbox builds
// share. Tags discriminate runs (run-id). The image lives only in the
// host's Docker image cache (no registry push); the sibling container
// pulls from the cache when [Driver.Start] runs `docker run <ref>`.
const buildImageRepo = "iterion-sandbox-build"

// stderrTailBytes caps the buildx stderr we surface in the returned
// error. 4 KB captures the "ERROR: failed to solve" footer — the part
// the operator needs to see — without flooding the run log.
const stderrTailBytes = 4096

// runBuildx is the indirection unit tests overwrite to mock
// `docker buildx build`. The runtime always uses the default (real
// exec). Tests assert argv shape and surface controlled errors.
var runBuildx = func(ctx context.Context, rt Runtime, args []string, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, string(rt), args...)
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "LANG=C")
	return cmd.Run()
}

// Build invokes `docker buildx build --load` on the host's Docker
// daemon to materialize the spec's Dockerfile + context into a fresh
// image, tag it `iterion-sandbox-build:<run-id>`, and load it into the
// local image cache. The returned PreparedSpec carries the new image
// reference; [Driver.Start] then runs the sibling container against
// that ref via plain `docker run`.
//
// V2-6 design notes:
//   - No registry — the daemon's BuildKit caches layers locally and
//     the sibling container reads from the same Docker image store,
//     so push/pull is unnecessary. This is the lightweight equivalent
//     of the kubernetes BuildKit-in-cluster path (which V2-6 leaves
//     out: cloud workflows reference pre-built images via CI).
//   - Cleanup of the local tag is the operator's job for V1
//     (`docker image prune` against the iterion-sandbox-build repo).
//     A tag-by-content-hash strategy + per-run TTL is V2-7+.
//   - The Dockerfile + context paths are resolved relative to
//     info.WorkspacePath. The runner mounts the same path into the
//     sibling container after Build, so the on-disk layout the
//     workflow author saw locally is preserved.
func (d *Driver) Build(ctx context.Context, prepared sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.PreparedSpec, error) {
	p, ok := prepared.(*Prepared)
	if !ok {
		return nil, fmt.Errorf("docker: PreparedSpec from driver %q passed to docker.Build", prepared.DriverName())
	}
	if p.spec.Build == nil {
		return prepared, nil
	}
	if info.WorkspacePath == "" {
		return nil, fmt.Errorf("docker: build requires a workspace path; the engine must populate RunInfo.WorkspacePath before invoking the docker driver")
	}

	target := buildImageRef(info.RunID)

	dfRel := p.spec.Build.Dockerfile
	if dfRel == "" {
		dfRel = "Dockerfile"
	}
	ctxRel := p.spec.Build.Context
	if ctxRel == "" {
		ctxRel = filepath.Dir(dfRel)
	}
	absCtx := filepath.Join(info.WorkspacePath, ctxRel)
	absDf := filepath.Join(info.WorkspacePath, dfRel)

	args := []string{
		"buildx", "build",
		"-f", absDf,
		"-t", target,
		// --load tags the result into the host's Docker image store
		// rather than pushing to a registry. Sibling container pulls
		// from the same store at Start time — no push/pull round-trip.
		"--load",
	}
	// Sort build-arg keys so argv is stable across runs (helps cache
	// reuse + keeps unit-test argv assertions trivial).
	argKeys := make([]string, 0, len(p.spec.Build.Args))
	for k := range p.spec.Build.Args {
		argKeys = append(argKeys, k)
	}
	sort.Strings(argKeys)
	for _, k := range argKeys {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, p.spec.Build.Args[k]))
	}
	args = append(args, absCtx)

	var stderr bytes.Buffer
	if err := runBuildx(ctx, d.rt, args, &stderr); err != nil {
		return nil, fmt.Errorf("docker buildx: %w\nstderr (last %d bytes):\n%s", err, stderrTailBytes, tailString(stderr.Bytes(), stderrTailBytes))
	}

	newSpec := p.spec
	newSpec.Image = target
	newSpec.Build = nil
	return &Prepared{
		spec:      newSpec,
		workspace: p.workspace,
		runtime:   p.runtime,
	}, nil
}

// buildImageRef composes the local registry-less ref Build pushes the
// fresh layers under. Run IDs may carry underscores (iterion
// convention); Docker tags only allow [A-Za-z0-9_.-] up to 128 chars.
func buildImageRef(runID string) string {
	tag := strings.ReplaceAll(strings.ToLower(runID), "_", "-")
	if len(tag) > 128 {
		tag = tag[:128]
	}
	return fmt.Sprintf("%s:%s", buildImageRepo, tag)
}

// tailString returns the last n bytes of b as a string with a "...\n"
// prefix when truncated. Used to surface the most-relevant trailing
// portion of buildx stderr in error messages without dragging in the
// full layer-by-layer progress noise.
func tailString(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "...\n" + string(b[len(b)-n:])
}
