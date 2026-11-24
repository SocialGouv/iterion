package store

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/SocialGouv/iterion/pkg/recordcodec"
)

// ExactRecordKind names an explicitly owned record registry. Merely obtaining
// a codec does not select a durable format or activate a store writer.
type ExactRecordKind string

const (
	ExactRun                ExactRecordKind = "run"
	ExactCheckpoint         ExactRecordKind = "checkpoint"
	ExactInteraction        ExactRecordKind = "interaction"
	ExactEvent              ExactRecordKind = "event"
	ExactArtifact           ExactRecordKind = "artifact"
	ExactResumePauseHandoff ExactRecordKind = "resume_pause_handoff"
)

var exactCheckpointPaths = []string{"Outputs", "LoopPreviousOutput", "LoopCurrentOutput", "Artifacts", "Vars", "InteractionQuestions", "FiredEvents", "Parallel/PendingInteractionQuestions", "Parallel/Branches/*/Outputs", "Parallel/Branches/*/Artifacts", "Parallel/Branches/*/LoopPreviousOutput", "Parallel/Branches/*/LoopCurrentOutput", "Parallel/Branches/*/ResumeAnswers"}
var exactRecordPlans = sync.OnceValues(func() (map[ExactRecordKind]recordcodec.Codec, error) {
	handoffPaths := []string{"InteractionQuestions"}
	runPaths := []string{"Inputs"}
	for _, p := range handoffPaths {
		runPaths = append(runPaths, "ResumePauseHandoff/"+p)
	}
	for _, p := range exactCheckpointPaths {
		runPaths = append(runPaths, "Checkpoint/"+p)
	}
	entries := []struct {
		kind  ExactRecordKind
		typ   reflect.Type
		paths []string
	}{
		{ExactRun, reflect.TypeFor[Run](), runPaths}, {ExactCheckpoint, reflect.TypeFor[Checkpoint](), exactCheckpointPaths},
		{ExactInteraction, reflect.TypeFor[Interaction](), []string{"Questions", "Answers", "Turns/[]/Verdict"}},
		{ExactEvent, reflect.TypeFor[Event](), []string{"Data"}}, {ExactArtifact, reflect.TypeFor[Artifact](), []string{"Data"}},
		{ExactResumePauseHandoff, reflect.TypeFor[ResumePauseHandoff](), handoffPaths},
	}
	out := map[ExactRecordKind]recordcodec.Codec{}
	for _, entry := range entries {
		p, err := recordcodec.Compile(entry.typ, entry.paths...)
		if err != nil {
			return nil, err
		}
		out[entry.kind] = p
		if entry.kind == ExactEvent {
			out[entry.kind] = eventRecordCodec{p}
		}
	}
	return out, nil
})

// ExactRecordCodec returns a concurrent, immutable codec for the named record.
// The persistence owner must validate its record format before using it.
func ExactRecordCodec(kind ExactRecordKind) (recordcodec.Codec, error) {
	plans, err := exactRecordPlans()
	if err != nil {
		return nil, err
	}
	p, ok := plans[kind]
	if !ok {
		return nil, fmt.Errorf("unknown exact record kind %q", kind)
	}
	return p, nil
}
