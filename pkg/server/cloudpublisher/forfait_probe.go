package cloudpublisher

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/forfait"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// UsageProbe refreshes a forfait's usage windows from the provider itself,
// given the credential payload the walk holds. The walk calls it only when
// the credential's stored readings are STALE BUT SUGGESTIVE — old enough
// that the trust window no longer believes them, yet saying the window was
// closed when they were taken — so the decision rests on what the provider
// reports now rather than on a memory or on a guess. Best effort: an error
// means "cannot tell", and the walk then trusts the credential.
type UsageProbe func(ctx context.Context, payload []byte) ([]usagecap.Reading, error)

// usageProbeTimeout bounds one refresh on the launch path.
const usageProbeTimeout = 5 * time.Second

// AnthropicForfaitProbe reads the Anthropic OAuth usage endpoint with the
// forfait's own access token (pkg/backend/forfait) and maps every window it
// reports onto a reading keyed like the claude_code delegate's own
// telemetry. A window at 100% is recorded as the provider's refusal. A
// credential the endpoint refuses (a setup-token lacks the user:profile
// scope) yields an error — the walk falls back to trusting it.
func AnthropicForfaitProbe(timeout time.Duration) UsageProbe {
	client := &http.Client{Timeout: timeout}
	return func(ctx context.Context, payload []byte) ([]usagecap.Reading, error) {
		token, ok := forfait.AccessTokenFromCredentialsJSON(payload)
		if !ok {
			return nil, errors.New("credential payload carries no access token")
		}
		windows, err := forfait.FetchWindows(ctx, token, client)
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		out := make([]usagecap.Reading, 0, len(windows))
		for _, w := range windows {
			status := usagecap.StatusAllowed
			if w.Utilization >= 1 {
				status = usagecap.StatusRejected
			}
			out = append(out, usagecap.Reading{
				Window:      usagecap.Window(w.Window),
				Utilization: w.Utilization,
				Status:      status,
				ResetsAt:    w.ResetsAt,
				ObservedAt:  now,
				Source:      "anthropic-oauth",
			})
		}
		return out, nil
	}
}
