package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
)

func TestValidateModelOverrides(t *testing.T) {
	ok := []runview.ModelOverrideEntry{
		{Selector: "*", Model: "anthropic/claude-opus-5"},
		{Selector: "agent", Backend: "claw", Effort: "high"},
		// ultracode is a mode, not a wire value, but it IS an accepted level.
		{Selector: "reviewer_*", Effort: "ultracode"},
		// A model iterion has never heard of stays legal: model names are host
		// state this process cannot enumerate.
		{Selector: "fix_*", Model: "somevendor/some-model-9"},
	}
	if err := validateModelOverrides(ok); err != nil {
		t.Fatalf("valid overrides rejected: %v", err)
	}

	// The effort reaches the provider verbatim, so an unknown level must die at
	// admission — not as an API error on the run's first node.
	err := validateModelOverrides([]runview.ModelOverrideEntry{
		{Selector: "*", Effort: "turbo"},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown reasoning effort")
	}
	if !strings.Contains(err.Error(), "turbo") {
		t.Errorf("error should name the offending value, got %q", err)
	}
	// And it should say what IS accepted, so the caller can fix it in one go.
	if !strings.Contains(err.Error(), "xhigh") {
		t.Errorf("error should list the valid levels, got %q", err)
	}

	err = validateModelOverrides([]runview.ModelOverrideEntry{
		{Selector: "*", Backend: "clua"},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown backend")
	}
	if !strings.Contains(err.Error(), "claude_code") {
		t.Errorf("error should list the valid backends, got %q", err)
	}

	if err := validateModelOverrides([]runview.ModelOverrideEntry{
		{Selector: "  ", Model: "anthropic/claude-opus-5"},
	}); err == nil {
		t.Error("expected an error for an empty selector")
	}
}
