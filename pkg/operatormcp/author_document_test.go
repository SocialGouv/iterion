package operatormcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// local_run and local_resume refuse an author document on their own, before
// the validate pre-flight and before any run doc or detached process: the
// validate tool reads a draft, a run must never follow from one.
func TestLocalRunAndResumeRefuseAnAuthorDocument(t *testing.T) {
	s := newTestServer(t)
	if err := os.WriteFile(filepath.Join(s.WorkDir, "draft.bot.yaml"), []byte("dsl: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, s, "local_run", `{"file_path":"draft.bot.yaml"}`)
	if !isErr || !strings.Contains(text, "author document") {
		t.Fatalf("local_run on a draft: isErr=%v text=%s", isErr, text)
	}
	st, err := s.store()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ids, err := st.ListRuns(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("a refused launch left run docs behind: %v (%v)", ids, err)
	}

	if _, err := st.CreateRun(ctx, "run-res", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunStatus(ctx, "run-res", store.RunStatusFailedResumable, "boom"); err != nil {
		t.Fatal(err)
	}
	text, isErr = call(t, s, "local_resume", `{"run_id":"run-res","file_path":"draft.bot.yaml"}`)
	if !isErr || !strings.Contains(text, "author document") {
		t.Fatalf("local_resume on a draft: isErr=%v text=%s", isErr, text)
	}
	if r, err := st.LoadRun(ctx, "run-res"); err != nil || r.Status != store.RunStatusFailedResumable {
		t.Fatalf("a refused resume changed the run: %+v (%v)", r, err)
	}
}
