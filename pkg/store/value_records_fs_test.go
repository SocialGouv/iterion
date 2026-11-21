package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/valuecodec"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func valuePayload() map[string]any {
	return map[string]any{"i": int64(9007199254740993), "n": json.Number("1e400"), "tiny": json.Number("1e-4000"), "bytes": []byte{0, 255}, "empty": []any{}, "nil": []any(nil)}
}
func requireValuePayload(t *testing.T, got map[string]any) {
	t.Helper()
	want, e := valuecodec.CanonicalJSON(t.Context(), valuePayload())
	if e != nil {
		t.Fatal(e)
	}
	actual, e := valuecodec.CanonicalJSON(t.Context(), got)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(actual, want) {
		t.Fatalf("payload lost: %#v", got)
	}
}
func valueFS(t *testing.T) (*FilesystemRunStore, context.Context) {
	t.Helper()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, err := s.CreateRun(t.Context(), "parent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, e = s.CreateRunWithValueFormat(t.Context(), exactBirthSpec("r")); e != nil {
		t.Fatal(e)
	}
	ctx, e := WithRunValueFormat(t.Context(), RunValueFormat{"r", 2})
	if e != nil {
		t.Fatal(e)
	}
	return s, ctx
}
func TestValueRecordsFilesystemRoundtrip(t *testing.T) {
	t.Parallel()
	s, ctx := valueFS(t)
	if e := s.WriteArtifact(ctx, &Artifact{RunID: "r", NodeID: "n", Version: 1, Data: valuePayload()}); e != nil {
		t.Fatal(e)
	}
	for _, load := range []func() (*Artifact, error){func() (*Artifact, error) { return s.LoadArtifact(ctx, "r", "n", 1) }, func() (*Artifact, error) { return s.LoadLatestArtifact(ctx, "r", "n") }} {
		a, e := load()
		if e != nil {
			t.Fatal(e)
		}
		requireValuePayload(t, a.Data)
	}
	if vs, e := s.ListArtifactVersions(ctx, "r", "n"); e != nil || len(vs) != 1 || vs[0].Version != 1 {
		t.Fatal(vs, e)
	}
	if _, e := s.AppendEvent(ctx, "r", Event{Type: "value_test", Data: valuePayload()}); e != nil {
		t.Fatal(e)
	}
	es, e := s.LoadEvents(ctx, "r")
	if e != nil || len(es) != 1 {
		t.Fatal(es, e)
	}
	requireValuePayload(t, es[0].Data)
	// Visitors may call writers: ScanEvents must release the stable lock before visit.
	if e = s.ScanEvents(ctx, "r", func(*Event) bool {
		if e := s.WriteArtifact(ctx, &Artifact{RunID: "r", NodeID: "from-visitor", Version: 1, Data: valuePayload()}); e != nil {
			t.Error(e)
		}
		return false
	}); e != nil {
		t.Fatal(e)
	}
	if e = s.WriteInteraction(ctx, &Interaction{RunID: "r", ID: "q", Questions: valuePayload()}); e != nil {
		t.Fatal(e)
	}
	i, e := AnswerInteraction(ctx, s, "r", "q", valuePayload())
	if e != nil {
		t.Fatal(e)
	}
	requireValuePayload(t, i.Answers)
	i, e = s.LoadInteraction(ctx, "r", "q")
	if e != nil {
		t.Fatal(e)
	}
	requireValuePayload(t, i.Questions)
	requireValuePayload(t, i.Answers)
	for _, write := range []func(*Checkpoint) error{func(cp *Checkpoint) error { return s.SaveCheckpoint(ctx, "r", cp) }, func(cp *Checkpoint) error { return s.PauseRun(ctx, "r", cp) }, func(cp *Checkpoint) error { return s.FailRunResumable(ctx, "r", cp, "fail", FailureCode("")) }, func(cp *Checkpoint) error { return s.FailRunTerminal(ctx, "r", cp, "fail", FailureCode("")) }} {
		if e = write(&Checkpoint{Outputs: map[string]map[string]any{"n": valuePayload()}, Vars: valuePayload()}); e != nil {
			t.Fatal(e)
		}
		r, e := s.LoadRun(ctx, "r")
		if e != nil {
			t.Fatal(e)
		}
		requireValuePayload(t, r.Checkpoint.Outputs["n"])
		requireValuePayload(t, r.Checkpoint.Vars)
	}
}
func TestValueRecordsFilesystemRefusalNoEffects(t *testing.T) {
	t.Parallel()
	s, exact := valueFS(t)
	if e := s.WriteInteraction(exact, &Interaction{RunID: "r", ID: "q", Questions: valuePayload()}); e != nil {
		t.Fatal(e)
	}
	writers := map[string]func(context.Context) error{
		"artifact": func(ctx context.Context) error {
			return s.WriteArtifact(ctx, &Artifact{RunID: "r", NodeID: "bad", Version: 1, Data: valuePayload()})
		},
		"event": func(ctx context.Context) error {
			_, e := s.AppendEvent(ctx, "r", Event{Type: "bad", Data: valuePayload()})
			return e
		},
		"interaction": func(ctx context.Context) error {
			return s.WriteInteraction(ctx, &Interaction{RunID: "r", ID: "bad", Questions: valuePayload()})
		},
		"answer": func(ctx context.Context) error {
			_, e := s.AnswerInteractionCAS(ctx, "r", "q", valuePayload())
			return e
		},
		"checkpoint": func(ctx context.Context) error {
			return s.SaveCheckpoint(ctx, "r", &Checkpoint{Outputs: map[string]map[string]any{"n": valuePayload()}})
		},
		"pause": func(ctx context.Context) error {
			return s.PauseRun(ctx, "r", &Checkpoint{Outputs: map[string]map[string]any{"n": valuePayload()}})
		},
		"fail-resumable": func(ctx context.Context) error {
			return s.FailRunResumable(ctx, "r", &Checkpoint{Outputs: map[string]map[string]any{"n": valuePayload()}}, "bad", "")
		},
		"fail-terminal": func(ctx context.Context) error {
			return s.FailRunTerminal(ctx, "r", &Checkpoint{Outputs: map[string]map[string]any{"n": valuePayload()}}, "bad", "")
		},
	}
	snapshot := func() map[string]string {
		out := map[string]string{}
		e := filepath.WalkDir(s.runDir("r"), func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if !d.IsDir() {
				b, e := os.ReadFile(p)
				if e != nil {
					return e
				}
				out[p] = string(b)
			}
			return nil
		})
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	before := snapshot()
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			if e := write(t.Context()); !errors.Is(e, ErrValueFormatChanged) {
				t.Fatalf("missing binding accepted: %v", e)
			}
			ctx, cancel := context.WithCancel(exact)
			cancel()
			if e := write(ctx); !errors.Is(e, context.Canceled) {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(before, snapshot()) {
				t.Fatal("refusal changed payload namespace")
			}
		})
	}
}
func TestValueRecordsFilesystemInterformatSequence(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	other, e := New(s.root)
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range []ValueFormat{1, 2, 1} {
		if _, e = s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", Format: f}); e != nil {
			t.Fatal(e)
		}
		ctx, e := WithRunValueFormat(t.Context(), RunValueFormat{"r", f})
		if e != nil {
			t.Fatal(e)
		}
		for j := int64(0); j < 2; j++ {
			got, e := s.AppendEvent(ctx, "r", Event{Type: "sequence", Data: map[string]any{"s": "v"}})
			if e != nil || got.Seq != j {
				t.Fatalf("format %d seq %v %v", f, got, e)
			}
		}
		if e = other.DeleteRun(t.Context(), "r"); e != nil {
			t.Fatal(e)
		}
		if n, e := other.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); e != nil || n != 1 {
			t.Fatal(n, e)
		}
	}
}
func TestValueRecordsFilesystemOrphansAndAnswerRace(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"event", "interaction"} {
		t.Run(kind, func(t *testing.T) {
			s, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			if _, err := s.CreateRun(t.Context(), "parent", "wf", nil); err != nil {
				t.Fatal(err)
			}
			if kind == "event" {
				_, e = s.AppendEvent(t.Context(), "r", Event{Type: "orphan"})
			} else {
				e = s.WriteInteraction(t.Context(), &Interaction{RunID: "r", ID: "q"})
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.CreateRunWithValueFormat(t.Context(), exactBirthSpec("r")); !errors.Is(e, ErrInvalidValueFormat) {
				t.Fatal(e)
			}
		})
	}
	s, ctx := valueFS(t)
	other, e := New(s.root)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.WriteInteraction(ctx, &Interaction{RunID: "r", ID: "q"}); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, st := range []*FilesystemRunStore{s, other} {
		wg.Go(func() { _, e := st.AnswerInteractionCAS(ctx, "r", "q", valuePayload()); errs <- e })
	}
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for e := range errs {
		if e == nil {
			wins++
		} else if errors.Is(e, ErrInteractionAlreadyAnswered) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
}
func TestValueRecordsFilesystemFutureAndMalformedAreTerminal(t *testing.T) {
	t.Parallel()
	s, ctx := valueFS(t)
	if e := os.WriteFile(s.eventsPath("r"), []byte("{\"seq\":0,\"data\":{}}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if es, e := s.LoadEvents(ctx, "r"); e == nil || len(es) != 0 {
		t.Fatal("missing frame accepted", es, e)
	}
	r, e := s.LoadRun(ctx, "r")
	if e != nil {
		t.Fatal(e)
	}
	r.FormatVersion = 99
	raw, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(s.runJSONPath("r"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	for _, read := range []func() error{func() error { _, e := s.LoadLatestArtifact(ctx, "r", "n"); return e }, func() error { _, e := s.LoadArtifact(ctx, "r", "n", 1); return e }, func() error { _, e := s.ListArtifactVersions(ctx, "r", "n"); return e }, func() error { _, e := s.LoadEvents(ctx, "r"); return e }, func() error { _, e := s.LoadInteraction(ctx, "r", "q"); return e }} {
		if e = read(); !errors.Is(e, ErrUnsupportedValueFormat) {
			t.Fatal("future swallowed", e)
		}
	}
}

// A legacy callback probes the actual stable-lock inode synchronously: no
// timing or scheduler assumptions. Exact mode refuses callbacks separately.
type valueLockWitness struct{ path string }

func (v valueLockWitness) MarshalJSON() ([]byte, error) {
	lock, err := AcquireFileLock(v.path, "witness")
	if err == nil {
		_ = lock.Unlock()
		return nil, fmt.Errorf("publication lock absent")
	}
	return []byte(`1`), nil
}
func TestValueRecordsFilesystemStablePublicationLock(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRun(t.Context(), "r", "w", nil); e != nil {
		t.Fatal(e)
	}
	payload := map[string]any{"n": valueLockWitness{filepath.Join(s.root, ".run-write-locks", "r")}}
	if e = s.WriteInteraction(t.Context(), &Interaction{RunID: "r", ID: "q"}); e != nil {
		t.Fatal(e)
	}
	writers := []func() error{
		func() error {
			return s.WriteArtifact(t.Context(), &Artifact{RunID: "r", NodeID: "n", Version: 1, Data: payload})
		},
		func() error { _, e := s.AppendEvent(t.Context(), "r", Event{Type: "witness", Data: payload}); return e },
		func() error {
			return s.WriteInteraction(t.Context(), &Interaction{RunID: "r", ID: "q2", Questions: payload})
		},
		func() error { _, e := s.AnswerInteractionCAS(t.Context(), "r", "q", payload); return e },
		func() error { return s.SaveCheckpoint(t.Context(), "r", &Checkpoint{Vars: payload}) },
		func() error { return s.PauseRun(t.Context(), "r", &Checkpoint{Vars: payload}) },
		func() error { return s.FailRunResumable(t.Context(), "r", &Checkpoint{Vars: payload}, "fail", "") },
		func() error { return s.FailRunTerminal(t.Context(), "r", &Checkpoint{Vars: payload}, "fail", "") },
	}
	for n, w := range writers {
		if e = w(); e != nil {
			t.Fatalf("writer %d stable lock: %v", n, e)
		}
	}
}
