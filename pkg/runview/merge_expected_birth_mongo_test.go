package runview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// TestMergeExpectedBirthNativeMongo runs the merge expected-birth witnesses
// of both doors against the native Mongo store; CI's mongo-conformance job
// supplies the URI.
func TestMergeExpectedBirthNativeMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	for _, tc := range []struct {
		name     string
		scenario func(*testing.T, snapshotOriginNative, context.Context)
	}{
		{"reconcile", mergeBirthReconcileScenario},
		{"perform", mergeBirthPerformScenario},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithIdentity(t.Context(), "merge-birth-tenant", "owner")
			s, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("merge_birth_%d", time.Now().UnixNano()), Blob: snapshotExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_ = s.DB().Drop(c)
				_ = s.Close(c)
			})
			tc.scenario(t, s, ctx)
		})
	}
}
