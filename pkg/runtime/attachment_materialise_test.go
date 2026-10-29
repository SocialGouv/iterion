package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2086. On a store with no filesystem root (cloud) nothing is there to
// bind or seed: the engine fetches the attachments the run holds into a
// directory of the run's own before the sandbox, hands nodes a path into
// it, and removes it with the sandbox. A fetch that fails is the run's
// failure, typed.

// noRootStore is a run store with no filesystem root: the cloud shape.
type noRootStore struct{ store.RunStore }

func (noRootStore) Root() string { return "" }

// openFailingStore cannot hand its attachments over.
type openFailingStore struct{ store.RunStore }

func (openFailingStore) Root() string { return "" }

func (openFailingStore) OpenAttachment(context.Context, string, string) (io.ReadCloser, store.AttachmentRecord, error) {
	return nil, store.AttachmentRecord{}, errors.New("the bucket is unreachable")
}

func engineOn(t *testing.T, s store.RunStore) *Engine {
	t.Helper()
	t.Setenv("ITERION_MODE", "local")
	return New(&ir.Workflow{Name: "wf"}, s, newStubExecutor(), WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
}

func TestAStoreWithNoRootMaterialisesTheAttachmentsBeforeTheSandbox(t *testing.T) {
	ctx := context.Background()
	fs := tmpStore(t)
	const runID = "run-materialise"
	if _, err := fs.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteAttachment(ctx, runID, store.AttachmentRecord{Name: "spec", OriginalFilename: "spec.md", MIME: "text/markdown"}, strings.NewReader("# spec")); err != nil {
		t.Fatal(err)
	}
	recs, err := fs.ListAttachments(ctx, runID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("ListAttachments = %v, %v", recs, err)
	}
	rec := recs[0]

	t.Run("the run's attachments are fetched, read-only, and handed to nodes by path", func(t *testing.T) {
		e := engineOn(t, noRootStore{fs})
		cleanup, err := e.startSandbox(ctx, runID, e.workDir, "", nil)
		if err != nil {
			t.Fatalf("startSandbox: %v", err)
		}
		dir := e.attachmentsLocalDir
		if dir == "" {
			t.Fatal("no directory was materialised for a run that holds an attachment")
		}
		file := filepath.Join(dir, "spec", "spec.md")
		got, err := os.ReadFile(file)
		if err != nil || string(got) != "# spec" {
			t.Fatalf("the attachment's bytes are not under the directory: %q, %v", got, err)
		}
		if info, _ := os.Stat(file); info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("the materialised file is writable: %v", info.Mode())
		}
		if p := e.attachmentPath(rec); p != file {
			t.Fatalf("attachmentPath = %q, want the materialised file %q", p, file)
		}
		if p := e.attachmentHostPath(rec); p != file {
			t.Fatalf("attachmentHostPath = %q, want %q", p, file)
		}
		cleanup()
		if e.attachmentsLocalDir != "" {
			t.Fatal("the directory is still announced after the cleanup")
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the directory survived the cleanup: %v", err)
		}
		if p := e.attachmentPath(rec); p != "" {
			t.Fatalf("after the cleanup attachmentPath = %q, want the URL accessor alone", p)
		}
	})
	t.Run("a fetch that fails is the run's typed failure", func(t *testing.T) {
		e := engineOn(t, openFailingStore{fs})
		_, err := e.startSandbox(ctx, runID, e.workDir, "", nil)
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Code != ErrCodeAttachmentsUnfetchable || !strings.Contains(rt.Message, "spec") {
			t.Fatalf("err = %v, want %s naming the attachment", err, ErrCodeAttachmentsUnfetchable)
		}
		if strings.Count(rt.Error(), "the bucket is unreachable") != 1 {
			t.Fatalf("the cause is printed %d times: %s", strings.Count(rt.Error(), "the bucket is unreachable"), rt.Error())
		}
		if e.attachmentsLocalDir != "" {
			t.Fatal("a failed materialisation announced a directory")
		}
	})
	t.Run("a run with no attachments materialises nothing", func(t *testing.T) {
		const bare = "run-bare"
		if _, err := fs.CreateRun(ctx, bare, "wf", nil); err != nil {
			t.Fatal(err)
		}
		e := engineOn(t, noRootStore{fs})
		cleanup, err := e.startSandbox(ctx, bare, e.workDir, "", nil)
		if err != nil {
			t.Fatalf("startSandbox: %v", err)
		}
		defer cleanup()
		if e.attachmentsLocalDir != "" {
			t.Fatalf("a run with no attachments got a directory: %s", e.attachmentsLocalDir)
		}
	})
}
