package task

import (
	"errors"
	"testing"
)

func TestCreateWithSpecWiresReciprocalEdges(t *testing.T) {
	r := NewRegistry()
	a, err := r.CreateWithSpec(TaskSpec{Subject: "design schema"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Prompt != "design schema" || a.Subject != "design schema" {
		t.Errorf("subject must seed prompt and vice versa: %+v", a)
	}
	if a.WorkStatus != "pending" {
		t.Errorf("WorkStatus = %q, want pending", a.WorkStatus)
	}

	b, err := r.CreateWithSpec(TaskSpec{
		Subject:    "implement schema",
		ActiveForm: "Implementing the schema",
		Owner:      "coder",
		BlockedBy:  []string{a.TaskID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Blocked {
		t.Error("b waits on a (pending) — must be blocked")
	}

	// Reciprocity: a.Blocks must now contain b.
	a2, _ := r.Get(a.TaskID)
	if len(a2.Blocks) != 1 || a2.Blocks[0] != b.TaskID {
		t.Errorf("a.Blocks = %v, want [%s]", a2.Blocks, b.TaskID)
	}

	// Completing a unblocks b.
	if err := r.SetStatus(a.TaskID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	b2, _ := r.Get(b.TaskID)
	if b2.Blocked {
		t.Error("b must be unblocked once its dependency completes")
	}
}

func TestCreateWithSpecUnknownRefErrors(t *testing.T) {
	r := NewRegistry()
	if _, err := r.CreateWithSpec(TaskSpec{Subject: "x", Blocks: []string{"nope"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown blocks ref must be ErrNotFound, got %v", err)
	}
	if _, err := r.CreateWithSpec(TaskSpec{}); err == nil {
		t.Error("empty prompt+subject must error")
	}
}

func TestSetFieldsAndDeletedCleansEdges(t *testing.T) {
	r := NewRegistry()
	a, _ := r.CreateWithSpec(TaskSpec{Subject: "a"})
	b, _ := r.CreateWithSpec(TaskSpec{Subject: "b", BlockedBy: []string{a.TaskID}})

	subj := "a (renamed)"
	st := StatusRunning
	got, err := r.SetFields(a.TaskID, TaskFieldUpdate{Subject: &subj, Status: &st})
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != subj || got.WorkStatus != "in_progress" {
		t.Errorf("SetFields result wrong: %+v", got)
	}

	// Deleting a strips it from b's blocked_by and unblocks b.
	if _, err := r.SetFields(a.TaskID, TaskFieldUpdate{Deleted: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get(a.TaskID); ok {
		t.Fatal("a must be gone")
	}
	b2, _ := r.Get(b.TaskID)
	if len(b2.BlockedBy) != 0 || b2.Blocked {
		t.Errorf("b must have no stale edges after a's deletion: %+v", b2)
	}
}

func TestParseStatusAlias(t *testing.T) {
	cases := map[string]TaskStatus{
		"pending":     StatusCreated,
		"in_progress": StatusRunning,
		"completed":   StatusCompleted,
		"created":     StatusCreated,
		"running":     StatusRunning,
		"failed":      StatusFailed,
		"stopped":     StatusStopped,
	}
	for in, want := range cases {
		got, ok := ParseStatusAlias(in)
		if !ok || got != want {
			t.Errorf("ParseStatusAlias(%q) = %v/%v, want %v", in, got, ok, want)
		}
	}
	if _, ok := ParseStatusAlias("bogus"); ok {
		t.Error("bogus must not parse")
	}
}

func TestSelfReferenceEdgesIgnored(t *testing.T) {
	r := NewRegistry()
	a, _ := r.CreateWithSpec(TaskSpec{Subject: "a"})
	got, err := r.SetFields(a.TaskID, TaskFieldUpdate{AddBlocks: []string{a.TaskID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Blocks) != 0 {
		t.Errorf("self-reference must be ignored: %+v", got.Blocks)
	}
}
