package runview

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newContextLaunchService builds a service whose binding store holds the
// given bindings, with the fetch seam faked to a checkout of the given
// files — the resolver's git half is pluginsource.Fetcher's, tested there.
func newContextLaunchService(t *testing.T, bindings []secrets.BotSecretBinding, checkoutFiles map[string]string) (*Service, *secrets.MemoryBotSecretBindingStore) {
	t.Helper()
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	bs := secrets.NewMemoryBotSecretBindingStore()
	ctx := store.WithTenant(context.Background(), "team-1")
	for _, b := range bindings {
		if err := bs.Create(ctx, b); err != nil {
			t.Fatalf("seed binding: %v", err)
		}
	}
	svc.botContexts = &BotContextResolver{
		Bindings: bs,
		Fetch: func(_ context.Context, b secrets.BotSecretBinding) (string, error) {
			dir := t.TempDir()
			for rel, content := range checkoutFiles {
				writeFile(t, dir, rel, content)
			}
			return dir, nil
		},
	}
	return svc, bs
}

func contextLaunchWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name: "doki",
		Attachments: map[string]*ir.Attachment{
			"handbook": {},
		},
	}
}

func contextLaunchBinding(id, name, path string) secrets.BotSecretBinding {
	return secrets.BotSecretBinding{
		ID:                    id,
		TenantID:              "team-1",
		BotID:                 "doki",
		Kind:                  secrets.BindingKindContext,
		SecretNameForWorkflow: name,
		Context:               &secrets.ContextSource{GitURL: "https://example.invalid/o/c.git", Ref: "v1.0.0", Path: path},
	}
}

// launchPromote runs the composition and invokes what it produced against a
// fresh run row, the way every hand-off path does.
func launchPromote(t *testing.T, svc *Service, ctx context.Context, spec *LaunchSpec, wf *ir.Workflow) []store.AttachmentRecord {
	t.Helper()
	runID := "run-contexts"
	if _, err := svc.store.CreateRun(ctx, runID, wf.Name, nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	svc.composeBoundContexts(ctx, runID, spec, wf)
	if spec.AttachmentPromote == nil {
		return nil
	}
	if err := spec.AttachmentPromote(ctx, runID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	held, err := svc.store.ListAttachments(ctx, runID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	return held
}

func TestComposeBoundContexts_DeliversTheBoundDocumentUnderTheDeclaredName(t *testing.T) {
	svc, _ := newContextLaunchService(t,
		[]secrets.BotSecretBinding{contextLaunchBinding("b1", "handbook", "docs/handbook.md")},
		map[string]string{"docs/handbook.md": "# the handbook\n"})
	ctx := store.WithTenant(context.Background(), "team-1")
	spec := &LaunchSpec{BotID: "doki"}

	held := launchPromote(t, svc, ctx, spec, contextLaunchWorkflow())
	if len(held) != 1 {
		t.Fatalf("attachments = %+v, want the one bound document", held)
	}
	if held[0].Name != "handbook" || held[0].OriginalFilename != "handbook.md" {
		t.Errorf("attachment = %+v", held[0])
	}
	rc, _, err := svc.store.OpenAttachment(ctx, "run-contexts", "handbook")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "# the handbook\n" {
		t.Errorf("stored bytes = %q", body)
	}
}

func TestComposeBoundContexts_ALaunchUploadWinsOverTheBoundContext(t *testing.T) {
	svc, _ := newContextLaunchService(t,
		[]secrets.BotSecretBinding{contextLaunchBinding("b1", "handbook", "h.md")},
		map[string]string{"h.md": "the operator default"})
	ctx := store.WithTenant(context.Background(), "team-1")
	// The launch's own upload rides the pre-existing promote, composed AFTER
	// the contexts: ADR-147 D4's author < operator < launch, decided by the
	// write order since WriteAttachment overwrites unconditionally.
	spec := &LaunchSpec{BotID: "doki"}
	spec.AttachmentPromote = func(ctx context.Context, runID string) error {
		return svc.store.WriteAttachment(ctx, runID, store.AttachmentRecord{
			Name: "handbook", OriginalFilename: "handbook.pdf", MIME: "application/pdf",
		}, bytes.NewReader([]byte("the launch's own file")))
	}

	held := launchPromote(t, svc, ctx, spec, contextLaunchWorkflow())
	if len(held) != 1 {
		t.Fatalf("attachments = %+v, want one", held)
	}
	rc, _, err := svc.store.OpenAttachment(ctx, "run-contexts", "handbook")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "the launch's own file" {
		t.Errorf("the launch's upload did not win: %q", body)
	}
}

func TestComposeBoundContexts_NoResolverNoTenantNoDeclarationNoOp(t *testing.T) {
	ctx := store.WithTenant(context.Background(), "team-1")
	wf := contextLaunchWorkflow()

	// No resolver wired: the pre-seam behaviour, byte for byte.
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	spec := &LaunchSpec{BotID: "doki"}
	svc.composeBoundContexts(ctx, "run-x", spec, wf)
	if spec.AttachmentPromote != nil {
		t.Error("a service without the resolver gained a promote")
	}

	// A tenant-less launch binds nothing: bindings are org policy.
	svc2, _ := newContextLaunchService(t,
		[]secrets.BotSecretBinding{contextLaunchBinding("b1", "handbook", "h.md")},
		map[string]string{"h.md": "x"})
	spec2 := &LaunchSpec{BotID: "doki"}
	svc2.composeBoundContexts(context.Background(), "run-y", spec2, wf)
	if spec2.AttachmentPromote != nil {
		t.Error("a tenant-less launch gained a promote")
	}

	// A workflow that declares no attachments block delivers none — but the
	// resolver, not the composition, is what judges: the binding is skipped
	// and flagged DEGRADED on its record (a misconfiguration the operator
	// can see), never silently passed over.
	spec3 := &LaunchSpec{BotID: "doki"}
	bare := &ir.Workflow{Name: "doki"}
	svc2.composeBoundContexts(ctx, "run-z", spec3, bare)
	if spec3.AttachmentPromote != nil {
		t.Error("a workflow with no attachments block gained a promote")
	}
	got, err := svc2.botContexts.Bindings.Get(ctx, "b1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Degraded() {
		t.Errorf("a context binding on a workflow with no attachments block must surface degraded — it was silently skipped: %+v", got)
	}
}
