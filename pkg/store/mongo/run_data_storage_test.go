package mongo

import (
	"bytes"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRunDataSnapshotProtocolRefusesBeforeMongoIO(t *testing.T) {
	t.Parallel()
	for _, protocol := range []store.RunDataProtocol{store.IncarnationRunData, -1, 2} {
		want := store.ErrUnsupportedRunDataProtocol
		if protocol == store.IncarnationRunData {
			want = store.ErrRunDataCapability
		}
		if err := (&Store{}).SaveRun(t.Context(), &store.Run{ID: "r", DataProtocol: protocol}); !errors.Is(err, want) {
			t.Fatalf("protocol %d: %v", protocol, err)
		}
	}
}

func TestRunDataStoredProtocolRefusesMongoReadAndReplacement(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	r, err := s.CreateRun(ctx, "r", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	childCtx, err := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "child", Format: store.LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadRun(childCtx, r.ID); err != nil {
		t.Fatalf("historical parent read: %v", err)
	}
	binding, err := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, BirthID: r.BirthID + "-stale", Format: store.LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.WithRunDataBinding(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadRun(stale, r.ID); !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("full read adopted current birth: %v", err)
	}
	before, err := s.runs.FindOne(ctx, bson.M{"_id": r.ID}).Raw()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRun(stale, r); !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("stale snapshot accepted: %v", err)
	}
	after, err := s.runs.FindOne(ctx, bson.M{"_id": r.ID}).Raw()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("refused snapshot changed row: %v", err)
	}
	for _, protocol := range []int{1, 99} {
		_, err = s.runs.UpdateOne(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"data_protocol": protocol}})
		if err != nil {
			t.Fatal(err)
		}
		want := store.ErrUnsupportedRunDataProtocol
		if protocol == 1 {
			want = store.ErrRunDataCapability
		}
		if err := s.WriteArtifact(ctx, &store.Artifact{RunID: r.ID, NodeID: "refused", Version: 1}); !errors.Is(err, want) {
			t.Fatalf("sidecar write protocol %d: %v", protocol, err)
		}
		if _, err := s.LoadArtifact(ctx, r.ID, "refused", 1); !errors.Is(err, want) {
			t.Fatalf("sidecar read protocol %d: %v", protocol, err)
		}
		if _, err = s.LoadRun(ctx, r.ID); !errors.Is(err, want) {
			t.Fatalf("read protocol %d: %v", protocol, err)
		}
		if err = s.SaveRun(ctx, r); !errors.Is(err, want) {
			t.Fatalf("replacement protocol %d: %v", protocol, err)
		}
		raw, err := s.runs.FindOne(ctx, bson.M{"_id": r.ID}).Raw()
		if err != nil {
			t.Fatal(err)
		}
		if raw.Lookup("data_protocol").Int32() != int32(protocol) {
			t.Fatal("replacement stripped protocol")
		}
	}
}

func TestRunDataSidecarRetainsOriginalBinding(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	r, err := s.CreateRun(ctx, "sidecar", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact := &store.Artifact{RunID: r.ID, NodeID: "node", Version: 1, Data: map[string]any{"value": "original"}}
	if err := s.WriteArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []bool{false, true} {
		birth := r.BirthID
		if stale {
			birth += "-old"
		}
		binding, err := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, BirthID: birth, Format: store.LegacyValueFormat})
		if err != nil {
			t.Fatal(err)
		}
		bound, err := store.WithRunDataBinding(ctx, binding)
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
		if !errors.Is(err, store.ErrRunDataIdentityChanged) {
			t.Fatalf("stale sidecar read: %v", err)
		}
		next := *artifact
		next.Data = map[string]any{"value": "stale"}
		if err := s.WriteArtifact(bound, &next); !errors.Is(err, store.ErrRunDataIdentityChanged) {
			t.Fatalf("stale sidecar write: %v", err)
		}
	}
	got, err := s.LoadArtifact(ctx, r.ID, artifact.NodeID, 1)
	if err != nil || got.Data["value"] != "original" {
		t.Fatalf("refusal altered payload: %v %v", got, err)
	}
}
