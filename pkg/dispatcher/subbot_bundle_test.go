package dispatcher

import (
	"context"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/internal/subbottest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestDispatchSubbotBundleResourceScope(t *testing.T) {
	for _, bare := range []bool{false, true} {
		t.Run(fmt.Sprint(bare), func(t *testing.T) {
			f := subbottest.New(t, bare)
			t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
			r, err := NewEngineRunner(f.Parent, iterlog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			ctx := context.Background()
			if err := r.Dispatch(ctx, DispatchSpec{RunID: "parent", WorkspacePath: f.Workspace, StoreDir: f.Store, Issue: &IssueRef{ID: "native:parent", Identifier: "parent", Title: "bundle scope"}}); err != nil {
				t.Fatal(err)
			}
			st, err := store.New(f.Store)
			if err != nil {
				t.Fatal(err)
			}
			f.Assert(t, ctx, st, "parent")
		})
	}
}
