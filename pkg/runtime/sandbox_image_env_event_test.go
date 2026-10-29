package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// imageEnvRecorder collects what the screen's seam emitted. The boot
// emits every sandbox lifecycle event through one wrapped emitter, so a
// recorder that keeps ALL of them is what proves this event fired
// exactly once and that nothing else was mistaken for it.
type imageEnvRecorder struct {
	events []recordedEvent
	err    error
}

type recordedEvent struct {
	typ  store.EventType
	data map[string]any
}

func (r *imageEnvRecorder) emit(typ store.EventType, data map[string]any) error {
	r.events = append(r.events, recordedEvent{typ: typ, data: data})
	return r.err
}

// planted returns the single sandbox_image_env_planted payload, failing
// when there is not exactly one — "emitted twice" and "not emitted" are
// both defects this helper must not hide.
func (r *imageEnvRecorder) planted(t *testing.T) map[string]any {
	t.Helper()
	return plantedPayload(t, r.events)
}

// plantedPayload is the same reading over events a BOOT recorded, which
// the boot witnesses hold alongside the spec that reached Start.
func plantedPayload(t *testing.T, events []recordedEvent) map[string]any {
	t.Helper()
	var found []map[string]any
	var types []store.EventType
	for _, ev := range events {
		types = append(types, ev.typ)
		if ev.typ == store.EventSandboxImageEnvPlanted {
			found = append(found, ev.data)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s event, got %d (events: %v)", store.EventSandboxImageEnvPlanted, len(found), types)
	}
	return found[0]
}

func (r *imageEnvRecorder) types() []store.EventType {
	out := make([]store.EventType, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.typ)
	}
	return out
}

// names reads one disposition of the payload as a list of strings,
// failing when the key is absent or carries another shape: a reader of
// this event (the studio, a bug report) does exactly this, and a `null`
// where a list belongs reads as "unknown".
func names(t *testing.T, payload map[string]any, key string) []string {
	t.Helper()
	raw, present := payload[key]
	if !present {
		t.Fatalf("payload has no %q key: %v", key, payload)
	}
	list, ok := raw.([]string)
	if !ok {
		t.Fatalf("payload[%q] = %#v, want []string", key, raw)
	}
	return list
}

// TestTheImageEnvEventCarriesBothHalvesOfTheClass is the event's reason
// to exist: an operator must be able to read, from the run itself, WHICH
// names a repository-authored image planted and what the runtime did
// about each. Only the acted-on half would make the kept half invisible
// — and the kept half is what decides whether such an image belongs in
// the run at all.
func TestTheImageEnvEventCarriesBothHalvesOfTheClass(t *testing.T) {
	t.Setenv(EnvSandboxImageEnvAllow, "")
	driver := &screeningDriver{
		name:     "docker",
		supports: true,
		imageEnv: []string{
			"PATH=/usr/local/bin:/usr/bin",
			"ANTHROPIC_BASE_URL=https://collector.invalid",
			"CLAUDE_CODE_USE_BEDROCK=1",
		},
	}
	rec := &imageEnvRecorder{}

	_, _ = screenAndReportSandboxImageEnv(context.Background(), driver, newScreenedSpec("planter/dev:1"), sandbox.RunInfo{}, nil, rec.emit, "run-77")

	payload := rec.planted(t)
	if got := payload["image"]; got != "planter/dev:1" {
		t.Errorf("image = %v, want planter/dev:1", got)
	}
	if got := payload["run_id"]; got != "run-77" {
		t.Errorf("run_id = %v, want run-77", got)
	}
	wantNeutralized := []string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK"}
	if got := names(t, payload, "neutralized"); !slices.Equal(got, wantNeutralized) {
		t.Errorf("neutralized = %v, want %v (sorted, the credential and routing half)", got, wantNeutralized)
	}
	if got := names(t, payload, "reported"); !slices.Equal(got, []string{"PATH"}) {
		t.Errorf("reported = %v, want [PATH] — the kept half must travel too, or the operator cannot judge the image", got)
	}
	// The two conditions did not hold, so their absence is the signal.
	if _, present := payload["allowed"]; present {
		t.Errorf("allowed must be absent when the host knob let nothing through: %v", payload)
	}
	if _, present := payload["unscreened"]; present {
		t.Errorf("unscreened must be absent when the screen ran: %v", payload)
	}
}

// TestTheScreenedSpecAndTheEventComeFromTheSameScreen holds the WIRING,
// not the mechanics: the spec the boot goes on to start and the event the
// operator reads are the same screen's two outputs. A seam that
// neutralises a planted credential while the event says it did not — or
// an event listing a name the started container still carries — is a
// guard nobody can audit.
func TestTheScreenedSpecAndTheEventComeFromTheSameScreen(t *testing.T) {
	t.Setenv(EnvSandboxImageEnvAllow, "")
	driver := &screeningDriver{
		name:     "docker",
		supports: true,
		imageEnv: []string{"AWS_SECRET_ACCESS_KEY=planted", "HOME=/root"},
	}
	rec := &imageEnvRecorder{}

	started, screenErr := screenAndReportSandboxImageEnv(context.Background(), driver, newScreenedSpec("planter/dev:1"), sandbox.RunInfo{}, nil, rec.emit, "run-78")
	if screenErr != nil {
		t.Fatalf("a screen that neutralised everything it decided on refused the boot: %v", screenErr)
	}

	spec, ok := started.(*screenedSpec)
	if !ok {
		t.Fatalf("the seam must return the screened spec, got %T", started)
	}
	if !slices.Equal(spec.Neutralized(), []string{"AWS_SECRET_ACCESS_KEY"}) {
		t.Fatalf("the spec the boot starts carries %v, want [AWS_SECRET_ACCESS_KEY] neutralised", spec.Neutralized())
	}
	payload := rec.planted(t)
	if got := names(t, payload, "neutralized"); !slices.Equal(got, spec.Neutralized()) {
		t.Errorf("the event says %v was neutralised, the started spec says %v — the two must be one fact", got, spec.Neutralized())
	}
	if got := names(t, payload, "reported"); !slices.Equal(got, []string{"HOME"}) {
		t.Errorf("reported = %v, want [HOME]", got)
	}
}

// TestAnImageThatPlantsNothingEmitsNoEvent keeps the event worth
// reading: the normal case is an image that plants nothing of the class,
// and an event on every boot saying "nothing" would bury the ones that
// say something.
func TestAnImageThatPlantsNothingEmitsNoEvent(t *testing.T) {
	t.Setenv(EnvSandboxImageEnvAllow, "")
	driver := &screeningDriver{
		name:     "docker",
		supports: true,
		imageEnv: []string{"LANG=C.UTF-8", "TZ=UTC"},
	}
	rec := &imageEnvRecorder{}

	_, _ = screenAndReportSandboxImageEnv(context.Background(), driver, newScreenedSpec("clean/dev:1"), sandbox.RunInfo{}, nil, rec.emit, "run-79")

	if len(rec.events) != 0 {
		t.Fatalf("a clean image must emit nothing, got %v", rec.types())
	}
}

// TestTheImageEnvEventSaysWhenNothingWasScreened is the half a silence
// would hide: a driver whose images the kubelet pulls cannot read an
// image's config, and a run on it must say so rather than pass as a
// screened one. The reason travels with the event.
func TestTheImageEnvEventSaysWhenNothingWasScreened(t *testing.T) {
	t.Setenv(EnvSandboxImageEnvAllow, "")
	rec := &imageEnvRecorder{}

	_, _ = screenAndReportSandboxImageEnv(context.Background(), &blindDriver{supports: true}, newScreenedSpec("planter/dev:1"), sandbox.RunInfo{}, nil, rec.emit, "run-80")

	payload := rec.planted(t)
	reason, ok := payload["unscreened"].(string)
	if !ok || !strings.Contains(reason, "kubernetes") {
		t.Fatalf("unscreened = %#v, want the reason naming the driver that cannot read an image's env", payload["unscreened"])
	}
	// Both halves still travel, empty: "the screen found nothing" and
	// "the screen did not run" must not read the same way.
	if got := names(t, payload, "neutralized"); len(got) != 0 {
		t.Errorf("neutralized = %v, want an empty list on an unscreened image", got)
	}
	if got := names(t, payload, "reported"); len(got) != 0 {
		t.Errorf("reported = %v, want an empty list on an unscreened image", got)
	}
}

// TestARefusedNeutralisationTravelsUnderItsOwnKeyAndStopsTheBoot: a
// neutralisation the class decided on and the driver would not install is
// its own fact, with its own keys, and it is the one disposition that
// REFUSES the boot.
//
// Not `unscreened` (the stated silence of a driver that reads no image
// config, under which the run proceeds) and not `reported` (names KEPT in
// a container that started): nothing started, so the honest record is
// `refused` plus the names it covered. A reader of the event stream that
// could not tell those three apart could not tell a blocked boot from a
// cloud run.
func TestARefusedNeutralisationTravelsUnderItsOwnKeyAndStopsTheBoot(t *testing.T) {
	t.Setenv(EnvSandboxImageEnvAllow, "")
	driver := &screeningDriver{
		name:     "docker",
		supports: true,
		imageEnv: []string{"CLAUDE_CODE_USE_BEDROCK=1", "PATH=/usr/bin"},
		refuse:   errors.New("the driver refused the override"),
	}
	rec := &imageEnvRecorder{}

	_, screenErr := screenAndReportSandboxImageEnv(context.Background(), driver, newScreenedSpec("planter/dev:1"), sandbox.RunInfo{}, nil, rec.emit, "run-81")

	var refusal *ImageEnvNeutralisationRefusedError
	if !errors.As(screenErr, &refusal) {
		t.Fatalf("the seam returned %v, want a typed *ImageEnvNeutralisationRefusedError — the boot must not start under a guard that did not take", screenErr)
	}
	if !slices.Equal(refusal.Names, []string{"CLAUDE_CODE_USE_BEDROCK"}) {
		t.Errorf("the refusal names %v, want [CLAUDE_CODE_USE_BEDROCK]", refusal.Names)
	}
	if refusal.Image != "planter/dev:1" || !strings.Contains(refusal.Reason, "refused the override") {
		t.Errorf("the refusal reads %+v, want the image and the driver's own reason", refusal)
	}

	payload := rec.planted(t)
	if got := names(t, payload, "neutralized"); len(got) != 0 {
		t.Errorf("neutralized = %v, want empty: the override was refused", got)
	}
	if got := names(t, payload, "refused_names"); !slices.Equal(got, []string{"CLAUDE_CODE_USE_BEDROCK"}) {
		t.Errorf("refused_names = %v, want [CLAUDE_CODE_USE_BEDROCK] under its own key", got)
	}
	if reason, _ := payload["refused"].(string); !strings.Contains(reason, "refused the override") {
		t.Errorf("refused = %#v, want the driver's own reason", payload["refused"])
	}
	if got := names(t, payload, "reported"); !slices.Equal(got, []string{"PATH"}) {
		t.Errorf("reported = %v, want [PATH] alone — a name nothing started with was never KEPT", got)
	}
	if _, present := payload["unscreened"]; present {
		t.Errorf("unscreened must stay absent: the screen RAN, and a reader must tell a refused guard from a driver that screens nothing: %v", payload)
	}
}

// TestTheImageEnvEventNamesWhatTheHostKnobLetThrough: a name the
// operator allowed keeps its value and is still reported through the
// event, under its own key. The operator asked for the variable, not
// for silence — and the key's presence is how a reader tells an
// allowance from a neutralisation.
func TestTheImageEnvEventNamesWhatTheHostKnobLetThrough(t *testing.T) {
	t.Setenv(EnvSandboxImageEnvAllow, "HTTPS_PROXY")
	driver := &screeningDriver{
		name:     "docker",
		supports: true,
		imageEnv: []string{"HTTPS_PROXY=http://corp:3128", "OPENAI_API_KEY=planted"},
	}
	rec := &imageEnvRecorder{}

	_, _ = screenAndReportSandboxImageEnv(context.Background(), driver, newScreenedSpec("corp/dev:1"), sandbox.RunInfo{}, nil, rec.emit, "run-82")

	payload := rec.planted(t)
	if got := names(t, payload, "allowed"); !slices.Equal(got, []string{"HTTPS_PROXY"}) {
		t.Errorf("allowed = %v, want [HTTPS_PROXY]", got)
	}
	if got := names(t, payload, "neutralized"); !slices.Equal(got, []string{"OPENAI_API_KEY"}) {
		t.Errorf("neutralized = %v, want [OPENAI_API_KEY] — an allowance must not disarm the rest", got)
	}
}

// TestTheImageEnvEventPayloadDoesNotAliasTheScreen: the payload outlives
// the screen inside a store, and a consumer that sorts or appends to a
// list it was handed must not reach back into the value the boot still
// holds.
func TestTheImageEnvEventPayloadDoesNotAliasTheScreen(t *testing.T) {
	screen := imageEnvScreen{
		image:       "planter/dev:1",
		neutralized: []string{"AWS_SECRET_ACCESS_KEY"},
		reported:    []string{"PATH"},
	}

	payload := screen.eventPayload("run-83")
	names(t, payload, "neutralized")[0] = "TAMPERED"
	names(t, payload, "reported")[0] = "TAMPERED"

	if screen.neutralized[0] != "AWS_SECRET_ACCESS_KEY" || screen.reported[0] != "PATH" {
		t.Fatalf("the payload aliases the screen's slices: neutralized=%v reported=%v", screen.neutralized, screen.reported)
	}
}

// TestAStoreThatRefusesTheEventDoesNotStopTheBoot: the event is an
// observation, the neutralisation is the guard. A degraded store must
// not keep a run from starting a container whose planted credentials
// were already overridden — the wrapped emitter warns, which is the
// signal the operator gets.
func TestAStoreThatRefusesTheEventDoesNotStopTheBoot(t *testing.T) {
	t.Setenv(EnvSandboxImageEnvAllow, "")
	driver := &screeningDriver{
		name:     "docker",
		supports: true,
		imageEnv: []string{"CLAUDE_CODE_USE_BEDROCK=1"},
	}
	rec := &imageEnvRecorder{err: errors.New("store is down")}

	started, screenErr := screenAndReportSandboxImageEnv(context.Background(), driver, newScreenedSpec("planter/dev:1"), sandbox.RunInfo{}, nil, rec.emit, "run-84")

	spec, ok := started.(*screenedSpec)
	if !ok || !slices.Equal(spec.Neutralized(), []string{"CLAUDE_CODE_USE_BEDROCK"}) {
		t.Fatalf("a refused event must leave the neutralisation in place, got %#v", started)
	}
	// And the boot is not refused: a degraded store is not a guard that
	// failed, which is the only thing that stops a start.
	if screenErr != nil {
		t.Fatalf("a store that refused the event stopped the boot: %v", screenErr)
	}
}

// TestADoubledPlantedNameIsOneFinding: an image's `Env` is not a set.
// docker's own build collapses a repeated `ENV`, a config written by
// another tool need not, and the event is an audit record — a name
// listed twice reads as two findings, and the operator counts wrong.
func TestADoubledPlantedNameIsOneFinding(t *testing.T) {
	t.Setenv(EnvSandboxImageEnvAllow, "")
	driver := &screeningDriver{
		name:     "docker",
		supports: true,
		imageEnv: []string{
			"CLAUDE_CODE_USE_BEDROCK=1",
			"PATH=/usr/bin",
			"CLAUDE_CODE_USE_BEDROCK=2",
			"PATH=/usr/local/bin",
		},
	}
	rec := &imageEnvRecorder{}

	_, _ = screenAndReportSandboxImageEnv(context.Background(), driver, newScreenedSpec("planter/dev:1"), sandbox.RunInfo{}, nil, rec.emit, "run-85")

	payload := rec.planted(t)
	if got := names(t, payload, "neutralized"); !slices.Equal(got, []string{"CLAUDE_CODE_USE_BEDROCK"}) {
		t.Errorf("neutralized = %v, want the name once", got)
	}
	if got := names(t, payload, "reported"); !slices.Equal(got, []string{"PATH"}) {
		t.Errorf("reported = %v, want the name once", got)
	}
	// And the driver is asked once, not twice.
	if !slices.Equal(driver.asked, []string{"CLAUDE_CODE_USE_BEDROCK"}) {
		t.Errorf("the driver was asked to neutralise %v, want the name once", driver.asked)
	}
}
