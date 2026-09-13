package runview

import (
	"context"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/internal/subbottest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestServiceSubbotBundleResourceScope(t *testing.T) {
	for _, bare := range []bool{false, true} {
		t.Run(fmt.Sprint(bare), func(t *testing.T) {
			f := subbottest.New(t, bare)
			t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
			s, err := NewService(f.Store, WithLogger(iterlog.Nop()), WithWorkDir(f.Workspace))
			if err != nil {
				t.Fatal(err)
			}
			defer stopService(t, s)
			ctx := context.Background()
			res, err := s.Launch(ctx, LaunchSpec{FilePath: f.Parent})
			if err != nil {
				t.Fatal(err)
			}
			awaitRunCompletion(t, res.Done, "parent did not finish")
			f.Assert(t, ctx, s.store, res.RunID)
		})
	}
}
