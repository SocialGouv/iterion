package runview

import (
	"context"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

const fillerBotSrc = "tool work:\n" +
	"  command: `printf stored`\n" +
	"  output: out\n" +
	"\n" +
	"schema out:\n" +
	"  ok: bool\n" +
	"\n" +
	"workflow main:\n" +
	"  entry: work\n" +
	"  work -> done\n"

// Every Resume surface that carries no source — answer-human (HTTP + WS)
// and the ADR-081 async auto-resume — builds a bare ResumeSpec. The
// filler seam must re-derive Source + BotBundle from the persisted run,
// so a stored-bot run resumes on ITS bundle: without it the compile reads
// the pod's baked twin (hash mismatch, or silently stale resources).
func TestResume_BareSpecInvokesSourceFiller(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()

	// The run's hash matches the STORED source; the persisted FilePath
	// deliberately does not exist on this "pod".
	_, wantHash, err := compileForLaunch("", fillerBotSrc, "")
	if err != nil {
		t.Fatalf("compile stored source: %v", err)
	}
	if err := st.SaveRun(ctx, &store.Run{
		FormatVersion: store.RunFormatVersion,
		ID:            "run-filler",
		WorkflowName:  "main",
		WorkflowHash:  wantHash,
		FilePath:      "/opt/iterion/bots/stored-bot/main.bot",
		Status:        store.RunStatusFailedResumable,
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	pub := &stubLaunchPublisher{}
	filled := 0
	svc, err := NewService(dir,
		WithLogger(iterlog.Nop()),
		WithStore(st),
		WithLaunchPublisher(pub),
		WithResumeSourceFiller(func(_ context.Context, run *store.Run, spec *ResumeSpec) (func(), error) {
			filled++
			if run.ID != "run-filler" {
				t.Errorf("filler saw run %q", run.ID)
			}
			spec.Source = fillerBotSrc
			spec.BotBundle = &BotBundleRef{TenantID: "platform:", Slug: "stored-bot", Version: 3}
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Resume(ctx, ResumeSpec{RunID: "run-filler", FilePath: "/opt/iterion/bots/stored-bot/main.bot"})
	if err != nil {
		t.Fatalf("bare resume with filler: %v", err)
	}
	<-res.Done
	if filled != 1 {
		t.Fatalf("filler invoked %d times, want 1", filled)
	}

	// Red-proof of the pre-seam behavior: the same bare resume WITHOUT the
	// filler must fail — the pod cannot read the persisted FilePath, which
	// is exactly the failure the seam exists to close.
	if err := st.UpdateRunStatus(ctx, "run-filler", store.RunStatusFailedResumable, ""); err != nil {
		t.Fatalf("re-arm run: %v", err)
	}
	bare, err := NewService(dir, WithLogger(iterlog.Nop()), WithStore(st), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService (no filler): %v", err)
	}
	if _, err := bare.Resume(ctx, ResumeSpec{RunID: "run-filler", FilePath: "/opt/iterion/bots/stored-bot/main.bot"}); err == nil {
		t.Fatal("bare resume without the filler unexpectedly succeeded — the red premise no longer holds")
	}
}

// A caller that already resolved (the resume handler, the retry sweeper)
// must NOT be second-guessed: any of Source/BundleDir/BotBundle present
// skips the filler.
func TestResume_ResolvedSpecSkipsFiller(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	_, wantHash, err := compileForLaunch("", fillerBotSrc, "")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := st.SaveRun(ctx, &store.Run{
		FormatVersion: store.RunFormatVersion,
		ID:            "run-resolved",
		WorkflowName:  "main",
		WorkflowHash:  wantHash,
		FilePath:      "x.bot",
		Status:        store.RunStatusFailedResumable,
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}
	svc, err := NewService(dir,
		WithLogger(iterlog.Nop()),
		WithStore(st),
		WithLaunchPublisher(&stubLaunchPublisher{}),
		WithResumeSourceFiller(func(context.Context, *store.Run, *ResumeSpec) (func(), error) {
			t.Error("filler invoked for an already-resolved spec")
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Resume(ctx, ResumeSpec{RunID: "run-resolved", FilePath: "x.bot", Source: fillerBotSrc})
	if err != nil {
		t.Fatalf("resolved resume: %v", err)
	}
	<-res.Done
}
