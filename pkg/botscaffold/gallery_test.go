package botscaffold

import (
	"strings"
	"testing"
)

func TestSpecFromTemplate_DefaultsToBlank(t *testing.T) {
	for _, id := range []string{"", "  "} {
		spec, err := SpecFromTemplate(id, Overrides{Slug: "b"})
		if err != nil {
			t.Fatalf("SpecFromTemplate(%q): %v", id, err)
		}
		blank, _ := TemplateByID(DefaultTemplateID)
		if spec.Instructions != blank.Spec.Instructions {
			t.Errorf("SpecFromTemplate(%q) did not fall back to the blank template", id)
		}
	}
}

func TestSpecFromTemplate_UnknownIDListsAvailable(t *testing.T) {
	_, err := SpecFromTemplate("nope", Overrides{Slug: "b"})
	if err == nil {
		t.Fatal("SpecFromTemplate(\"nope\") = nil, want error")
	}
	// The error must name the alternatives — a bare rejection sends the
	// operator hunting through --help.
	for _, id := range TemplateIDs() {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error %q does not mention available template %q", err, id)
		}
	}
}

func TestSpecFromTemplate_OverridesWinButOnlyWhenSet(t *testing.T) {
	// docs-writer ships Worktree=true and its own description.
	spec, err := SpecFromTemplate("docs-writer", Overrides{Slug: "d1"})
	if err != nil {
		t.Fatalf("SpecFromTemplate: %v", err)
	}
	if !spec.Worktree {
		t.Error("docs-writer lost its Worktree=true default")
	}
	if spec.Description == "" {
		t.Error("docs-writer lost its description")
	}

	off := false
	spec, err = SpecFromTemplate("docs-writer", Overrides{
		Slug:        "d2",
		Description: "mine",
		Worktree:    &off,
	})
	if err != nil {
		t.Fatalf("SpecFromTemplate: %v", err)
	}
	if spec.Worktree {
		t.Error("an explicit false dial did not override the template")
	}
	if spec.Description != "mine" {
		t.Errorf("Description = %q, want the override to win", spec.Description)
	}
	if spec.Instructions == "" {
		t.Error("un-overridden field was cleared")
	}
}

func TestSpecValidate_NormalizesSlug(t *testing.T) {
	// Both creation surfaces rely on Validate to trim, so a caller that
	// passes a padded slug must not get `invalid slug " my-bot"`.
	spec := Spec{Slug: "  my-bot\t", Instructions: "Do the thing."}
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if spec.Slug != "my-bot" {
		t.Errorf("Slug = %q, want it trimmed to %q", spec.Slug, "my-bot")
	}
}
