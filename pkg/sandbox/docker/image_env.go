package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Compile-time guard: the docker driver screens the ENV its images bake in
// (#2317). The kubernetes driver and the noop passthrough deliberately do
// not — see [sandbox.ImageEnvScreener].
var _ sandbox.ImageEnvScreener = (*Driver)(nil)

// ImageConfigEnv implements [sandbox.ImageEnvScreener]: the `ENV` entries of
// the image the prepared spec will start, read with `image inspect`.
//
// `image inspect` reads the LOCAL store only (`No such image` on a tag that
// was never pulled), which is why the engine calls this after Prepare — the
// pull has happened by then. An image that is still absent is an error here,
// reported as not screened, never as a clean image.
func (d *Driver) ImageConfigEnv(ctx context.Context, prepared sandbox.PreparedSpec) ([]string, error) {
	p, ok := prepared.(*Prepared)
	if !ok {
		return nil, fmt.Errorf("docker driver: image env: prepared spec is %T, not this driver's", prepared)
	}
	image := strings.TrimSpace(p.spec.Image)
	if image == "" {
		return nil, fmt.Errorf("docker driver: image env: the prepared spec names no image")
	}
	out, err := runtimeCmdContext(ctx, p.runtime, "image", "inspect", "--format", "{{json .Config.Env}}", image).Output()
	if err != nil {
		return nil, fmt.Errorf("docker driver: image env: %s image inspect %s: %w", p.runtime, image, err)
	}
	// A config with no ENV at all inspects as the JSON literal `null`,
	// which decodes into a nil slice — an image that sets none, not a
	// failure.
	var env []string
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("docker driver: image env: decoding %s's .Config.Env: %w", image, err)
	}
	return env, nil
}

// NeutralizeImageEnv implements [sandbox.ImageEnvScreener]: a copy of the
// prepared spec whose env carries each name with an EMPTY value, so Start's
// argv puts `--env NAME=` after the image's own `ENV` and the planted value
// never reaches a process in the container — and the names it actually
// emptied.
//
// A name the spec's env already carries is left as it is, and is NOT among
// them: that value is the operator's — the workflow's `sandbox.env:`, the
// launch overlay — and the screen never overrides the reviewed channel with
// an empty string. The planted value still never reaches a process, because
// the operator's wins; but the container is not created with `NAME=`, so a
// caller that reported it as emptied would publish a container state that is
// not the one started.
func (d *Driver) NeutralizeImageEnv(prepared sandbox.PreparedSpec, names []string) (sandbox.PreparedSpec, []string, error) {
	p, ok := prepared.(*Prepared)
	if !ok {
		return nil, nil, fmt.Errorf("docker driver: neutralize image env: prepared spec is %T, not this driver's", prepared)
	}
	if len(names) == 0 {
		return prepared, nil, nil
	}
	cp := *p
	cp.spec.Env = make(map[string]string, len(p.spec.Env)+len(names))
	maps.Copy(cp.spec.Env, p.spec.Env)
	emptied := make([]string, 0, len(names))
	for _, name := range names {
		if _, operators := cp.spec.Env[name]; operators {
			continue
		}
		// The same guard Start applies to every --env pair, applied here so
		// a malformed name from an image's ENV is refused at the screen
		// rather than killing the run three steps later with an argv error.
		if err := validateEnvVar(name, ""); err != nil {
			return nil, nil, fmt.Errorf("docker driver: neutralize image env %s: %w", name, err)
		}
		cp.spec.Env[name] = ""
		emptied = append(emptied, name)
	}
	return &cp, emptied, nil
}
