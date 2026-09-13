package cli

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/SocialGouv/iterion/internal/subbottest"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestCLISubbotBundleResourceScope(t *testing.T) {
	for _, bare := range []bool{false, true} {
		t.Run(fmt.Sprint(bare), func(t *testing.T) {
			f := subbottest.New(t, bare)
			t.Chdir(f.Workspace)
			t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
			ctx := context.Background()
			if err := RunRun(ctx, RunOptions{File: f.Parent, StoreDir: f.Store, RunID: "parent"}, &Printer{W: io.Discard, Format: OutputJSON}); err != nil {
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
