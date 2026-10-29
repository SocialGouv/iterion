package runtime

import (
	"context"
	"os"
	"slices"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
)

// EnvSandboxImageEnvAllow names the variables the image env screen must let
// an image contribute untouched, comma-separated and exact
// (`ITERION_SANDBOX_IMAGE_ENV_ALLOW=HTTPS_PROXY,AWS_REGION`).
//
// It is the greppable escape hatch of the neutralisation (philosophy #1: a
// bound with no override is a defect), and it is HOST-side only, read from
// the process environment of the runner — never from the workspace, never
// from a bot var, never from the `.bot`. The repository under review must
// never be able to allowlist itself, and `ITERION_SANDBOX_` is already an
// infra prefix of [platformcfg]'s bot-var deny list, so a stored bot var
// cannot shadow it either.
//
// Names are matched exactly and case-sensitively, the same way the class's
// exact names are. A name it lets through is still REPORTED: the operator
// asked for the variable, not for silence.
const EnvSandboxImageEnvAllow = "ITERION_SANDBOX_IMAGE_ENV_ALLOW"

// imageEnvScreen is what one screen of an image's ENV found.
type imageEnvScreen struct {
	// image is the reference that was inspected.
	image string
	// neutralized are the names the runtime overrode with an empty value,
	// sorted. They are the credential/routing sub-class of the deny class.
	neutralized []string
	// reported are the names of the class the image plants that keep their
	// value, sorted — an empty one would break the image doing its job.
	reported []string
	// allowed are the names the host knob let through, sorted.
	allowed []string
	// unscreened, when non-empty, says WHY nothing was screened: the driver
	// cannot read an image's config, or the read failed. The run is told;
	// it is never reported as a clean image.
	unscreened string
}

// anyFinding reports whether the screen has something to tell the operator.
func (s imageEnvScreen) anyFinding() bool {
	return len(s.neutralized) > 0 || len(s.reported) > 0 || len(s.allowed) > 0 || s.unscreened != ""
}

// screenSandboxImageEnv screens the ENV of the image a sandbox is about to
// start from, and returns the [sandbox.PreparedSpec] to start — the same one
// when nothing is neutralised, an amended one otherwise (#2317).
//
// A repository-authored devcontainer.json always names an image or a build,
// and that image's `ENV` applies to every process iterion runs in the
// container: the drivers pass `--env` for spec.Env alone, so the deny class
// [ToSandboxSpec] enforces on `containerEnv` never sees it. A repository
// that commits `{"image":"ghcr.io/planter/dev:1"}` whose Dockerfile carries
// `ENV CLAUDE_CODE_USE_BEDROCK=1` and the planter's AWS keys redirects every
// prompt of the run with zero containerEnv entries.
//
// What it does, per ARB-112 on #2317:
//
//   - the credential/routing sub-class of the class — the names that choose
//     WHERE a model call goes and WHAT credential it carries — is
//     NEUTRALISED: the driver starts the container with `--env NAME=`, which
//     overrides the image's ENV with an empty value every one of those
//     consumers reads as absent;
//   - the rest of the class is REPORTED only. Emptying `PATH`, `HOME` or a
//     loader path would break an image doing its job — iterion's own `full`
//     variant bakes `PATH`, and its runner image bakes `ITERION_VERSION` —
//     so the operator is told and the value stays;
//   - a driver that cannot read an image's config is reported as NOT
//     screened, with the reason. Nothing is inferred from a silence.
//
// It is called where [admitSandboxImageBackends] refuses, one step later:
// after Prepare and after a `build:` has materialised the image, because
// `image inspect` reads the local store and an unpulled tag would answer
// "no such image" — and before Start, so the override reaches the
// container's own environment and not merely the commands run inside it.
func screenSandboxImageEnv(
	ctx context.Context,
	driver sandbox.Driver,
	prepared sandbox.PreparedSpec,
	logger *iterlog.Logger,
) (sandbox.PreparedSpec, imageEnvScreen) {
	screen := imageEnvScreen{image: preparedImageRef(prepared)}
	// A driver that does not honour [sandbox.Spec.Image] starts nothing
	// FROM an image — the noop passthrough runs the commands on the host —
	// so there is no baked-in env to screen and no silence to confess. The
	// capability, not the driver's name: the next passthrough would need
	// the same answer.
	if !driver.Capabilities().SupportsImage {
		return prepared, screen
	}
	// The warning is emitted ONCE, from the lists as they end up: a line
	// claiming a name was overridden before the driver accepted the override
	// would be a log that lies, and the branch that could not neutralise is
	// exactly the one the operator must hear about.
	defer func() { warnImageEnvScreen(logger, screen) }()
	screener, ok := driver.(sandbox.ImageEnvScreener)
	if !ok {
		screen.unscreened = "the " + driver.Name() + " driver cannot read an image's baked-in env"
		return prepared, screen
	}
	env, err := screener.ImageConfigEnv(ctx, prepared)
	if err != nil {
		// Explicit, never a silent clean bill: the operator learns the
		// channel was not screened and why.
		screen.unscreened = err.Error()
		return prepared, screen
	}
	allow := imageEnvAllowList(os.Getenv(EnvSandboxImageEnvAllow))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			// An entry with no `=` is not a variable assignment; nothing
			// in the container reads it as one.
			continue
		}
		denied, neutralize := devcontainer.ScreenImageEnvName(name)
		if !denied {
			continue
		}
		switch {
		case allow[name]:
			screen.allowed = append(screen.allowed, name)
		case neutralize && devcontainer.UsableEnvName(name):
			screen.neutralized = append(screen.neutralized, name)
		default:
			// Reported, never handed to the driver: either the class says
			// so, or the name is one no driver could pass and no process
			// could resolve (an image's `Env` is the planter's text, and a
			// `AWS_SECRET<newline>ACCESS_KEY` entry reads as a variable to
			// nothing). Refusing the batch over such a name would leave the
			// well-formed planted names un-neutralised with it.
			screen.reported = append(screen.reported, name)
		}
	}
	// Sorted: these lists are a run event's payload and a warning's text,
	// and an image's ENV order is the planter's, not a fact to publish.
	slices.Sort(screen.neutralized)
	slices.Sort(screen.reported)
	slices.Sort(screen.allowed)
	if len(screen.neutralized) == 0 {
		return prepared, screen
	}
	neutralized, err := screener.NeutralizeImageEnv(prepared, screen.neutralized)
	if err != nil {
		// The neutralisation is the guard: a run that cannot have it must
		// not silently start without it. The names move to unscreened so
		// the caller's refusal names what it could not neutralise.
		screen.unscreened = err.Error()
		screen.reported = append(screen.reported, screen.neutralized...)
		slices.Sort(screen.reported)
		screen.neutralized = nil
		return prepared, screen
	}
	return neutralized, screen
}

// preparedImageRef is the image a PreparedSpec will start, read back through
// the same `Spec()` accessor [buildSandboxImageIfRequested] reads the built
// image through. "" when the driver's prepared spec does not expose one.
func preparedImageRef(prepared sandbox.PreparedSpec) string {
	sp, ok := prepared.(interface{ Spec() sandbox.Spec })
	if !ok {
		return ""
	}
	return sp.Spec().Image
}

// imageEnvAllowList parses [EnvSandboxImageEnvAllow]: comma-separated exact
// names, blanks ignored.
func imageEnvAllowList(raw string) map[string]bool {
	allow := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			allow[name] = true
		}
	}
	return allow
}

// warnImageEnvScreen tells the run's log what the image contributed. One
// line per disposition, each naming the variables, so a planted credential
// switch is visible in the run's own stream — the operator-facing half of
// the guard, which the `sandbox_image_env_planted` event carries to the
// studio.
func warnImageEnvScreen(logger *iterlog.Logger, screen imageEnvScreen) {
	if logger == nil || !screen.anyFinding() {
		return
	}
	image := screen.image
	if image == "" {
		image = "(unnamed)"
	}
	if len(screen.neutralized) > 0 {
		logger.Warn("sandbox: image %s bakes in %s — overridden with an empty value: a repository-authored image must not contribute a credential or a model route (set %s to let one through)",
			image, strings.Join(screen.neutralized, ", "), EnvSandboxImageEnvAllow)
	}
	if len(screen.reported) > 0 {
		logger.Warn("sandbox: image %s bakes in %s — kept, because an empty value would break the image: the container's processes read the image's value, not iterion's",
			image, strings.Join(screen.reported, ", "))
	}
	if len(screen.allowed) > 0 {
		logger.Warn("sandbox: image %s bakes in %s — let through by %s",
			image, strings.Join(screen.allowed, ", "), EnvSandboxImageEnvAllow)
	}
	if screen.unscreened != "" {
		logger.Warn("sandbox: the env image %s bakes in was NOT screened (%s) — a variable it plants reaches every process iterion runs in the container",
			image, screen.unscreened)
	}
}
