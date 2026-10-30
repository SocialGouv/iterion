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
// for silence. What the container then carries for it is READ from the
// container like every other name — the knob withholds iterion's
// override, it does not stop the driver's own pair from winning, and
// `allowed` is published only for a name the container shows untouched.
const EnvSandboxImageEnvAllow = "ITERION_SANDBOX_IMAGE_ENV_ALLOW"

// imageEnvScreen is one screen of an image's ENV: the DECISION it took
// before the container existed, and the DISPOSITIONS it DEDUCED from the
// container that started.
//
// The split is the lesson of two contradicted audit records (#2317, rounds
// 3 and 4). The decision is iterion's own act and is known in advance —
// which names the class caught, which ones the driver was asked to empty.
// A disposition is a claim about a CONTAINER — "set and empty", "the
// image's value kept", "iterion's own value" — and reassembling it from
// the rules that produce it published, twice, a state `docker inspect`
// contradicted: once `HTTP_PROXY` as emptied, once `NODE_EXTRA_CA_CERTS`
// as "let through untouched", of a container carrying iterion's per-run
// value. Both times the rules were right and the ORDER they were applied
// in was not.
//
// So a disposition is a measurement here: the image's `ENV` compared, name
// by name, to the environment the container was CREATED with, read from
// the driver that created it ([sandbox.StartedEnvReader]). It cannot
// contradict `docker inspect` by construction, and when the container
// cannot be read, nothing is filed at all — [imageEnvScreen.undeduced]
// says so, and never a guess.
type imageEnvScreen struct {
	// image is the reference that was inspected.
	image string

	// planted are the names of the deny class the image bakes into its
	// own `ENV`, sorted. The ONE fact that does not depend on the
	// container: it is read from `image inspect` and it is what the
	// operator judges the image on.
	planted []string
	// imageEnv holds EVERY value the image bakes for a planted name, in
	// no meaningful order. Every disposition below is a comparison
	// against it, which is what keeps an arm from reading "kept" off a
	// container whose value merely looks plausible.
	//
	// A slice and not one value, because an image's `Env` is not a set —
	// docker's own build collapses a repeated name, a config written by
	// another tool need not — and WHICH of two entries the daemon keeps
	// is not a fact this screen may assume: measured on docker 29.9.2,
	// a container created with no `--env` keeps BOTH entries (the
	// process reads the last), and one created with a single unrelated
	// `--env` pair keeps the FIRST and drops the other. Picking an end
	// published a name the container carries at the image's own value as
	// `unexplained` — the one disposition this record declares
	// unreachable (#2317, round 5 F1). So the comparison asks whether
	// the container's value is ONE OF the image's, which holds under
	// both merges and under the next runtime's.
	imageEnv map[string][]string
	// allow are the planted names the host knob named, so the screen
	// withheld its override. A name of this set is `allowed` only if the
	// container shows the image's value: the knob stops ITERION's
	// `--env NAME=`, not the driver's own pair.
	allow map[string]bool
	// runEnv is the RUN's own reviewed env as the prepared spec carried
	// it BEFORE the neutralisation — the workflow's `sandbox.env:`, the
	// launch overlay, the forfait config dirs — and driverSets the names
	// the driver declared its own Start would set. They are not
	// dispositions: they ATTRIBUTE a container value that is neither the
	// image's nor empty, which the container alone cannot explain.
	runEnv     map[string]string
	driverSets map[string]bool
	// emptied are the names the driver reported it overrode with
	// `--env NAME=`, iterion's own act. A name is `neutralized` when the
	// container AGREES with it.
	emptied []string

	// deduced says the container's own environment was read and the
	// dispositions below are measurements of it. False means none of
	// them travels: see undeduced.
	deduced bool
	// neutralized are the names the container carries EMPTY — set and
	// empty, which every consumer of that sub-class reads as absent —
	// where the image baked a value. The credential/routing sub-class,
	// measured at the container.
	neutralized []string
	// reported are the names the container carries at the IMAGE's own
	// value: the image's value does reach every process iterion starts
	// there. This half is told and not emptied — for most of it because
	// an empty value would break an image doing its job, and for the
	// startup-code channels because an image a repository chooses is the
	// repository's code, which no `--env NAME=` makes untrue (ARB-121).
	reported []string
	// allowed are the planted names the host knob let through AND the
	// container shows untouched — the image's own value, as the operator
	// asked. A knob-named name the driver's own pair overrode is filed
	// under driverSet instead, because that is what the container has.
	allowed []string
	// operatorSet are the names the container carries at the value the
	// RUN's own reviewed env holds: the operator's channel won, the
	// image's value is never read, and iterion did not override the
	// reviewed channel with an empty string.
	operatorSet []string
	// driverSet are the names the container carries at ITERION's own
	// value — neither the image's nor the run's — for a name the driver
	// declared its Start sets: the egress proxy's spellings, the per-run
	// CA bundles, the JVM truststore.
	//
	// Apart from operatorSet because the two answer different questions
	// for whoever reads the record: an operator finds an operatorSet
	// name in their own `.bot` or launch env, and a driverSet name
	// nowhere — it is iterion's own infrastructure, applied because the
	// run has an enforced network policy.
	driverSet []string
	// unexplained are the names whose container value matches NOTHING
	// the run can account for: not the image's, not the run's own env,
	// and the driver declared no pair for them. Nothing in this
	// repository produces one — it is the shape of a driver whose
	// declaration drifted from its argv — and it is published rather
	// than folded into a neighbour, because a record that guessed here
	// is the defect this whole deduction exists to retire.
	unexplained []string

	// unscreened, when non-empty, says WHY nothing was screened: the
	// driver cannot read an image's config, or the read failed. The run
	// is told; it is never reported as a clean image.
	unscreened string
	// undeduced, when non-empty, says WHY the dispositions could not be
	// measured although the class WAS read: no container was handed back
	// (the boot was refused, or Start failed — which can be a
	// postCreate command failing in a container the driver then
	// removes), the driver cannot read back a container it started, or
	// that read failed. The planted list still travels — it is the
	// image's own fact — and not one disposition does.
	undeduced string
	// refused, when non-empty, says WHY a neutralisation this screen
	// DECIDED on could not be installed, and refusedNames are the names
	// it covered, sorted.
	//
	// No docker image can produce it today, and that is proved rather
	// than assumed: the screen only hands over names passing
	// [devcontainer.UsableEnvName], and docker's own `--env` guard
	// refuses on exactly the characters that predicate excludes, with an
	// empty value refused for nothing else — pkg/sandbox/docker's
	// TestUsableEnvNameImpliesTheDockerEnvGuardAccepts walks the
	// implication. It is kept closed-by-default for the next screener,
	// whose guard may be narrower, and both docs pages say so rather
	// than describing an outcome an operator could observe.
	//
	// Its own field, never folded into unscreened or undeduced: the
	// three are different facts with different consequences. Unscreened
	// says the channel was never read — a driver whose images the
	// kubelet pulls — and the run proceeds under a stated silence.
	// Undeduced says the class was read and the container was not.
	// Refused says the class was read, a credential WAS found, and the
	// override the guard consists of did not take: the run does not
	// start (ARB-119).
	refused      string
	refusedNames []string
}

// anyFinding reports whether the screen has something to tell the operator.
func (s imageEnvScreen) anyFinding() bool {
	return len(s.planted) > 0 || s.unscreened != "" || s.refused != ""
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
// `planted` travels whenever the image's own ENV was read: it is the
// image's fact and depends on no container.
//
// The DISPOSITIONS travel only when they were measured. Both halves of
// the partition then always travel, the empty list included: a reader must
// never have to tell "this disposition caught nothing" from "this event
// does not carry that disposition". The conditional ones — the host knob
// let a name through untouched, the run's own env set one, the driver set
// one, the container holds a value nothing explains — travel only when
// they hold, so their presence is the signal.
//
// When nothing was measured, not one disposition is present and exactly
// one key says why: `unscreened` (the class was never read) or `undeduced`
// (it was read, and the container was not). A reader that found an empty
// `neutralized` next to `undeduced` would read "nothing was planted" off
// an event that means "nobody looked".
func (s imageEnvScreen) eventPayload(runID string) map[string]any {
	payload := map[string]any{
		"image":  s.image,
		"run_id": runID,
	}
	if s.unscreened == "" {
		payload["planted"] = envNameList(s.planted)
	}
	if s.deduced {
		payload["neutralized"] = envNameList(s.neutralized)
		payload["reported"] = envNameList(s.reported)
		if len(s.allowed) > 0 {
			payload["allowed"] = envNameList(s.allowed)
		}
		if len(s.operatorSet) > 0 {
			payload["operator_set"] = envNameList(s.operatorSet)
		}
		if len(s.driverSet) > 0 {
			payload["driver_set"] = envNameList(s.driverSet)
		}
		if len(s.unexplained) > 0 {
			payload["unexplained"] = envNameList(s.unexplained)
		}
	}
	if s.unscreened != "" {
		payload["unscreened"] = s.unscreened
	}
	if s.undeduced != "" {
		payload["undeduced"] = s.undeduced
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

// startScreenedSandbox is the seam the sandbox boot starts its container
// through: it screens the ENV the image bakes in, starts the container
// from the SCREENED spec, and reports what the image planted from the
// container that actually started (#2317).
//
// One function, because the three are one fact. A boot that neutralised a
// planted credential and an event that does not say so — or that says it
// of a container the daemon describes differently — are both a guard the
// operator cannot audit, and the second is what shipped twice: the record
// was PREDICTED from the screen's own rules while the container had
// already resolved them in another order. Here the start sits between the
// decision and the record, so the record is read off its result and the
// two cannot be wired apart.
//
// The refusal is CLOSED by default: a neutralisation the class decided on
// and the driver could not install means a planted credential would ride
// into a started container, which is the whole failure #2317 exists to
// prevent. Nothing starts, the event goes out with `refused` and the
// names on it — a refusal the operator cannot read would be as bad as the
// silent start it replaces (ARB-119) — and the typed
// [ImageEnvNeutralisationRefusedError] travels in the error chain so the
// caller can tell it from a Start that failed for its own reasons.
//
// It returns the Run, the prepared spec the container was created from
// (the amended one), and the error.
func startScreenedSandbox(
	ctx context.Context,
	driver sandbox.Driver,
	prepared sandbox.PreparedSpec,
	info sandbox.RunInfo,
	logger *iterlog.Logger,
	emit func(store.EventType, map[string]any) error,
	runID string,
) (sandbox.Run, sandbox.PreparedSpec, error) {
	screened, screen := screenSandboxImageEnv(ctx, driver, prepared, info)
	if err := screen.refusal(); err != nil {
		screen.undeduced = "no container was created: the driver refused the override this screen decided on"
		reportImageEnvScreen(screen, driver.Name(), logger, emit, runID)
		return nil, prepared, err
	}
	run, err := driver.Start(ctx, screened, info)
	if err != nil {
		// The class was read and no container was handed back to read:
		// the record says what the image plants and claims nothing about
		// a container. Wording holds to what is known — Start can fail
		// in a postCreate command, in a container the driver then
		// removes, so whether one was created is the error's own fact to
		// carry, never this record's.
		screen.undeduced = "no container this driver could hand back: Start failed (" + err.Error() + ")"
		reportImageEnvScreen(screen, driver.Name(), logger, emit, runID)
		return nil, screened, err
	}
	reportImageEnvScreen(deduceImageEnvScreen(ctx, driver, run, screen), driver.Name(), logger, emit, runID)
	return run, screened, nil
}

// reportImageEnvScreen tells the operator what the image contributed,
// twice: the warning in the run's log, and the
// [store.EventSandboxImageEnvPlanted] event the studio and a bug report
// read. An image that plants nothing of the class says nothing — an event
// on every started sandbox saying "nothing" would bury the ones that say
// something.
func reportImageEnvScreen(
	screen imageEnvScreen,
	driverName string,
	logger *iterlog.Logger,
	emit func(store.EventType, map[string]any) error,
	runID string,
) {
	if !screen.anyFinding() {
		return
	}
	warnImageEnvScreen(logger, driverName, screen)
	if emit == nil {
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
// What it DECIDES, per ARB-112 and ARB-121 on #2317:
//
//   - the credential/routing sub-class of the class — the names that choose
//     WHERE a model call goes and WHAT credential it carries — is
//     NEUTRALISED: the driver is asked for `--env NAME=`, which overrides
//     the image's ENV with an empty value every one of those consumers
//     reads as absent. That DECLARATIVE redirection is the whole promise;
//   - the rest of the class is kept and told. Emptying `PATH`, `HOME` or a
//     loader path would break an image doing its job — iterion's own
//     `full` variant bakes `PATH`, and its runner image bakes
//     `ITERION_VERSION`. The startup-code channels (`BASH_ENV`,
//     `NODE_OPTIONS`, `LD_PRELOAD`, `GIT_CONFIG_*`, …) are kept for
//     another reason, stated rather than silent: they FIRE, measured one
//     by one, and an image a repository chooses is the repository's code —
//     its binaries, its files, its `postCreateCommand` — so emptying one
//     name of a channel those re-open would promise what the artefact
//     cannot owe (ARB-121);
//   - a name the host knob let through is not handed over: the operator
//     asked for that variable;
//   - a driver that cannot read an image's config is reported as NOT
//     screened, with the reason. Nothing is inferred from a silence.
//
// What the container then HAS is not decided here: it is read from the
// container by [deduceImageEnvScreen]. This function records what the
// other channels into those names are — the run's own env, the driver's
// declared pairs — for the attribution that deduction owes, and nothing
// in it is published as a disposition.
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
	info sandbox.RunInfo,
) (sandbox.PreparedSpec, imageEnvScreen) {
	screen := imageEnvScreen{image: preparedImageRef(prepared), imageEnv: map[string][]string{}}
	// A driver that does not honour [sandbox.Spec.Image] starts nothing
	// FROM an image — the noop passthrough runs the commands on the host —
	// so there is no baked-in env to screen and no silence to confess. The
	// capability, not the driver's name: the next passthrough would need
	// the same answer.
	if !driver.Capabilities().SupportsImage {
		return prepared, screen
	}
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
	// Who ELSE gives these names a value when the container is created:
	// the run's own reviewed env, which the prepared spec carries, and
	// the driver's own `--env` pairs, which it alone knows. Read HERE,
	// before the neutralisation amends the spec, because the question
	// they answer is "whose value is this" about the ORIGINAL channels.
	screen.runEnv = preparedSpecEnv(prepared)
	screen.driverSets = driverStartEnvNames(screener, info)
	screen.allow = map[string]bool{}
	var decided []string
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			// An entry with no `=` is not a variable assignment; nothing
			// in the container reads it as one.
			continue
		}
		denied, neutralize := devcontainer.ScreenImageEnvName(name)
		if !denied {
			continue
		}
		// Every entry of the name, never only one: which of two the
		// daemon keeps for the container is measured to depend on the
		// `--env` pairs beside it, so the image side stays order-free
		// (see [imageEnvScreen.imageEnv]).
		screen.planted = append(screen.planted, name)
		screen.imageEnv[name] = append(screen.imageEnv[name], value)
		if allow[name] {
			screen.allow[name] = true
			continue
		}
		// Never handed to the driver otherwise: either the class says
		// so, or the name is one no driver could pass and no process
		// could resolve (an image's `Env` is the planter's text, and a
		// `AWS_SECRET<newline>ACCESS_KEY` entry reads as a variable to
		// nothing). Refusing the batch over such a name would leave the
		// well-formed planted names un-neutralised with it.
		if neutralize && devcontainer.UsableEnvName(name) {
			decided = append(decided, name)
		}
	}
	// Sorted and deduplicated: these lists are a run event's payload and a
	// warning's text, an image's ENV order is the planter's rather than a
	// fact to publish, and its entries are not a set — docker's own build
	// collapses a repeated name, a config written by another tool need
	// not. A name listed twice in an audit record reads as two findings.
	screen.planted = sortedSet(screen.planted)
	decided = sortedSet(decided)
	if len(decided) == 0 {
		return prepared, screen
	}
	neutralized, emptied, err := screener.NeutralizeImageEnv(prepared, info, decided)
	if err != nil {
		// The neutralisation IS the guard, so this is the closed-by-default
		// branch: the names it covered move to their own disposition, where
		// the caller reads them to refuse the boot by name. Nothing is
		// kept, because nothing starts.
		screen.refused = err.Error()
		screen.refusedNames = decided
		return prepared, screen
	}
	// Keyed on what the screen ASKED, so a name the driver echoes that
	// was never decided on cannot enter the record — and it is the
	// driver's answer, never the screen's decision: the container the
	// operator will read `--env NAME=` into is the driver's.
	for _, name := range decided {
		if slices.Contains(emptied, name) {
			screen.emptied = append(screen.emptied, name)
		}
	}
	return neutralized, screen
}

// deduceImageEnvScreen files every planted name under the disposition the
// CONTAINER witnesses: the image's `ENV` compared, name by name, to the
// environment the container was created with, read from the driver that
// created it.
//
// This is the whole answer to two rounds of contradicted records. A
// disposition is not inferred from the rules that produce a container's
// env — "the knob allowed it, so it is untouched", "the driver's pair is
// appended last, so it wins" — because those rules are right and their
// ORDER is what the daemon resolves. It is read off the result: emptied,
// kept at the image's value, or replaced. The two sources of a
// replacement are then attributed from what the run itself declared, and
// a value none of them explains is published as exactly that.
//
// A driver that cannot read back a container it started, or whose read
// failed, leaves every disposition EMPTY and says why: the record then
// carries what the image planted and claims nothing about the container.
func deduceImageEnvScreen(
	ctx context.Context,
	driver sandbox.Driver,
	run sandbox.Run,
	screen imageEnvScreen,
) imageEnvScreen {
	if len(screen.planted) == 0 {
		// Nothing of the class was planted: there is no disposition to
		// measure, and a container read would answer about nothing.
		return screen
	}
	reader, ok := driver.(sandbox.StartedEnvReader)
	if !ok {
		screen.undeduced = "the " + driver.Name() + " driver cannot read back the env of a container it started"
		return screen
	}
	entries, err := reader.StartedContainerEnv(ctx, run)
	if err != nil {
		screen.undeduced = err.Error()
		return screen
	}
	container := map[string]string{}
	for _, entry := range entries {
		if name, value, ok := strings.Cut(entry, "="); ok && name != "" {
			// The LAST entry wins, which is docker's own resolution and
			// the order the whole claim is about.
			container[name] = value
		}
	}
	for _, name := range screen.planted {
		got, inContainer := container[name]
		switch {
		case !inContainer:
			// The image baked the name and the container has no entry
			// for it: the planted value reaches no process there, which
			// is what this disposition publishes. No docker form produces
			// it today — `--env NAME=` sets it empty, and a bare
			// `--env NAME` would drop it — and a screener that gains one
			// inherits an honest record.
			screen.neutralized = append(screen.neutralized, name)
		case slices.Contains(screen.emptied, name) && got == "":
			// iterion asked for `--env NAME=` and the container agrees.
			screen.neutralized = append(screen.neutralized, name)
		case slices.Contains(screen.imageEnv[name], got):
			// The image's own value is what every process iterion starts
			// there reads. ONE OF the image's values, because a name the
			// image bakes twice has two and the daemon keeps whichever
			// its merge picks; either way the container carries what the
			// IMAGE put there and nothing else. `allowed` is the same
			// measurement for a name the operator asked through:
			// untouched, and asked for.
			if screen.allow[name] {
				screen.allowed = append(screen.allowed, name)
			} else {
				screen.reported = append(screen.reported, name)
			}
		case screen.driverSets[name]:
			// Neither the image's value nor empty, for a name the driver
			// DECLARED its own Start sets: iterion's own infrastructure
			// won, which is the one answer an operator will not find in
			// their `.bot`. Declared, never assumed from the value.
			screen.driverSet = append(screen.driverSet, name)
		case mapHasKey(screen.runEnv, name) && screen.runEnv[name] == got:
			// The run's own reviewed channel is what the container
			// carries, value for value.
			screen.operatorSet = append(screen.operatorSet, name)
		default:
			screen.unexplained = append(screen.unexplained, name)
		}
	}
	screen.neutralized = sortedSet(screen.neutralized)
	screen.reported = sortedSet(screen.reported)
	screen.allowed = sortedSet(screen.allowed)
	screen.operatorSet = sortedSet(screen.operatorSet)
	screen.driverSet = sortedSet(screen.driverSet)
	screen.unexplained = sortedSet(screen.unexplained)
	screen.deduced = true
	return screen
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

// preparedSpecEnv is the RUN's own env the prepared spec carries — the
// workflow's `sandbox.env:`, the launch overlay, the forfait config dirs —
// read back through the same `Spec()` accessor [preparedImageRef] reads the
// image through. nil when the driver's prepared spec exposes no spec.
//
// A read of a documented public field, used to ATTRIBUTE a value the
// container carries: the container says WHAT it has, and this says whose
// it is.
func preparedSpecEnv(prepared sandbox.PreparedSpec) map[string]string {
	sp, ok := prepared.(interface{ Spec() sandbox.Spec })
	if !ok {
		return nil
	}
	return sp.Spec().Env
}

// driverStartEnvNames is the set of names the driver's own Start will set
// for this run, asked of the driver because nothing else can know: the
// values come from [sandbox.RunInfo] inside Start and appear in no spec.
func driverStartEnvNames(screener sandbox.ImageEnvScreener, info sandbox.RunInfo) map[string]bool {
	names := screener.StartEnvNames(info)
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// mapHasKey reports whether m carries name at all — a key with an EMPTY
// value is still the operator's choice, and `v, ok :=` is the only read
// that says so.
func mapHasKey(m map[string]string, name string) bool {
	_, ok := m[name]
	return ok
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
func warnImageEnvScreen(logger *iterlog.Logger, driverName string, screen imageEnvScreen) {
	if logger == nil || !screen.anyFinding() {
		return
	}
	image := strconv.Quote(screen.image)
	if screen.image == "" {
		image = "(unnamed)"
	}
	if len(screen.neutralized) > 0 {
		logger.Warn("sandbox: image %s bakes in %s — the container reads them EMPTY: a repository-authored image must not contribute a credential or a model route (set %s to let one through)",
			image, quotedEnvNames(screen.neutralized), EnvSandboxImageEnvAllow)
	}
	if len(screen.reported) > 0 {
		// Bounded to the layer this screen is about: iterion forwards a
		// run-level env into every in-container `docker exec`, and that
		// layer may legitimately carry the OPERATOR's value for one of
		// these names. The claim is about the container's own env, which
		// is what was read.
		//
		// The reason is not given here, and deliberately: this half holds
		// names kept because an empty value would break the image AND
		// names kept although they lead to code (ARB-121), and a single
		// line that gave one reason for both would be false about the
		// other. What it states is the measurement — the container has
		// the image's value — and the page carries the rest.
		logger.Warn("sandbox: image %s bakes in %s — kept: the container was created with the image's value, which its processes read unless a command's own env layer sets the name (a name of this half may lead to CODE the image ships — see the image ENV screen in docs/sandbox.md)",
			image, quotedEnvNames(screen.reported))
	}
	if len(screen.operatorSet) > 0 {
		logger.Warn("sandbox: image %s bakes in %s — the container carries the value the run's own env sets, so the OPERATOR's wins: the image's is never read, and iterion does not override the reviewed channel with an empty one",
			image, quotedEnvNames(screen.operatorSet))
	}
	if len(screen.driverSet) > 0 {
		logger.Warn("sandbox: image %s bakes in %s — the container carries ITERION's value: the %s driver sets them itself when it starts the container (the egress proxy's variables, the per-run CA bundles), so the image's is never read, and they are not empty either",
			image, quotedEnvNames(screen.driverSet), driverName)
	}
	if len(screen.allowed) > 0 {
		logger.Warn("sandbox: image %s bakes in %s — the container carries the image's own value, let through by %s",
			image, quotedEnvNames(screen.allowed), EnvSandboxImageEnvAllow)
	}
	if len(screen.unexplained) > 0 {
		logger.Warn("sandbox: image %s bakes in %s and the container carries a value that is neither the image's, nor this run's own env, nor a pair the %s driver declares it sets — the audit record cannot attribute it: report it (#2317)",
			image, quotedEnvNames(screen.unexplained), driverName)
	}
	if screen.unscreened != "" {
		// Quoted for the same reason the names are: the reason wraps a
		// driver error that carries the repository's own image
		// reference, so an unquoted one is a second door to the same
		// log-line injection.
		logger.Warn("sandbox: the env image %s bakes in was NOT screened (%s) — a variable it plants reaches every process iterion runs in the container",
			image, strconv.Quote(screen.unscreened))
	}
	if screen.undeduced != "" && screen.refused == "" {
		// Its own line: the class WAS read, so the names are known, and
		// what the container did with them is not. Never a guess, and
		// never silence.
		logger.Warn("sandbox: image %s bakes in %s and what the container did with them could not be read (%s) — no disposition is reported for this run",
			image, quotedEnvNames(screen.planted), strconv.Quote(screen.undeduced))
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
