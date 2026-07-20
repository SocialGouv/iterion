package runner

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/queue"
)

func TestContributionsFromWire_NilStaysNil(t *testing.T) {
	if got := contributionsFromWire(nil); got != nil {
		t.Errorf("nil wire payload must stay nil (local resolution), got %+v", got)
	}
}

// An EMPTY (but present) payload must convert to a non-nil value: the runner
// passes it to the engine to suppress the dead local lookup on a pod whose
// iterion home is empty. Converting it to nil would silently re-enable that.
func TestContributionsFromWire_EmptyStaysNonNil(t *testing.T) {
	got := contributionsFromWire(&queue.Contributions{})
	if got == nil {
		t.Fatal("an empty-but-present payload must convert to non-nil (authoritative)")
	}
	if !got.IsEmpty() {
		t.Errorf("expected empty payload, got %+v", got)
	}
}

func TestContributionsFromWire_CarriesBothKinds(t *testing.T) {
	got := contributionsFromWire(&queue.Contributions{
		Plugin: []queue.ContributionFile{
			{Kind: "skills", Name: "deploy-target.md", Content: []byte("playbook")},
		},
		Library: []queue.LibrarySkillFile{
			{Name: "changelog-writer", Description: "writes changelogs", Content: []byte("body")},
		},
	})
	if len(got.Plugin) != 1 || got.Plugin[0].Kind != "skills" ||
		got.Plugin[0].Name != "deploy-target.md" || string(got.Plugin[0].Content) != "playbook" {
		t.Errorf("plugin file not carried faithfully: %+v", got.Plugin)
	}
	if len(got.Library) != 1 || got.Library[0].Name != "changelog-writer" ||
		got.Library[0].Description != "writes changelogs" || string(got.Library[0].Content) != "body" {
		t.Errorf("library skill not carried faithfully: %+v", got.Library)
	}
}

// The payload has to survive the actual JSON envelope, since that is how it
// reaches the pod.
func TestContributions_SurvivesQueueJSONRoundtrip(t *testing.T) {
	in := &queue.RunMessage{
		V:     queue.SchemaVersion,
		RunID: "r1",
		Contributions: &queue.Contributions{
			Plugin:  []queue.ContributionFile{{Kind: "skills", Name: "deploy-target.md", Content: []byte("playbook")}},
			Library: []queue.LibrarySkillFile{{Name: "cw", Description: "d", Content: []byte("b")}},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out queue.RunMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Contributions == nil {
		t.Fatal("contributions lost in JSON roundtrip")
	}
	conv := contributionsFromWire(out.Contributions)
	if len(conv.Plugin) != 1 || string(conv.Plugin[0].Content) != "playbook" {
		t.Errorf("plugin file lost: %+v", conv.Plugin)
	}
	if len(conv.Library) != 1 || string(conv.Library[0].Content) != "b" {
		t.Errorf("library skill lost: %+v", conv.Library)
	}
}

// A message published WITHOUT contributions must leave the field absent, so an
// older consumer sees exactly what it saw before.
func TestContributions_OmittedWhenNil(t *testing.T) {
	raw, err := json.Marshal(&queue.RunMessage{V: queue.SchemaVersion, RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if _, present := generic["contributions"]; present {
		t.Error("contributions must be omitted from the wire when nil")
	}
}
