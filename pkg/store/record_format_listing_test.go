package store

import (
	"bytes"
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The indexed run listing (RunWindowListingStore) filters and sorts on the
// persisted document, so a field it reads must be stored the same way by
// both value formats: a format that encoded created_at or status differently
// would have its runs silently dropped or misordered by a predicate written
// for the other. The exact codec re-encodes only its own paths (inputs,
// checkpoint, handoff); this pins that every listing key is outside them —
// same BSON type, same bytes — in both formats.
func TestRunListingKeysEncodeIdenticallyInBothValueFormats(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 11, 26, 10, 0, 0, 123_000_000, time.UTC)
	deleted := created.Add(time.Hour)
	r := &Run{
		ID:           "run-listing-keys",
		WorkflowName: "wf",
		Status:       RunStatusFinished,
		TenantID:     "team-a",
		CreatedAt:    created,
		UpdatedAt:    created.Add(time.Minute),
		DeletedAt:    &deleted,
		Inputs:       map[string]any{"pr_url": "https://example.test/pr/1", "n": 3},
		ProjectPath:  "org/repo",
		BundleName:   "bot",
	}
	encode := func(f ValueFormat) bson.Raw {
		t.Helper()
		run := *r
		run.FormatVersion = int(f)
		raw, err := EncodeValueRecordBSON(ctx, f, ExactRun, &run)
		if err != nil {
			t.Fatalf("encode format %d: %v", f, err)
		}
		return raw
	}
	legacy, exact := encode(LegacyValueFormat), encode(ExactValueFormat)
	for _, key := range []string{"_id", "created_at", "updated_at", "status", "workflow_name", "tenant_id", "deleted_at", "project_path", "bundle_name"} {
		l, lerr := legacy.LookupErr(key)
		e, eerr := exact.LookupErr(key)
		if lerr != nil || eerr != nil {
			t.Fatalf("%s: legacy lookup %v, exact lookup %v — both formats must store it", key, lerr, eerr)
		}
		if l.Type != e.Type || !bytes.Equal(l.Value, e.Value) {
			t.Errorf("%s: legacy stores %s %s, exact stores %s %s — a listing predicate would read them differently", key, l.Type, l, e.Type, e)
		}
	}
	if v, _ := exact.LookupErr("created_at"); v.Type != bson.TypeDateTime {
		t.Errorf("created_at is BSON %s, want a date: the listing's range and sort assume one", v.Type)
	}
}
