package cloudpublisher

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-131 §6, the refusal's remedy: a launch takes its pin from the webhook
// or trigger subscription that launched it, so repinning or clearing that
// entry is what unblocks the next launch. A resume replays the pin the run
// froze at its launch and never reads the webhook again — after the key's
// deletion the credential sweep has already cleared the webhook's pin — so
// the refusal of a replay names the run's own frozen pin and a relaunch as
// the remedy, never a webhook edit that cannot reach the run.
func TestGatewayPin_TheRefusalOfAReplayNamesTheRunsFrozenPin(t *testing.T) {
	for _, row := range []struct {
		name   string
		resume bool
		want   []string
		never  []string
	}{
		{"a launch", false,
			[]string{"repin or clear the openai_compatible entry of key_overrides on the webhook or trigger subscription that launched the run"},
			[]string{"relaunch"}},
		{"a resume", true,
			[]string{"this run's own pin, frozen at its launch", "relaunch the run"},
			[]string{"repin or clear", "on the webhook or trigger subscription that launched the run"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, _ := gatewayKeyPublisher(t)
			seedOtherGatewayKey(t, p)
			pin := secrets.NewApiKeyID()
			boundary := boundaryDerived
			if row.resume {
				boundary = boundaryFrozen(&store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: pin, GatewayHost: gwHost})
			}
			_, err := resolvePinnedRun(t, p, "run-pin-replay", gatewayOnlyWorkflow(), gatewayPin(pin), nil, boundary)
			var refusal *GatewayPinRefusedError
			if !errors.As(err, &refusal) || refusal.Cause != GatewayPinMissing {
				t.Fatalf("err = %v, want the missing pin's *GatewayPinRefusedError", err)
			}
			msg := err.Error()
			for _, want := range row.want {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal %q does not say %q", msg, want)
				}
			}
			for _, never := range row.never {
				if strings.Contains(msg, never) {
					t.Errorf("refusal %q says %q", msg, never)
				}
			}
		})
	}
}
