package runner

import (
	"context"
	"errors"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// ADR-123: a "dek"-stamped record whose message carries no per-run key
// is corrupt — it refuses with the typed error, never executed.
func TestInjectCredentials_DekRecordWithoutDEKRefuses(t *testing.T) {
	rs := secrets.NewMemoryRunSecretsStore()
	dek, err := secrets.NewRunBundleDEK()
	if err != nil {
		t.Fatal(err)
	}
	dekSealer, err := secrets.NewAESGCMSealer(dek)
	if err != nil {
		t.Fatal(err)
	}
	sealed, keyID, err := secrets.SealRunBundle(dekSealer, "team-a", "", "run-1", secrets.RunBundle{GenericSecrets: map[string]string{"x": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if keyID != secrets.DEKKeyID {
		t.Fatalf("key id %q, want the dek scheme", keyID)
	}
	if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-1", TenantID: "team-a", RunID: "run-1", KeyID: keyID, SealedBundle: sealed}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs}}
	_, _, err = r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-1", TenantID: "team-a", SecretsRef: "ref-1"})
	if !errors.Is(err, secrets.ErrBundleDEKMissing) {
		t.Fatalf("err = %v, want ErrBundleDEKMissing", err)
	}
}
