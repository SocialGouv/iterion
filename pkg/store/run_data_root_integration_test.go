package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIncarnationNamespaceRejectsSymlinkAncestors(t *testing.T) {
	requireIncarnationPayloadStorage(t)
	for depth := 0; depth < 4; depth++ {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			s, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			r, e := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", WorkflowName: "wf", Format: LegacyValueFormat, DataProtocol: IncarnationRunData})
			if e != nil {
				t.Fatal(e)
			}
			h := RunDataHeader{RunID: r.ID, Format: LegacyValueFormat, Protocol: r.DataProtocol, BirthID: r.BirthID}
			b, e := CaptureRunDataBinding(h)
			if e != nil {
				t.Fatal(e)
			}
			ctx, e := WithRunDataBinding(t.Context(), b)
			if e != nil {
				t.Fatal(e)
			}
			components := []string{".run-data-v1", "f1", r.ID, r.BirthID}
			parent := filepath.Join(append([]string{s.root}, components[:depth]...)...)
			if e = os.MkdirAll(parent, 0700); e != nil {
				t.Fatal(e)
			}
			outside := t.TempDir()
			if e = os.Symlink(outside, filepath.Join(parent, components[depth])); e != nil {
				t.Fatal(e)
			}
			e = s.AppendQueuedMessage(ctx, r.ID, QueuedUserMessage{ID: "x", Text: "private"})
			leaked := filepath.Join(append([]string{outside}, append(components[depth+1:], "user_messages.jsonl")...)...)
			if data, readErr := os.ReadFile(leaked); readErr == nil {
				t.Fatalf("escaped at ancestor %d: append=%v leaked_bytes=%d", depth, e, len(data))
			}
			if e == nil {
				t.Fatal("symlink namespace accepted")
			}
		})
	}
}
func TestIncarnationInboxFileSymlinkAndCounters(t *testing.T) {
	requireIncarnationPayloadStorage(t)
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			r, e := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", WorkflowName: "wf", Format: format, DataProtocol: IncarnationRunData})
			if e != nil {
				t.Fatal(e)
			}
			h := RunDataHeader{RunID: r.ID, Format: format, Protocol: r.DataProtocol, BirthID: r.BirthID}
			b, e := CaptureRunDataBinding(h)
			if e != nil {
				t.Fatal(e)
			}
			ctx, e := WithRunDataBinding(t.Context(), b)
			if e != nil {
				t.Fatal(e)
			}
			other, e := New(s.root)
			if e != nil {
				t.Fatal(e)
			}
			v0, e := other.QueuedInboxVersionContext(ctx, r.ID)
			if e != nil {
				t.Fatal(e)
			}
			if yes, e := s.AppendQueuedMessageOnce(ctx, r.ID, QueuedUserMessage{ID: "x", Text: "first"}); e != nil || !yes {
				t.Fatal(yes, e)
			}
			if yes, e := s.AppendQueuedMessageOnce(ctx, r.ID, QueuedUserMessage{ID: "x", Text: "replay"}); e != nil || yes {
				t.Fatal(yes, e)
			}
			v1, e := other.QueuedInboxVersionContext(ctx, r.ID)
			if e != nil || v1 <= v0 {
				t.Fatal(v0, v1, e)
			}
			rows, e := other.ListQueuedMessages(ctx, r.ID)
			if e != nil || len(rows) != 1 || rows[0].Text != "first" {
				t.Fatal(rows, e)
			}
			path, e := s.runPayloadPath(ctx, r.ID, "user_messages.jsonl")
			if e != nil {
				t.Fatal(e)
			}
			if e = os.Remove(path); e != nil {
				t.Fatal(e)
			}
			target := filepath.Join(t.TempDir(), "outside")
			body := []byte("outside sentinel")
			if e = os.WriteFile(target, body, 0600); e != nil {
				t.Fatal(e)
			}
			if e = os.Symlink(target, path); e != nil {
				t.Fatal(e)
			}
			if e = s.AppendQueuedMessage(ctx, r.ID, QueuedUserMessage{ID: "escape", Text: "x"}); e == nil {
				t.Fatal("appended through external symlink")
			}
			if rows, e = s.ListQueuedMessages(ctx, r.ID); e == nil || len(rows) != 0 {
				t.Fatal("read external symlink", rows, e)
			}
			if _, e = other.QueuedInboxVersionContext(ctx, r.ID); e == nil {
				t.Fatal("stat external symlink")
			}
			got, e := os.ReadFile(target)
			if e != nil || string(got) != string(body) {
				t.Fatal("outside changed", e)
			}
		})
	}
}
func TestIncarnationPayloadHandleCannotReopenReplacedPath(t *testing.T) {
	requireIncarnationPayloadStorage(t)
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", WorkflowName: "wf", Format: LegacyValueFormat, DataProtocol: IncarnationRunData})
	if e != nil {
		t.Fatal(e)
	}
	h := RunDataHeader{RunID: r.ID, Format: LegacyValueFormat, Protocol: r.DataProtocol, BirthID: r.BirthID}
	b, e := CaptureRunDataBinding(h)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := WithRunDataBinding(t.Context(), b)
	if e != nil {
		t.Fatal(e)
	}
	f, e := s.openRunPayloadFile(ctx, r.ID, "payload", true)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	parent := filepath.Dir(f.key)
	if e = os.Rename(parent, parent+"-retained"); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	if e = os.Symlink(outside, parent); e != nil {
		t.Fatal(e)
	}
	out, e := f.OpenFile(os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = out.Write([]byte("original")); e != nil {
		t.Fatal(e)
	}
	out.Close()
	if _, e = os.Stat(filepath.Join(outside, "payload")); !os.IsNotExist(e) {
		t.Fatal("retained handle reopened replacement pathname", e)
	}
	data, e := os.ReadFile(filepath.Join(parent+"-retained", "payload"))
	if e != nil || string(data) != "original" {
		t.Fatal(string(data), e)
	}
}
func TestIncarnationAtomicSnapshotKeepsRetainedDirectory(t *testing.T) {
	requireIncarnationPayloadStorage(t)
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", WorkflowName: "wf", Format: ExactValueFormat, DataProtocol: IncarnationRunData})
	if e != nil {
		t.Fatal(e)
	}
	h := RunDataHeader{RunID: r.ID, Format: ExactValueFormat, Protocol: r.DataProtocol, BirthID: r.BirthID}
	b, e := CaptureRunDataBinding(h)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := WithRunDataBinding(t.Context(), b)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SetRunTags(ctx, r.ID, []string{"original"}); e != nil {
		t.Fatal(e)
	}
	tags, e := s.GetRunTags(ctx, r.ID)
	if e != nil || len(tags) != 1 || tags[0] != "original" {
		t.Fatal(tags, e)
	}
	if e = s.SaveRunGitMeta(ctx, r.ID, &RunGitMeta{BaseCommit: "original"}); e != nil {
		t.Fatal(e)
	}
	meta, e := s.LoadRunGitMeta(ctx, r.ID)
	if e != nil || meta.BaseCommit != "original" {
		t.Fatal(meta, e)
	}
	f, e := s.openRunPayloadFile(ctx, r.ID, "snapshot", true)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	parent := filepath.Dir(f.key)
	if e = os.Rename(parent, parent+"-retained"); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	if e = os.Symlink(outside, parent); e != nil {
		t.Fatal(e)
	}
	if e = f.WriteAtomic(ctx, []byte("retained"), 0600); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(parent+"-retained", "snapshot"))
	if e != nil || string(data) != "retained" {
		t.Fatal(string(data), e)
	}
	if rows, e := os.ReadDir(outside); e != nil || len(rows) != 0 {
		t.Fatal("atomic write escaped", rows, e)
	}
	if e = s.SetRunTags(ctx, r.ID, []string{"escape"}); e == nil {
		t.Fatal("new snapshot adopted replaced namespace")
	}
}
func TestIncarnationNestedToolBlobIsolation(t *testing.T) {
	requireIncarnationPayloadStorage(t)
	for depth := 0; depth < 2; depth++ {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			s, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			r, e := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", WorkflowName: "wf", Format: LegacyValueFormat, DataProtocol: IncarnationRunData})
			if e != nil {
				t.Fatal(e)
			}
			h := RunDataHeader{RunID: r.ID, Format: LegacyValueFormat, Protocol: r.DataProtocol, BirthID: r.BirthID}
			b, e := CaptureRunDataBinding(h)
			if e != nil {
				t.Fatal(e)
			}
			ctx, e := WithRunDataBinding(t.Context(), b)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.WriteToolBlob(ctx, r.ID, "good", "output", []byte("original")); e != nil {
				t.Fatal(e)
			}
			data, _, _, e := s.ReadToolBlob(ctx, r.ID, "good", "output", 0, 0)
			if e != nil || string(data) != "original" {
				t.Fatal(string(data), e)
			}
			scope, e := ResolveIncarnationDataScope(ctx, h)
			if e != nil {
				t.Fatal(e)
			}
			base, e := scope.FilesystemRoot(s.root)
			if e != nil {
				t.Fatal(e)
			}
			var link string
			if depth == 0 {
				link = filepath.Join(base, "tools")
				if e = os.Rename(link, link+"-retained"); e != nil {
					t.Fatal(e)
				}
			} else {
				link = filepath.Join(base, "tools", "bad")
			}
			outside := t.TempDir()
			if e = os.Symlink(outside, link); e != nil {
				t.Fatal(e)
			}
			if _, e = s.WriteToolBlob(ctx, r.ID, "bad", "output", []byte("escape")); e == nil {
				t.Fatal("nested symlink accepted")
			}
			if files, e := os.ReadDir(outside); e != nil || len(files) != 0 {
				t.Fatal("nested write escaped", files, e)
			}
		})
	}
}
func TestIncarnationEventsAndCheckpointStayScoped(t *testing.T) {
	requireIncarnationPayloadStorage(t)
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			r, e := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", WorkflowName: "wf", Format: format, DataProtocol: IncarnationRunData})
			if e != nil {
				t.Fatal(e)
			}
			h := RunDataHeader{RunID: r.ID, Format: format, Protocol: r.DataProtocol, BirthID: r.BirthID}
			b, e := CaptureRunDataBinding(h)
			if e != nil {
				t.Fatal(e)
			}
			ctx, e := WithRunDataBinding(t.Context(), b)
			if e != nil {
				t.Fatal(e)
			}
			ctx, e = WithRunValueFormat(ctx, RunValueFormat{RunID: r.ID, Format: format})
			if e != nil {
				t.Fatal(e)
			}
			other, e := New(s.root)
			if e != nil {
				t.Fatal(e)
			}
			first, e := s.AppendEvent(ctx, r.ID, Event{Type: "first"})
			if e != nil || first.Seq != 0 {
				t.Fatal(first, e)
			}
			second, e := other.AppendEvent(ctx, r.ID, Event{Type: "second"})
			if e != nil || second.Seq != 1 {
				t.Fatal(second, e)
			}
			rows, e := s.LoadEvents(ctx, r.ID)
			if e != nil || len(rows) != 2 || rows[0].Seq != 0 || rows[1].Seq != 1 {
				t.Fatal(rows, e)
			}
			if e = s.SaveRun(ctx, r); e != nil {
				t.Fatal(e)
			}
			before, e := os.ReadFile(s.runJSONPath(r.ID))
			if e != nil {
				t.Fatal(e)
			}
			path, e := s.runPayloadPath(ctx, r.ID, "events.jsonl")
			if e != nil {
				t.Fatal(e)
			}
			if e = os.Remove(path); e != nil {
				t.Fatal(e)
			}
			outside := filepath.Join(t.TempDir(), "events")
			sentinel := []byte("outside")
			if e = os.WriteFile(outside, sentinel, 0600); e != nil {
				t.Fatal(e)
			}
			if e = os.Symlink(outside, path); e != nil {
				t.Fatal(e)
			}
			if _, e = s.AppendEvent(ctx, r.ID, Event{Type: "escape"}); e == nil {
				t.Fatal("append adopted outside events")
			}
			if rows, e = s.LoadEvents(ctx, r.ID); e == nil || len(rows) != 0 {
				t.Fatal("read adopted outside events", rows, e)
			}
			r.Status = RunStatusFinished
			if e = s.SaveRun(ctx, r); e == nil {
				t.Fatal("checkpoint crossed outside event log")
			}
			after, e := os.ReadFile(s.runJSONPath(r.ID))
			if e != nil || string(before) != string(after) {
				t.Fatal("checkpoint changed despite barrier", e)
			}
			data, e := os.ReadFile(outside)
			if e != nil || string(data) != string(sentinel) {
				t.Fatal("outside changed", e)
			}
		})
	}
}

// The production fence stays closed until all storage domains are qualified.
// The dormant lifecycle is qualified through a test-only admission overlay;
// after activation these tests run normally without an override.
func requireIncarnationPayloadStorage(t *testing.T) {
	t.Helper()
	if err := RequireRunDataStorage(IncarnationRunData); err != nil {
		if !errors.Is(err, ErrRunDataCapability) {
			t.Fatal(err)
		}
		t.Skip("incarnation payload admission is not enabled")
	}
}
