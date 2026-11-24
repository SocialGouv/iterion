package runtime

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestTheContainerStartsWithTheSpecTheScreenReturned is the witness of the
// DEPARTURE, and the one thing the rest of this package's deterministic
// layer did not hold: what `driver.Start` is handed.
//
// Every other witness reads the seam's return value or the event. A boot
// that CALLS the seam — so the event fires, carrying a perfectly shaped
// `neutralized: […]` an operator would read as a closed guard — and then
// hands Start the PRE-screen spec satisfies all of them, while `docker
// run` creates the container with the planter's routing URL and keys. It
// is exactly the state the single seam exists to make impossible, and one
// discarded assignment away.
//
// So this drives the REAL boot — resolveAndStartSandbox — records the spec
// that reached Start, and asks the question at the departure: does the
// container start with the neutralisation?
//
// The second half is the new contract (#2317 round 5): this fixture's
// driver answers Start with an error, so NO container exists, and the
// record the boot publishes must claim nothing about one. The
// dispositions of this event are measurements of a container's own
// environment; here there is none, and the honest record is the names the
// IMAGE plants plus the reason nothing could be measured.
func TestTheContainerStartsWithTheSpecTheScreenReturned(t *testing.T) {
	driver, events, err := bootOnPlantedImage(t, nil)

	// Start is where this witness reads its answer, so a boot that never
	// got there proves nothing either way.
	if !errors.Is(err, errStartReached) {
		t.Fatalf("the boot returned %v, want the driver's own Start marker: this witness reads the spec Start was handed, and a boot that stopped earlier hands it nothing", err)
	}
	spec, ok := driver.startSpec.(*screenedSpec)
	if !ok {
		t.Fatalf("driver.Start was handed %#v, want this driver's own prepared spec", driver.startSpec)
	}
	startedWith := spec.Neutralized()
	// The bookkeeping list is not the container's env: what `docker run`
	// turns into `--env NAME=` is the spec's env MAP, and a producer that
	// reported the right names and wrote nothing would satisfy a witness
	// reading the list alone. So both, off the same recorded start.
	startedEnv := spec.Spec().Env
	for _, name := range startedWith {
		if v, present := startedEnv[name]; !present || v != "" {
			t.Errorf("the spec handed to Start says it neutralised %s and its env has it = %q (present %v): the argv is built from that map, so the container would be created with the image's value", name, v, present)
		}
	}

	// The planted ROUTE first, by name: #2317 is the failure where a
	// repository-authored image re-points every model call of the run.
	if !slices.Contains(startedWith, "ANTHROPIC_BASE_URL") {
		t.Errorf("the container was CREATED while the image's planted ANTHROPIC_BASE_URL still rode — the spec handed to Start neutralises %v: every prompt of this run goes to the planter's collector", startedWith)
	}
	wantNeutralized := []string{"ANTHROPIC_BASE_URL", "AWS_SECRET_ACCESS_KEY", "CLAUDE_CODE_USE_BEDROCK"}
	if !slices.Equal(startedWith, wantNeutralized) {
		t.Errorf("the spec handed to Start neutralises %v, want %v — the whole credential and routing sub-class the image plants", startedWith, wantNeutralized)
	}
	// And the REPORTED half is not in it: emptying PATH would leave the
	// container unable to resolve a command, which is why that half is
	// told and not overridden.
	if slices.Contains(startedWith, "PATH") {
		t.Errorf("the spec handed to Start neutralises PATH (%v): the container would resolve no command", startedWith)
	}

	// The record the operator reads is published by the BOOT — which is
	// how this witness proves the boot goes through the screen's seam at
	// all — and it describes what it can: the names the image plants.
	payload := plantedPayload(t, events)
	if got := payload["image"]; got != "ghcr.io/planter/dev:1" {
		t.Errorf("the event names image %v, want the one the run started from", got)
	}
	wantPlanted := append(slices.Clone(wantNeutralized), "PATH")
	slices.Sort(wantPlanted)
	if got := names(t, payload, "planted"); !slices.Equal(got, wantPlanted) {
		t.Errorf("planted = %v, want %v — what the IMAGE bakes is the fact that does not depend on a container", got, wantPlanted)
	}
	// No container was HANDED BACK, so not one disposition may travel:
	// each of them is a claim about a container's own environment. A
	// record that published `neutralized: [ANTHROPIC_BASE_URL …]` here
	// would be describing the spec it HOPED for, which is precisely the
	// kind of claim `docker inspect` contradicted twice. The wording
	// holds to what is known: Start can fail in a postCreate command —
	// a container the driver then removes — so "no container was
	// created" is a claim this record cannot make.
	undeduced, _ := payload["undeduced"].(string)
	if !strings.Contains(undeduced, "no container this driver could hand back") {
		t.Errorf("undeduced = %q, want the reason stated to what is known: Start failed, so no container was handed back — whether one was created and removed is the error's own fact to carry", undeduced)
	}
	for _, key := range []string{"neutralized", "reported", "allowed", "operator_set", "driver_set", "unexplained"} {
		if _, present := payload[key]; present {
			t.Errorf("%s travels on a boot where Start failed: %v — the dispositions are measurements of a container, and this run has none", key, payload[key])
		}
	}
	// A started sandbox is the other half of the record: the refusal arm
	// of TestTheBootRefusesToStartWhenANeutralisationIsRefused asserts
	// this event's ABSENCE, so a fixture where it never fires would make
	// that assertion vacuous.
	if !eventRecorded(events, store.EventSandboxImageEnvPlanted) {
		t.Errorf("no %s event on a boot that screened a planted image", store.EventSandboxImageEnvPlanted)
	}
}
