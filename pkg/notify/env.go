package notify

import (
	"os"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// NewFromEnv builds the deployment's completion notifier from the
// conventional environment — the one construction every lane that can
// settle a run (the in-process runview, the cloud runner, the retry
// sweeper that abandons a retry) shares so a callback signed or relaxed
// for one of them behaves identically on the others:
//
//   - ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE=1 relaxes the SSRF guard
//     for self-hosted deployments whose callback receiver lives on a
//     private network alongside iterion; off by default (a cloud runner
//     must not gateway into a private network).
//   - ITERION_COMPLETION_WEBHOOK_SECRET, when set, HMAC-signs every
//     outbound payload (X-Iterion-Signature) so receivers can
//     authenticate the delivery. Empty = unsigned (a receiver expecting a
//     signature must reject the request).
func NewFromEnv(logger *iterlog.Logger, timeout time.Duration) *Notifier {
	return New(logger, timeout,
		WithAllowPrivate(os.Getenv("ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE") == "1"),
		WithSigningSecret(os.Getenv("ITERION_COMPLETION_WEBHOOK_SECRET")))
}
