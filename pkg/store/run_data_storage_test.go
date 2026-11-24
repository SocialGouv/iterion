package store

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestRunDataStorageRefusesOpaqueProtocolBeforePayload(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		protocol string
		want     error
	}{{"1", ErrRunDataCapability}, {"99", ErrUnsupportedRunDataProtocol}} {
		r, err := s.CreateRun(t.Context(), "r"+tc.protocol, "wf", nil)
		if err != nil {
			t.Fatal(err)
		}
		raw := []byte(`{"id":"` + r.ID + `","birth_id":"` + r.BirthID + `","data_protocol":` + tc.protocol + `,"inputs":{"n":1e400}}`)
		if err := os.WriteFile(s.runJSONPath(r.ID), raw, filePerm); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteArtifact(t.Context(), &Artifact{RunID: r.ID, NodeID: "refused", Version: 1}); !errors.Is(err, tc.want) {
			t.Fatalf("sidecar write protocol %s: %v", tc.protocol, err)
		}
		if _, err := s.LoadArtifact(t.Context(), r.ID, "refused", 1); !errors.Is(err, tc.want) {
			t.Fatalf("sidecar read protocol %s: %v", tc.protocol, err)
		}
		if _, err := s.LoadRun(t.Context(), r.ID); !errors.Is(err, tc.want) {
			t.Fatalf("protocol %s decoded payload: %v", tc.protocol, err)
		}
		r.Name = "stale snapshot"
		if err := s.SaveRun(t.Context(), r); !errors.Is(err, tc.want) {
			t.Fatalf("protocol %s overwritten: %v", tc.protocol, err)
		}
		after, err := os.ReadFile(s.runJSONPath(r.ID))
		if err != nil || string(after) != string(raw) {
			t.Fatalf("refusal changed durable row: %v", err)
		}
	}
}

func TestRunDataStorageCannotEnterThroughSnapshotReplacement(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(t.Context(), "r", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []RunDataProtocol{IncarnationRunData, -1, 2} {
		proposed := *r
		proposed.DataProtocol = protocol
		want := ErrUnsupportedRunDataProtocol
		if protocol == IncarnationRunData {
			want = ErrRunDataCapability
		}
		if err := s.SaveRun(t.Context(), &proposed); !errors.Is(err, want) {
			t.Fatalf("snapshot protocol %d: %v", protocol, err)
		}
	}
	got, err := s.LoadRun(t.Context(), r.ID)
	if err != nil || got.DataProtocol != HistoricalRunData || got.CASVersion != r.CASVersion {
		t.Fatalf("refusal altered historical row: %+v %v", got, err)
	}
	r.Name = "legitimate historical save"
	if err := s.SaveRun(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadRun(t.Context(), r.ID)
	if err != nil || got.Name != r.Name {
		t.Fatal(got, err)
	}
}

func TestRunDataSnapshotBindingDoesNotTurnFormatIntoDataIdentity(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(t.Context(), "parent", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	childCtx, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: "child", Format: LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadRun(childCtx, r.ID); err != nil {
		t.Fatalf("historical child preparation cannot read parent: %v", err)
	}
	binding, err := CaptureRunDataBinding(RunDataHeader{RunID: r.ID, BirthID: r.BirthID + "-stale", Format: LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := WithRunDataBinding(t.Context(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadRun(stale, r.ID); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("full read adopted current birth: %v", err)
	}
}

// Exercise identity separately from the activation fence: these assertions
// remain necessary when complete backend capability eventually permits one.
func TestRunDataReplacementIdentitySurvivesCapabilityActivation(t *testing.T) {
	t.Parallel()
	current := &Run{ID: "r", BirthID: "birth-a", DataProtocol: IncarnationRunData, FormatVersion: 2}
	binding, err := CaptureRunDataBinding(RunDataHeader{RunID: current.ID, BirthID: current.BirthID, Protocol: current.DataProtocol, Format: ExactValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := WithRunDataBinding(t.Context(), binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		current  *Run
		incoming Run
		want     error
	}{
		{"same incarnation", bound, current, *current, nil},
		{"different run", bound, current, Run{ID: "other", BirthID: "birth-a", DataProtocol: IncarnationRunData, FormatVersion: 2}, ErrRunDataIdentityChanged},
		{"missing original binding", t.Context(), current, *current, ErrRunDataBindingRequired},
		{"snapshot birth", bound, nil, *current, ErrRunDataCapability},
		{"protocol downgrade", bound, current, Run{ID: "r", BirthID: "birth-a", FormatVersion: 2}, ErrRunDataIdentityChanged},
		{"protocol upgrade", bound, &Run{ID: "r", BirthID: "birth-a", FormatVersion: 2}, *current, ErrRunDataIdentityChanged},
		{"empty birth", bound, current, Run{ID: "r", DataProtocol: IncarnationRunData, FormatVersion: 2}, ErrRunDataIdentityChanged},
		{"successor birth", bound, current, Run{ID: "r", BirthID: "birth-b", DataProtocol: IncarnationRunData, FormatVersion: 2}, ErrRunDataIdentityChanged},
		{"historical omitted birth", t.Context(), &Run{ID: "old", BirthID: "old-birth"}, Run{ID: "old"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckRunDataReplacement(tc.ctx, tc.current, &tc.incoming); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRunDataSnapshotSaveRetainsOriginalBinding(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(t.Context(), "bound-save", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := CaptureRunDataBinding(RunDataHeader{RunID: r.ID, BirthID: r.BirthID + "-old", Format: LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := WithRunDataBinding(t.Context(), binding)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.runJSONPath(r.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRun(ctx, r); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("stale bound snapshot accepted: %v", err)
	}
	after, err := os.ReadFile(s.runJSONPath(r.ID))
	if err != nil || string(before) != string(after) {
		t.Fatalf("refusal mutated row: %v", err)
	}
}

func TestRunDataSidecarRetainsOriginalBinding(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	r, err := s.CreateRun(ctx, "sidecar", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact := &Artifact{RunID: r.ID, NodeID: "node", Version: 1, Data: map[string]any{"value": "original"}}
	if err := s.WriteArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []bool{false, true} {
		birth := r.BirthID
		if stale {
			birth += "-old"
		}
		binding, err := CaptureRunDataBinding(RunDataHeader{RunID: r.ID, BirthID: birth, Format: LegacyValueFormat})
		if err != nil {
			t.Fatal(err)
		}
		bound, err := WithRunDataBinding(ctx, binding)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.LoadArtifact(bound, r.ID, artifact.NodeID, 1)
		if !stale {
			if err != nil || got.Data["value"] != "original" {
				t.Fatalf("valid binding refused: %v %v", got, err)
			}
			continue
		}
		if !errors.Is(err, ErrRunDataIdentityChanged) {
			t.Fatalf("stale sidecar read: %v", err)
		}
		next := *artifact
		next.Data = map[string]any{"value": "stale"}
		if err := s.WriteArtifact(bound, &next); !errors.Is(err, ErrRunDataIdentityChanged) {
			t.Fatalf("stale sidecar write: %v", err)
		}
	}
	got, err := s.LoadArtifact(ctx, r.ID, artifact.NodeID, 1)
	if err != nil || got.Data["value"] != "original" {
		t.Fatalf("refusal altered payload: %v %v", got, err)
	}
}
