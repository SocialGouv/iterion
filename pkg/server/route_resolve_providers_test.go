package server

// Witness 6 (end-to-end over slice 2's Validate): a level whose routing
// block carries `providers: []` — the "forbid everything" spelling —
// refuses the LAUNCH at the fold, naming the level; the empty list is not
// an inherit (nil is), so a mis-typed allowlist cannot disarm tenancy by
// looking empty. The refusal is Validate's, re-used; this pins that it
// reaches the launch surface through the one fold.

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
)

func TestResolveRunLLMRoutePolicy_EmptyProvidersListRefusesTheLaunch(t *testing.T) {
	s := routeResolveServer(t, &platformcfg.PlatformCredentials{Routing: &llmroute.Policy{
		Providers: []string{},
	}})
	_, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "")
	if err == nil {
		t.Fatal("an empty providers list must refuse the launch — omit it to inherit")
	}
	if !strings.Contains(err.Error(), "providers is empty") {
		t.Errorf("error = %v, want the Validate refusal naming the field", err)
	}
	if !strings.Contains(err.Error(), "platform") {
		t.Errorf("error = %v, want it to name the level that set the list", err)
	}
}
