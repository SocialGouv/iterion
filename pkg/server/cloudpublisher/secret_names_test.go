package cloudpublisher

import (
	"slices"
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestTheLaunchResolvesOnlyValuelessSecretDeclarations holds the publisher's
// two secret-name selections to ir.Secret.HasInlineValue: a declaration with
// an inline value is resolved by the run from its own expression, so the
// store, a bot binding or a launch override never fills it, and it is never a
// name the required-secret gate waits on. A value of blanks is no value.
func TestTheLaunchResolvesOnlyValuelessSecretDeclarations(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"forge_token": {Name: "forge_token", As: "file", Optional: true},
		"api_key":     {Name: "api_key"},
		"inline":      {Name: "inline", Value: "{{vars.token}}"},
		"blank":       {Name: "blank", Value: "  "},
	}}

	resolved := genericSecretNamesForWorkflow(wf)
	sort.Strings(resolved)
	if want := []string{"api_key", "blank", "forge_token"}; !slices.Equal(resolved, want) {
		t.Errorf("the launch resolves %v from the store, bindings and overrides; want %v — a declaration with an inline value carries its own, and a valueless one is the only one an override can fill", resolved, want)
	}

	required := requiredSecretNamesForWorkflow(wf)
	sort.Strings(required)
	if want := []string{"api_key", "blank"}; !slices.Equal(required, want) {
		t.Errorf("the required-secret gate waits on %v; want %v — an optional declaration and one with an inline value are never refused for an empty resolution", required, want)
	}
}
