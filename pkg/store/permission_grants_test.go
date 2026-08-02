package store

import (
	"context"
	"reflect"
	"testing"
)

// The grant set has to survive the run record, because every
// permission-gate pause is resumed by a fresh engine that re-seeds from
// it. A SaveRun between the patch and the reload would clobber it — this
// pins that it does not.
func TestPatchRunPermissionGrantsRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateRun(ctx, "run-grants", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if got, err := s.LoadRun(ctx, "run-grants"); err != nil {
		t.Fatalf("LoadRun: %v", err)
	} else if len(got.PermissionGrants) != 0 {
		t.Fatalf("a fresh run already holds grants: %#v", got.PermissionGrants)
	}

	want := []string{"Write", "Bash(git add:*)"}
	if err := s.PatchRunPermissionGrants(ctx, "run-grants", want); err != nil {
		t.Fatalf("PatchRunPermissionGrants: %v", err)
	}
	got, err := s.LoadRun(ctx, "run-grants")
	if err != nil {
		t.Fatalf("LoadRun after patch: %v", err)
	}
	if !reflect.DeepEqual(got.PermissionGrants, want) {
		t.Errorf("PermissionGrants = %#v, want %#v", got.PermissionGrants, want)
	}

	// A nil slice is a no-op patch, not an erasure: the steering patches
	// next door use the same convention.
	if err := s.PatchRunPermissionGrants(ctx, "run-grants", nil); err != nil {
		t.Fatalf("no-op patch: %v", err)
	}
	if got, err := s.LoadRun(ctx, "run-grants"); err != nil {
		t.Fatalf("LoadRun after no-op: %v", err)
	} else if !reflect.DeepEqual(got.PermissionGrants, want) {
		t.Errorf("a nil patch erased the grants: %#v", got.PermissionGrants)
	}

	if err := s.PatchRunPermissionGrants(ctx, "no-such-run", want); err == nil {
		t.Error("patching an unknown run returned no error")
	}
}
