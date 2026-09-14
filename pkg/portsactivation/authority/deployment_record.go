package authority

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
)

// ReadDeploymentRecord binds the operator's Secret to the server's configured
// namespace and queue scope before any live observation. The returned source
// contains private authorization material and must stay inside the privileged
// server adapter. A matching record is an assertion, not an activation proof.
func ReadDeploymentRecord(ctx context.Context, kubectlBinary, kubeContext, authorityRef string,
	namespaces []string, expectedQueue natsconfig.QueueTopology) (*Record, *SecretSource, error) {
	parts := strings.Split(authorityRef, "/")
	if len(parts) != 2 || len(namespaces) == 0 || len(namespaces) > 16 || expectedQueue.Validate() != nil {
		return nil, nil, fmt.Errorf("distributed authority has an invalid configured scope")
	}
	source, err := ReadAuthoritySecret(ctx, kubectlBinary, kubeContext, parts[0], parts[1])
	if err != nil {
		return nil, nil, err
	}
	record, err := ParseRecord(*source)
	if err != nil {
		return nil, nil, err
	}
	if !sameStrings(record.Namespaces, namespaces) || record.Queue != expectedQueue {
		return nil, nil, fmt.Errorf("distributed authority record differs from configured Kubernetes or NATS queue scope")
	}
	return record, source, nil
}
