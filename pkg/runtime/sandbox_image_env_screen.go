package runtime

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
	"github.com/SocialGouv/iterion/pkg/store"
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
// Names are matched exactly and case-sensitively — against the spelling
// the IMAGE baked, not against a class entry (the class itself is
// case-insensitive, so one entry answers for every spelling). A name it
// lets through is still REPORTED: the operator asked for the variable, not
// for silence.
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
	// refused, when non-empty, says WHY a neutralisation this screen
	// DECIDED on could not be installed, and refusedNames are the names it
	// covered, sorted.
	//
	// Its own field, never folded into unscreened: the two are different
	// facts with different consequences. Unscreened says the channel was
	// never read — a driver whose images the kubelet pulls — and the run
	// proceeds under a stated silence. Refused says the class was read, a
	// credential WAS found, and the override the guard consists of did not
	// take: the run does not start (ARB-119). A reader that could not tell
	// them apart would read a blocked boot and a cloud run the same way.
	refused      string
	refusedNames []string
}

// anyFinding reports whether the screen has something to tell the operator.
func (s imageEnvScreen) anyFinding() bool {
	return len(s.neutralized) > 0 || len(s.reported) > 0 || len(s.allowed) > 0 ||
		s.unscreened != "" || s.refused != ""
}

// refusal is the typed error a refused neutralisation stops the boot with,
// or nil when the screen installed everything it decided on.
func (s imageEnvScreen) refusal() error {
	if s.refused == "" {
		return nil
	}
	return &ImageEnvNeutralisationRefusedError{
		Image:  s.image,
		Names:  slices.Clone(s.refusedNames),
		Reason: s.refused,
	}
}

// eventPayload is the screen as [store.EventSandboxImageEnvPlanted]
// carries it — the half of the guard an operator reads in the studio and
// in a bug report, rather than in the run's log (#2317).
//
// The two halves of the partition ALWAYS travel, the empty list
// included: a reader must never have to tell "this disposition caught
// nothing" from "this event does not carry that disposition". The two
// conditions — the host knob let a name through, the screen could not
// run — travel only when they hold, so their presence is the signal.
func (s imageEnvScreen) eventPayload(runID string) map[string]any {
	payload := map[string]any{
		"image":       s.image,
		"neutralized": envNameList(s.neutralized),
		"reported":    envNameList(s.reported),
		"run_id":      runID,
	}
	if len(s.allowed) > 0 {
		payload["allowed"] = envNameList(s.allowed)
	}
	if s.unscreened != "" {
		payload["unscreened"] = s.unscreened
	}
	if s.refused != "" {
		payload["refused"] = s.refused
		payload["refused_names"] = envNameList(s.refusedNames)
	}
	return payload
}

// sortedSet is one disposition as the event and the log publish it: in
// name order, each name once.
func sortedSet(names []string) []string {
	slices.Sort(names)
	return slices.Compact(names)
}

// envNameList renders one disposition for the event: a copy, so the
// payload does not alias the screen's own slices, and never nil, so an
// empty disposition serialises as `[]` and not as `null` — a null reads
// as "unknown" to every consumer of the event stream, which is the one
// thing this event must never say by accident.
func envNameList(names []string) []string {
	out := make([]string, len(names))
	copy(out, names)
	return out
}

// screenAndReportSandboxImageEnv is the seam the sandbox boot calls: it
// screens the image's baked-in env, reports what it found to the run —
// the warning inside the screen, the
// [store.EventSandboxImageEnvPlanted] event here — and returns the spec
// to start, or the typed refusal that must stop the boot.
//
// One function, because the two are one fact: a boot that neutralised a
// planted credential and an event that does not say so, or the reverse,
// are both a guard the operator cannot audit. Keeping them at a single
// seam is what makes the pair testable without a container (#2317).
//
// The error is CLOSED by default: a neutralisation the class decided on
// and the driver could not install means a planted credential would ride
// into a started container, which is the whole failure #2317 exists to
// prevent. The event and the warning go out FIRST, so the run's record
// carries what the image planted even though nothing started — a refusal
// the operator cannot read would be as bad as the silent start it
// replaces (ARB-119).
func screenAndReportSandboxImageEnv(
	ctx context.Context,
	driver sandbox.Driver,
	prepared sandbox.PreparedSpec,
	logger *iterlog.Logger,
	emit func(store.EventType, map[string]any) error,
	runID string,
) (sandbox.PreparedSpec, error) {
	screened, screen := screenSandboxImageEnv(ctx, driver, prepared, logger)
	emitImageEnvScreen(emit, screen, runID)
	return screened, screen.refusal()
}

// emitImageEnvScreen carries the screen's findings to the run's event
// stream (#2317). An image that plants nothing of the class emits
// nothing: an event on every started sandbox saying "nothing" would bury
// the ones that say something.
func emitImageEnvScreen(
	emit func(store.EventType, map[string]any) error,
	screen imageEnvScreen,
	runID string,
) {
	if !screen.anyFinding() {
		return
	}
	// Discarded the way every sandbox lifecycle event is: the emitter the
	// runtime wraps already warns on a store-side failure, and a degraded
	// store must not stop a run whose container was screened.
	_ = emit(store.EventSandboxImageEnvPlanted, screen.eventPayload(runID))
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
	// Sorted and deduplicated: these lists are a run event's payload and a
	// warning's text, an image's ENV order is the planter's rather than a
	// fact to publish, and its entries are not a set — docker's own build
	// collapses a repeated name, a config written by another tool need
	// not. A name listed twice in an audit record reads as two findings.
	screen.neutralized = sortedSet(screen.neutralized)
	screen.reported = sortedSet(screen.reported)
	screen.allowed = sortedSet(screen.allowed)
	if len(screen.neutralized) == 0 {
		return prepared, screen
	}
	neutralized, err := screener.NeutralizeImageEnv(prepared, screen.neutralized)
	if err != nil {
		// The neutralisation IS the guard, so this is the closed-by-default
		// branch: the names it covered move to their own disposition, where
		// the caller reads them to refuse the boot by name. They are not
		// folded into `reported` — nothing is kept, because nothing starts
		// — and `neutralized` is emptied, because nothing was overridden.
		screen.refused = err.Error()
		screen.refusedNames = screen.neutralized
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
	image := strconv.Quote(screen.image)
	if screen.image == "" {
		image = "(unnamed)"
	}
	if len(screen.neutralized) > 0 {
		logger.Warn("sandbox: image %s bakes in %s — overridden with an empty value: a repository-authored image must not contribute a credential or a model route (set %s to let one through)",
			image, quotedEnvNames(screen.neutralized), EnvSandboxImageEnvAllow)
	}
	if len(screen.reported) > 0 {
		logger.Warn("sandbox: image %s bakes in %s — kept, because an empty value would break the image: the container's processes read the image's value, not iterion's",
			image, quotedEnvNames(screen.reported))
	}
	if len(screen.allowed) > 0 {
		logger.Warn("sandbox: image %s bakes in %s — let through by %s",
			image, quotedEnvNames(screen.allowed), EnvSandboxImageEnvAllow)
	}
	if screen.unscreened != "" {
		// Quoted for the same reason the names are: the reason wraps a
		// driver error that carries the repository's own image
		// reference, so an unquoted one is a second door to the same
		// log-line injection.
		logger.Warn("sandbox: the env image %s bakes in was NOT screened (%s) — a variable it plants reaches every process iterion runs in the container",
			image, strconv.Quote(screen.unscreened))
	}
	if screen.refused != "" {
		// Its own line, and it says what happens next: this is not the
		// stated silence of an unscreened channel but a guard that was
		// decided and could not be installed, so the run does not start.
		logger.Warn("sandbox: image %s bakes in %s and the driver REFUSED to override them (%s) — the run does not start: a credential or a model route the image contributes would reach every process iterion runs in the container",
			image, quotedEnvNames(screen.refusedNames), strconv.Quote(screen.refused))
	}
}

// quotedEnvNames renders one disposition for the operator's LOG.
//
// Quoted, every name, because the text is the PLANTER's: an image's `Env`
// is whatever its OCI config holds, and a repository's `devcontainer.json`
// chooses the image reference. A planted `AWS_SECRET\nACCESS_KEY` — which
// the class catches on its `AWS_` namespace and which nothing in the
// container could resolve — would otherwise write lines of its own into
// the run's log. The devcontainer channel already quotes its refused keys
// for exactly this reason (emitRepoTrustEvents); this is the same fact at
// the image channel. The `sandbox_image_env_planted` event needs no such
// care: its payload is JSON, which escapes the name itself.
func quotedEnvNames(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	return strings.Join(quoted, ", ")
}

// ImageEnvNeutralisationRefusedError is the cause of a boot refused
// because the driver would not override what the image bakes in: the
// class was read, a credential or a model route WAS found, and the
// `--env NAME=` the guard consists of did not take.
//
// A distinct type rather than a message, for the same reason the screen
// keeps `refused` apart from `unscreened`: a caller — a doctor, a bug
// report, a test — must be able to tell "the guard could not be
// installed" from "this driver screens nothing", and read WHICH names
// were at stake without parsing prose.
type ImageEnvNeutralisationRefusedError struct {
	// Image is the reference whose baked-in env was screened.
	Image string
	// Names are the names the screen decided to neutralise and could
	// not, sorted.
	Names []string
	// Reason is the driver's own refusal.
	Reason string
}

func (e *ImageEnvNeutralisationRefusedError) Error() string {
	image := strconv.Quote(e.Image)
	if e.Image == "" {
		image = "(unnamed)"
	}
	return "sandbox: image " + image + " bakes in " + quotedEnvNames(e.Names) +
		" and the driver refused to override them: " + e.Reason
}
