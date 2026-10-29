package runtime

import (
	"errors"
	"slices"
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
// So this drives the REAL boot, records the spec that reached Start, and
// asks the question at the departure: does the container start with the
// neutralisation, and does the event the operator reads AGREE with it?
// The agreement is the assertion that matters — a guard whose two halves
// can disagree is a guard nobody can audit.
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

	// The event an operator reads must describe THAT spec. Compared
	// against the recorded start rather than against a literal: a
	// hard-coded list would pass while the two drifted apart, which is
	// the audit failure this seam was made one function to prevent.
	payload := plantedPayload(t, events)
	if got := names(t, payload, "neutralized"); !slices.Equal(got, startedWith) {
		t.Errorf("the event says %v was neutralised and the container STARTED with %v — the operator reads the event, the container runs the spec, and a boot where the two can disagree is a guard nobody can audit", got, startedWith)
	}
	if got := names(t, payload, "reported"); !slices.Equal(got, []string{"PATH"}) {
		t.Errorf("reported = %v, want [PATH]: the kept half decides whether such an image belongs in the run at all", got)
	}
	if got := payload["image"]; got != "ghcr.io/planter/dev:1" {
		t.Errorf("the event names image %v, want the one the run started from", got)
	}
	// A started sandbox is the other half of the record: the refusal arm
	// of TestTheBootRefusesToStartWhenANeutralisationIsRefused asserts
	// this event's ABSENCE, so a fixture where it never fires would make
	// that assertion vacuous.
	if !eventRecorded(events, store.EventSandboxImageEnvPlanted) {
		t.Errorf("no %s event on a boot that screened a planted image", store.EventSandboxImageEnvPlanted)
	}
}
