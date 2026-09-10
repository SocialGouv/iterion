package boardops

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// staleReadStore hands every reader a snapshot taken BEFORE a one-shot
// trigger label was consumed — the shape of a bot that read the card,
// then had the trigger spine strip `triage:auto` atomically before its
// own label write landed. A label operation built from a read (Get →
// merge → Update, or the agent composing an absolute list) re-arms the
// one-shot; a relative operation never consults the read.
type staleReadStore struct {
	native.BoardStore
	stale *native.Issue
}

func (s *staleReadStore) Get(id string) (*native.Issue, error) {
	if id == s.stale.ID {
		cp := *s.stale
		cp.Labels = append([]string(nil), s.stale.Labels...)
		return &cp, nil
	}
	return s.BoardStore.Get(id)
}

// TestAddLabels_DoesNotRearmAConsumedOneShot is the #666 defect as a
// contract: an incremental label change must be RELATIVE to the card as
// it is, never rebuilt from what the caller read earlier. set_labels is
// absolute by design (its stale list re-arms `triage:auto` BY INTENT —
// pinned below as the documented shape), which is exactly why agents
// need add_labels / remove_labels for anything incremental.
func TestAddLabels_DoesNotRearmAConsumedOneShot(t *testing.T) {
	s := newStore(t)
	caps := NewCapabilities("board.label,board.read")
	created, err := s.Create(native.Issue{Title: "fresh issue", State: native.StateInbox, Labels: []string{"triage:auto", "kind:bug"}})
	if err != nil {
		t.Fatal(err)
	}
	// The bot's read happens here…
	stale, _ := s.Get(created.ID)
	// …then the trigger spine consumes the one-shot before the bot's
	// label write (the local effect strips it through Update).
	consumed := []string{"kind:bug"}
	if _, err := s.Update(created.ID, native.Patch{Labels: &consumed}); err != nil {
		t.Fatal(err)
	}
	wrapped := &staleReadStore{BoardStore: s, stale: stale}

	args, _ := json.Marshal(map[string]any{"id": created.ID, "labels": []string{"source:issue-triage"}})
	res, err := Call(wrapped, caps, "add_labels", args)
	if err != nil {
		t.Fatalf("add_labels: %v", err)
	}
	var got native.Issue
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.Labels, "triage:auto") {
		t.Fatalf("REPRODUCED: add_labels re-armed the consumed one-shot from a stale read: labels=%v", got.Labels)
	}
	want := []string{"kind:bug", "source:issue-triage"}
	if !slices.Equal(got.Labels, want) {
		t.Fatalf("labels = %v, want %v (existing kept in order, the new one appended)", got.Labels, want)
	}
	if cur, _ := s.Get(created.ID); !slices.Equal(cur.Labels, want) {
		t.Fatalf("stored labels = %v, want %v", cur.Labels, want)
	}

	// The absolute op, fed the stale list, re-arms it — the API's shape,
	// and the reason the relative ops exist.
	args, _ = json.Marshal(map[string]any{"id": created.ID, "labels": append(stale.Labels, "source:issue-triage")})
	if _, err := Call(s, caps, "set_labels", args); err != nil {
		t.Fatalf("set_labels: %v", err)
	}
	if cur, _ := s.Get(created.ID); !slices.Contains(cur.Labels, "triage:auto") {
		t.Fatalf("set_labels is documented as ABSOLUTE and must replay its list: %v", cur.Labels)
	}
}

func TestAddRemoveLabels_RelativeSemantics(t *testing.T) {
	s := newStore(t)
	caps := NewCapabilities("board.label")
	created, err := s.Create(native.Issue{Title: "labelled", State: native.StateBacklog, Labels: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(tool string, labels []string) native.Issue {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"id": created.ID, "labels": labels})
		res, err := Call(s, caps, tool, args)
		if err != nil {
			t.Fatalf("%s(%v): %v", tool, labels, err)
		}
		var got native.Issue
		if err := json.Unmarshal(res, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Idempotent add: present labels are left alone, duplicates in the
	// request collapse, order is existing-then-new.
	if got := call("add_labels", []string{"b", "c", "c", " d "}); !slices.Equal(got.Labels, []string{"a", "b", "c", "d"}) {
		t.Fatalf("add: %v", got.Labels)
	}
	// Remove leaves the rest in place; an absent label is a no-op.
	if got := call("remove_labels", []string{"a", "zzz"}); !slices.Equal(got.Labels, []string{"b", "c", "d"}) {
		t.Fatalf("remove: %v", got.Labels)
	}
	// Short id prefixes resolve like every other board op.
	prefix := created.ID[len("native:") : len("native:")+8]
	args, _ := json.Marshal(map[string]any{"id": prefix, "labels": []string{"e"}})
	if _, err := Call(s, caps, "add_labels", args); err != nil {
		t.Fatalf("add_labels by prefix: %v", err)
	}
	if cur, _ := s.Get(created.ID); !slices.Equal(cur.Labels, []string{"b", "c", "d", "e"}) {
		t.Fatalf("after prefix add: %v", cur.Labels)
	}
	// An empty request is an error, never a silent no-op.
	args, _ = json.Marshal(map[string]any{"id": created.ID, "labels": []string{" "}})
	if _, err := Call(s, caps, "add_labels", args); err == nil || !strings.Contains(err.Error(), "labels") {
		t.Fatalf("empty add_labels must be refused explicitly, got %v", err)
	}
	args, _ = json.Marshal(map[string]any{"id": created.ID})
	if _, err := Call(s, caps, "remove_labels", args); err == nil {
		t.Fatal("remove_labels without labels must be refused")
	}
}

func TestAddRemoveLabels_RequireLabelCapability(t *testing.T) {
	s := newStore(t)
	created, err := s.Create(native.Issue{Title: "gated", State: native.StateBacklog})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"id": created.ID, "labels": []string{"x"}})
	for _, tool := range []string{"add_labels", "remove_labels"} {
		if _, err := Call(s, NewCapabilities("board.read,board.assign"), tool, args); !errors.Is(err, ErrCapabilityDenied) {
			t.Fatalf("%s without board.label: err=%v, want ErrCapabilityDenied", tool, err)
		}
	}
	names := map[string]bool{}
	for _, tl := range ToolsFor(NewCapabilities("board.label")) {
		names[tl.Name] = true
	}
	for _, want := range []string{"add_labels", "remove_labels", "set_labels"} {
		if !names[want] {
			t.Fatalf("board.label must unlock %s, got %v", want, names)
		}
	}
}
