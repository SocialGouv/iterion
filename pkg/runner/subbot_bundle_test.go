package runner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/subbottest"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
)

func TestPodSubbotBundleResourceScope(t *testing.T) {
	for _, kind := range []string{"bundle", "member", "bare"} {
		t.Run(kind, func(t *testing.T) {
			f := subbottest.New(t, kind)
			r, st := subbotTestRunner(t)
			wf, hash, b, err := runview.CompileWorkflowPath(f.Parent)
			if err != nil {
				t.Fatal(err)
			}
			msg := &queue.RunMessage{RunID: "parent"}
			ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithWorkDir(f.Workspace))
			defer func() { _ = ex.Close() }()
			eng := runtime.New(wf, st, ex, runtime.WithFilePath(f.Parent), runtime.WithWorkflowHash(hash), runtime.WithBundle(b), runtime.WithWorkDir(f.Workspace), runtime.WithSandboxOverride("none"), runtime.WithSubbotRunner(r.subbotRunnerFor(msg, filepath.Dir(f.Parent), f.Workspace, iterlog.Nop())))
			ctx := context.Background()
			if err := eng.Run(ctx, msg.RunID, nil); err != nil {
				t.Fatal(err)
			}
			f.Assert(t, ctx, st, msg.RunID)
		})
	}
}
