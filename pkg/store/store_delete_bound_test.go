package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rewriteRunJSON applies edit to id's run.json as a JSON object.
func rewriteRunJSON(t *testing.T, s *FilesystemRunStore, id string, edit func(m map[string]json.RawMessage)) {
	t.Helper()
	raw, err := os.ReadFile(s.runJSONPath(id))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.runJSONPath(id), out, filePerm); err != nil {
		t.Fatal(err)
	}
}

// TestBoundDeleteRemovesTheRunItObserved: a delete bound to the run its
// caller observed removes it and leaves its tombstone, whatever the run's
// legitimate shape — a tenant's run, an exact-format run, a historical run
// (no birth, or an explicit empty one), a run.json a failed cleanup retained
// beside its marker.
func TestBoundDeleteRemovesTheRunItObserved(t *testing.T) {
	t.Parallel()
	create := func(t *testing.T, s *FilesystemRunStore, ctx context.Context, id string) {
		t.Helper()
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		tenant string
		shape  func(t *testing.T, s *FilesystemRunStore, ctx context.Context, id string)
	}{
		{name: "legacy", shape: create},
		{name: "tenant", tenant: "team-a", shape: func(t *testing.T, s *FilesystemRunStore, ctx context.Context, id string) {
			create(t, s, ctx, id)
			rewriteRunJSON(t, s, id, func(m map[string]json.RawMessage) { m["tenant_id"] = json.RawMessage(`"team-a"`) })
		}},
		{name: "exact", shape: func(t *testing.T, s *FilesystemRunStore, ctx context.Context, id string) {
			if _, err := s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: id, WorkflowName: "wf", Format: ExactValueFormat}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "historical", shape: func(t *testing.T, s *FilesystemRunStore, ctx context.Context, id string) {
			create(t, s, ctx, id)
			rewriteRunJSON(t, s, id, func(m map[string]json.RawMessage) { delete(m, "birth_id") })
		}},
		{name: "explicit-empty-birth", shape: func(t *testing.T, s *FilesystemRunStore, ctx context.Context, id string) {
			create(t, s, ctx, id)
			rewriteRunJSON(t, s, id, func(m map[string]json.RawMessage) { m["birth_id"] = json.RawMessage(`""`) })
		}},
		{name: "run-json-retained-beside-marker", shape: func(t *testing.T, s *FilesystemRunStore, ctx context.Context, id string) {
			create(t, s, ctx, id)
			if err := os.WriteFile(filepath.Join(s.runDir(id), deletionMarkerName), []byte("interrupted\n"), filePerm); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if tc.tenant != "" {
				ctx = WithIdentity(ctx, tc.tenant, "user")
			}
			const id = "r"
			tc.shape(t, s, ctx, id)
			// A load refuses a tombstoned run: the observation is the raw
			// header and stored tenant the caller read before the marker.
			raw, err := os.ReadFile(s.runJSONPath(id))
			if err != nil {
				t.Fatal(err)
			}
			header, err := RunDataHeaderFromJSON(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			var stored struct {
				TenantID string `json:"tenant_id"`
			}
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.TenantID != tc.tenant {
				t.Fatalf("fixture: stored tenant %q, want %q", stored.TenantID, tc.tenant)
			}
			o, err := CaptureRunObservation(header, stored.TenantID, true)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := BindRunObservation(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteRun(bound, id); err != nil {
				t.Fatalf("bound delete of the run it observed (%s) = %v, want nil", tc.name, err)
			}
			if _, err := os.Stat(s.runJSONPath(id)); !os.IsNotExist(err) {
				t.Fatalf("run.json survived a bound delete of the run it observed: %v", err)
			}
			if !s.runDeleted(id) {
				t.Fatal("a bound delete of the run it observed left no tombstone")
			}
		})
	}
}

// TestBoundDeleteComparesTheStoredTenant: the tenant half of the bound
// delete's comparison, alone. A record under the same header as the observed
// run — historical, so no birth tells them apart — but another stored tenant
// is refused ErrRunDataIdentityChanged, its run.json left as it was.
func TestBoundDeleteComparesTheStoredTenant(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithIdentity(t.Context(), "team-a", "user")
	const id = "r"
	if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatal(err)
	}
	rewriteRunJSON(t, s, id, func(m map[string]json.RawMessage) {
		delete(m, "birth_id")
		m["tenant_id"] = json.RawMessage(`"team-a"`)
	})
	bound := bindFilesystemRun(t, s, ctx, id)
	b, _ := RunDataBindingFrom(bound)
	if b.Header().BirthID != "" {
		t.Fatalf("fixture: the observed run has birth %q, want a historical one", b.Header().BirthID)
	}
	if tenant, observed := b.TenantID(); !observed || tenant != "team-a" {
		t.Fatalf("fixture: observed tenant (%q, %t), want (team-a, true)", tenant, observed)
	}
	rewriteRunJSON(t, s, id, func(m map[string]json.RawMessage) { m["tenant_id"] = json.RawMessage(`"team-b"`) })
	before, err := os.ReadFile(s.runJSONPath(id))
	if err != nil {
		t.Fatal(err)
	}
	err = s.DeleteRun(bound, id)
	if after, rerr := os.ReadFile(s.runJSONPath(id)); rerr != nil || !bytes.Equal(before, after) {
		t.Fatalf("a bound delete changed another tenant's run.json (read: %v)", rerr)
	}
	if s.runDeleted(id) {
		t.Fatal("a bound delete tombstoned another tenant's run")
	}
	if !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("delete bound to team-a on a team-b record of the same header = %v, want ErrRunDataIdentityChanged", err)
	}
}

// bindFilesystemRun binds ctx to the observation of the run id holds now.
func bindFilesystemRun(t *testing.T, s *FilesystemRunStore, ctx context.Context, id string) context.Context {
	t.Helper()
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	o, err := CaptureRunObservation(RunDataHeader{RunID: r.ID, Format: ValueFormat(r.FormatVersion), Protocol: r.DataProtocol, BirthID: r.BirthID}, r.TenantID, true)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindRunObservation(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// TestBoundDeleteAnswersWhatABoundLoadAnswers: a delete bound to a run that
// was deleted answers ErrRunDeleted while its tombstone stands and
// ErrRunNotFound once the tombstone is reaped — never a silent idempotent
// success that would report the bound run removed by this call.
func TestBoundDeleteAnswersWhatABoundLoadAnswers(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := s.CreateRun(ctx, "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	bound := bindFilesystemRun(t, s, ctx, "r")
	if err := s.DeleteRun(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(s.runDir("r"), deletionMarkerName)
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(bound, "r"); !errors.Is(err, ErrRunDeleted) {
		t.Fatalf("bound delete of a tombstoned run = %v, want ErrRunDeleted", err)
	}
	if after, err := os.ReadFile(marker); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a refused bound delete rewrote the tombstone: %v", err)
	}
	if n, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("PruneDeletionMarkers = (%d, %v), want (1, nil)", n, err)
	}
	if err := s.DeleteRun(bound, "r"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("bound delete of a reaped run = %v, want ErrRunNotFound", err)
	}
	if _, err := os.Stat(s.runDir("r")); !os.IsNotExist(err) {
		t.Fatalf("a refused bound delete left a run directory behind: %v", err)
	}
}

// TestUnboundDeletionKeepsTheFormatOnlyHeaderCheck: a run.json the strict
// header parser refuses (a birth_id case alias) while the format read
// accepts it stays deletable and prunable by unbound callers, as before the
// bound door; a bound delete reads the header and refuses it, the document
// untouched.
func TestUnboundDeletionKeepsTheFormatOnlyHeaderCheck(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"delete", "prune", "bound-delete"} {
		t.Run(op, func(t *testing.T) {
			s, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if _, err := s.CreateRun(ctx, "r", "wf", nil); err != nil {
				t.Fatal(err)
			}
			bound := bindFilesystemRun(t, s, ctx, "r")
			raw, err := os.ReadFile(s.runJSONPath("r"))
			if err != nil {
				t.Fatal(err)
			}
			legacy := []byte(strings.Replace(string(raw), "{", `{"Birth_Id": "x",`, 1))
			if err := os.WriteFile(s.runJSONPath("r"), legacy, filePerm); err != nil {
				t.Fatal(err)
			}
			if _, err := RunValueFormatFromJSON(ctx, legacy); err != nil {
				t.Fatalf("fixture: the format read refuses the legacy shape: %v", err)
			}
			if _, err := RunDataHeaderFromJSON(ctx, legacy); !errors.Is(err, ErrInvalidRunDataIdentity) {
				t.Fatalf("fixture: the header parser accepts the legacy shape: %v", err)
			}
			switch op {
			case "delete":
				if err := s.DeleteRun(ctx, "r"); err != nil {
					t.Fatalf("unbound delete of the legacy shape = %v, want nil", err)
				}
				if _, err := os.Stat(s.runJSONPath("r")); !os.IsNotExist(err) {
					t.Fatalf("unbound delete kept run.json: %v", err)
				}
				if !s.runDeleted("r") {
					t.Fatal("unbound delete wrote no tombstone")
				}
			case "prune":
				if err := os.WriteFile(filepath.Join(s.runDir("r"), deletionMarkerName), []byte("interrupted"), filePerm); err != nil {
					t.Fatal(err)
				}
				if n, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
					t.Fatalf("unbound prune of a tombstone retaining the legacy shape = (%d, %v), want (1, nil)", n, err)
				}
				if _, err := os.Stat(s.runDir("r")); !os.IsNotExist(err) {
					t.Fatalf("unbound prune kept the run directory: %v", err)
				}
			case "bound-delete":
				if err := s.DeleteRun(bound, "r"); !errors.Is(err, ErrInvalidRunDataIdentity) {
					t.Fatalf("bound delete of the legacy shape = %v, want ErrInvalidRunDataIdentity", err)
				}
				if after, err := os.ReadFile(s.runJSONPath("r")); err != nil || !bytes.Equal(after, legacy) {
					t.Fatalf("a refused bound delete changed run.json: %v", err)
				}
				if s.runDeleted("r") {
					t.Fatal("a refused bound delete wrote a tombstone")
				}
			}
		})
	}
}
