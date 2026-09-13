package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestPreflightResume_CompilesTheRecordedBundle: a run launched from the
// studio's file picker has as FilePath the store's materialised copy of its
// main.bot — `<hash>-main.bot`, a name no promotion recognises — and as
// BundlePath the bundle the engine was handed. A resume that resolved no
// bundle of its own compiles against that recorded directory: the prompts
// in scope, the hash the run recorded. Without it the preflight refused
// the run with C003 (or a hash mismatch) it could never satisfy.
func TestPreflightResume_CompilesTheRecordedBundle(t *testing.T) {
	bundleDir := promptedBundle(t)
	opened, err := bundle.OpenDir(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	_, wantHash, err := CompileBundleWorkflow(opened.IterPath, opened)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(opened.IterPath)
	if err != nil {
		t.Fatal(err)
	}
	// The materialised copy: same bytes, a name and a directory that are
	// nobody's bundle.
	copyPath := filepath.Join(t.TempDir(), "a1b2c3d4e5f6-main.bot")
	if err := os.WriteFile(copyPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := compileForLaunch(copyPath, "", ""); err == nil || !strings.Contains(err.Error(), "C003") {
		t.Fatalf("the materialised copy compiled alone (err=%v); the fixture no longer arms the case", err)
	}

	storeDir := t.TempDir()
	st, err := store.New(storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	if err := st.SaveRun(ctx, &store.Run{
		FormatVersion: store.RunFormatVersion,
		ID:            "run-recorded-bundle",
		WorkflowName:  "w",
		WorkflowHash:  wantHash,
		FilePath:      copyPath,
		BundlePath:    bundleDir,
		Status:        store.RunStatusFailedResumable,
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}
	svc, err := NewService(storeDir, WithLogger(iterlog.Nop()), WithStore(st))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.PreflightResume(ctx, ResumeSpec{RunID: "run-recorded-bundle", FilePath: copyPath}); err != nil {
		t.Fatalf("preflight on the recorded bundle: %v", err)
	}

	// A resume that resolved its own bundle, or a stored-bot ref, is left
	// alone; a recorded path that is not a directory (a .botz archive, a
	// pod path this process cannot see) does not qualify.
	r := &store.Run{BundlePath: bundleDir}
	if got := resumeBundleDir(r, ResumeSpec{BundleDir: "/elsewhere"}); got != "/elsewhere" {
		t.Fatalf("a resolved BundleDir was replaced by %q", got)
	}
	if got := resumeBundleDir(r, ResumeSpec{BotBundle: &BotBundleRef{Slug: "x"}}); got != "" {
		t.Fatalf("a stored-bot resume derived %q", got)
	}
	if got := resumeBundleDir(&store.Run{BundlePath: filepath.Join(bundleDir, "main.bot")}, ResumeSpec{}); got != "" {
		t.Fatalf("a file path was taken as a bundle dir: %q", got)
	}
	if got := resumeBundleDir(&store.Run{BundlePath: "/nowhere/on/this/host"}, ResumeSpec{}); got != "" {
		t.Fatalf("an unreachable path was taken as a bundle dir: %q", got)
	}
}
