package cloudpublisher

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"strings"
	"testing"
)

type contextLaunchStore struct{ store.RunStore }

func (s contextLaunchStore) ContextIdentity(context.Context) (store.ContextRef, error) {
	return store.ContextRef{}, errors.New("test fixture identity failure")
}
func TestExecutionContextCloudRefusesBeforePersistence(t *testing.T) {
	for _, identityFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid context", true: "identity error"}[identityFailure], func(t *testing.T) {
			p, st, ctx := sourceTestPublisher(t)
			published := 0
			p.publishRun = func(context.Context, *queue.RunMessage) error { published++; return nil }
			spec := runview.LaunchSpec{Source: "workflow wf:\n  entry: survey\n  survey -> done\n", FilePath: "wf.bot"}
			expected := "unsupported execution context version"
			if identityFailure {
				p.store = contextLaunchStore{st}
				expected = "test fixture identity failure"
			} else {
				spec.ExecutionContext = &store.ExecutionContext{Version: 2}
			}
			_, err := p.SubmitLaunch(ctx, "context-launch-cloud-refusal", spec, &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "h"})
			doc, loadErr := st.LoadRun(ctx, "context-launch-cloud-refusal")
			t.Logf("error=%v; published=%d; persisted=%v; loadErr=%v", err, published, doc != nil, loadErr)
			if err == nil || !strings.Contains(err.Error(), expected) || published != 0 || doc != nil {
				t.Fatal("cloud refusal contract failed")
			}
		})
	}
}
