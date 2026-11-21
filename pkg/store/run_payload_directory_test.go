package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func directoryFixture(t *testing.T, format ValueFormat) (*FilesystemRunStore, context.Context) {
	t.Helper()
	requireIncarnationPayloadStorage(t)
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", WorkflowName: "wf", Format: format, DataProtocol: IncarnationRunData})
	if e != nil {
		t.Fatal(e)
	}
	ctx, _ := scopeFixture(t, RunDataHeader{RunID: r.ID, Format: format, Protocol: r.DataProtocol, BirthID: r.BirthID})
	ctx, e = WithRunValueFormat(ctx, RunValueFormat{RunID: r.ID, Format: format})
	if e != nil {
		t.Fatal(e)
	}
	return s, ctx
}
func TestIncarnationDirectoryRetainsWholeOperation(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := directoryFixture(t, format)
			d, e := s.openRunPayloadDirectory(ctx, "r", true, "plans")
			if e != nil {
				t.Fatal(e)
			}
			defer d.Close()
			if e = d.WriteAtomic(ctx, "a.json", []byte("old"), 0600); e != nil {
				t.Fatal(e)
			}
			entries, e := d.ReadDir()
			if e != nil || len(entries) != 1 {
				t.Fatal(entries, e)
			}
			original := d.parent.key + "-retained"
			if e = os.Rename(d.parent.key, original); e != nil {
				t.Fatal(e)
			}
			if e = os.Mkdir(d.parent.key, 0700); e != nil {
				t.Fatal(e)
			}
			replacement := filepath.Join(d.parent.key, "a.json")
			if e = os.WriteFile(replacement, []byte("replacement"), 0600); e != nil {
				t.Fatal(e)
			}
			b, e := d.ReadFile(entries[0].Name())
			if e != nil || string(b) != "old" {
				t.Fatalf("scan/read changed scope: %q %v", b, e)
			}
			if e = d.WriteAtomic(ctx, "b.json", append(b, '!'), 0600); e != nil {
				t.Fatal(e)
			}
			if e = d.Remove(ctx, "a.json"); e != nil {
				t.Fatal(e)
			}
			b, e = os.ReadFile(filepath.Join(original, "b.json"))
			if e != nil || string(b) != "old!" {
				t.Fatalf("write reopened replacement: %q %v", b, e)
			}
			b, e = os.ReadFile(replacement)
			if e != nil || string(b) != "replacement" {
				t.Fatalf("removal escaped original: %q %v", b, e)
			}
			if _, e = os.Stat(filepath.Join(original, "a.json")); !os.IsNotExist(e) {
				t.Fatal("original entry not removed", e)
			}
		})
	}
}
func TestIncarnationDirectoryRejectsLastDirectorySymlink(t *testing.T) {
	s, ctx := directoryFixture(t, LegacyValueFormat)
	d, e := s.openRunPayloadDirectory(ctx, "r", true, "notes")
	if e != nil {
		t.Fatal(e)
	}
	path := d.parent.key
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(path, path+"-old"); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(t.TempDir(), path); e != nil {
		t.Fatal(e)
	}
	for _, create := range []bool{false, true} {
		if got, e := s.openRunPayloadDirectory(ctx, "r", create, "notes"); e == nil {
			got.Close()
			t.Fatal("symlink directory accepted", create)
		}
	}
}
func TestIncarnationDirectoryMissingAndCancellation(t *testing.T) {
	s, ctx := directoryFixture(t, LegacyValueFormat)
	d, e := s.openRunPayloadDirectory(ctx, "r", false, "plans")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = d.ReadDir(); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if d, e = s.openRunPayloadDirectory(canceled, "r", true, "plans"); !errors.Is(e, context.Canceled) || d != nil {
		t.Fatal(d, e)
	}
}
func TestPayloadDirectoryLegacyAndInvalidContext(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRun(t.Context(), "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	d, e := s.openRunPayloadDirectory(t.Context(), "r", true, "plans")
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if e = d.WriteAtomic(t.Context(), "a.json", []byte("legacy"), 0600); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(s.root, "runs", "r", "plans", "a.json"))
	if e != nil || string(b) != "legacy" {
		t.Fatal(string(b), e)
	}
	if _, e = d.ReadFile("../outside"); e == nil {
		t.Fatal("invalid filename accepted")
	}
	if d, e = s.openRunPayloadDirectory(nil, "r", false, "plans"); !errors.Is(e, ErrRunDataBindingRequired) || d != nil {
		t.Fatal(d, e)
	}
}

func TestIncarnationAttemptAndSessionRejectDirectoryReplacement(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, operation := range []string{"record-attempt", "load-attempt", "put-session", "get-session", "delete-session"} {
			t.Run(fmt.Sprintf("%d/%s", format, operation), func(t *testing.T) {
				s, ctx := directoryFixture(t, format)
				attempt := &AttemptSnapshot{WorkflowHash: "original"}
				meta, encoded, e := EncodeAttempt(attempt)
				if e != nil {
					t.Fatal(e)
				}
				domain, name, body := "backend-sessions", "session", []byte("outside")
				if operation == "record-attempt" || operation == "load-attempt" {
					domain, name, body = "attempts", meta.ID+".json", encoded
				}
				d, e := s.openRunPayloadDirectory(ctx, "r", true, domain)
				if e != nil {
					t.Fatal(e)
				}
				path := d.parent.key
				if e = d.Close(); e != nil {
					t.Fatal(e)
				}
				if e = os.Rename(path, path+"-retained"); e != nil {
					t.Fatal(e)
				}
				outside := t.TempDir()
				target := filepath.Join(outside, name)
				if e = os.WriteFile(target, body, 0600); e != nil {
					t.Fatal(e)
				}
				if e = os.Symlink(outside, path); e != nil {
					t.Fatal(e)
				}
				switch operation {
				case "record-attempt":
					_, e = s.RecordAttempt(ctx, "r", attempt)
				case "load-attempt":
					_, e = s.LoadAttempt(ctx, "r", meta.ID)
				case "put-session":
					e = s.PutBackendSession(ctx, "r", "session", []byte("replacement"))
				case "get-session":
					_, e = s.GetBackendSession(ctx, "r", "session")
				case "delete-session":
					e = s.DeleteBackendSession(ctx, "r", "session")
				}
				if e == nil {
					t.Fatal("operation accepted a replaced directory")
				}
				after, e := os.ReadFile(target)
				if e != nil || string(after) != string(body) {
					t.Fatal("foreign payload modified", e)
				}
			})
		}
	}
}

func TestIncarnationAttemptAndSessionRestart(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := directoryFixture(t, format)
			meta, e := s.RecordAttempt(ctx, "r", &AttemptSnapshot{WorkflowHash: "original"})
			if e != nil {
				t.Fatal(e)
			}
			if e = s.PutBackendSession(ctx, "r", "session", []byte("session-body")); e != nil {
				t.Fatal(e)
			}
			resumed, e := New(s.root)
			if e != nil {
				t.Fatal(e)
			}
			attempt, e := resumed.LoadAttempt(ctx, "r", meta.ID)
			if e != nil || attempt.WorkflowHash != "original" {
				t.Fatal(attempt, e)
			}
			body, e := resumed.GetBackendSession(ctx, "r", "session")
			if e != nil || string(body) != "session-body" {
				t.Fatal(string(body), e)
			}
			if e = resumed.DeleteBackendSession(ctx, "r", "session"); e != nil {
				t.Fatal(e)
			}
			if e = resumed.DeleteBackendSession(ctx, "r", "session"); e != nil {
				t.Fatal("delete retry", e)
			}
			if _, e = s.GetBackendSession(ctx, "r", "session"); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("session survives removal", e)
			}
		})
	}
}
