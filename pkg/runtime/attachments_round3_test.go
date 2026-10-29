package runtime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Round 3 of the adversarial loop on #2086, the runtime side.

// routeStub answers the executor's own reading of a node's routes.
type routeStub struct {
	primary   string
	fallbacks []string
}

func (r routeStub) EffectiveBackendName(ir.Node) string        { return r.primary }
func (r routeStub) EffectiveFallbackBackends(ir.Node) []string { return r.fallbacks }

// The exemption is the executor's rule on the executor's reading: the
// reference text the executor walks (a "!" spelling is not an image block,
// it renders as text) and the routes the executor will dispatch (a
// --backend override, a fallback spelled through vars).
func TestTheImageBlockRuleIsTheExecutors(t *testing.T) {
	w := &ir.Workflow{
		Attachments: map[string]*ir.Attachment{"logo": {Name: "logo", Type: ir.AttachmentImage}},
		Prompts: map[string]*ir.Prompt{
			"see":  {Name: "see", Body: "look {{attachments.logo}}", TemplateRefs: refsOf(t, "look {{attachments.logo}}")},
			"bang": {Name: "bang", Body: "look {{!attachments.logo.path}}", TemplateRefs: refsOf(t, "look {{!attachments.logo.path}}")},
		},
	}
	td := &model.TemplateData{Attachments: map[string]model.AttachmentInfo{"logo": {Name: "logo"}}}
	node := func(backend, user string, fallbacks ...ir.Fallback) *ir.AgentNode {
		n := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "ask"}}
		n.Backend = backend
		n.UserPrompt = user
		n.Fallbacks = fallbacks
		return n
	}
	refused := func(name string, n ir.Node, executor any) {
		t.Helper()
		var rt *RuntimeError
		if err := admitAttachmentPaths(n, w, td, executor); !errors.As(err, &rt) || rt.Code != ErrCodeAttachmentPathUnavailable {
			t.Fatalf("%s: err = %v, want %s", name, err, ErrCodeAttachmentPathUnavailable)
		}
	}
	admitted := func(name string, n ir.Node, executor any) {
		t.Helper()
		if err := admitAttachmentPaths(n, w, td, executor); err != nil {
			t.Fatalf("%s: refused: %v", name, err)
		}
	}
	refused("a \"!\" spelling renders as text on claw", node("claw", "bang"), nil)
	admitted("the plain spelling is an image block on claw", node("claw", "see"), nil)
	refused("a claw node the executor serves with a CLI backend", node("claw", "see"), routeStub{primary: "claude_code"})
	admitted("a CLI-declared node the executor serves with claw", node("claude_code", "see"), routeStub{primary: "claw"})
	admitted("a fallback spelled through vars the executor resolves to claw",
		node("claw", "see", ir.Fallback{Name: "spare", Backend: "${SPARE_BACKEND:-claw}"}), routeStub{primary: "claw"})
	refused("a fallback the executor resolves to a CLI backend",
		node("claw", "see", ir.Fallback{Name: "spare", Backend: "${SPARE_BACKEND:-claw}"}), routeStub{primary: "claw", fallbacks: []string{"claude_code"}})
	interaction := node("claw", "see")
	interaction.InteractionPrompt = "see"
	refused("a user prompt that is also the interaction prompt", interaction, nil)
}

// A setup failure an operator's cancel cut short reaches the runner as the
// cancel — the typed code it would otherwise carry (and be re-offered
// under) does not ride along.
func TestASetupFailureUnderACancelIsTheCancel(t *testing.T) {
	e := &Engine{}
	cause := attachmentsUnfetchable("run-x", "fetch attachment spec", context.Canceled)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := e.setupErr(ctx, cause)
	if !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("err = %v, want it to wrap ErrRunCancelled", err)
	}
	var rt *RuntimeError
	if errors.As(err, &rt) {
		t.Fatalf("the typed failure %s still rides the cancel: a runner keyed on it would re-offer a cancelled run", rt.Code)
	}
	if live := e.setupErr(context.Background(), cause); !errors.As(live, &rt) || rt.Code != ErrCodeAttachmentsUnfetchable {
		t.Fatalf("under a live context the typed failure must ride as is: %v", live)
	}
}

// The ghost path leaves nothing on disk: a run holding nothing but a
// record no name reaches gets no directory, and none is left behind.
func TestARunHoldingOnlyAGhostLeavesNoDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	fs := tmpStore(t)
	if _, err := fs.CreateRun(context.Background(), "run-only-ghost", "wf", nil); err != nil {
		t.Fatal(err)
	}
	e := engineOn(t, ghostListStore{fs})
	dir, err := e.materializeAttachments(context.Background(), "run-only-ghost")
	if err != nil || dir != "" {
		t.Fatalf("materializeAttachments = %q, %v; want no directory and no error", dir, err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "iterion-attachments-") {
			t.Fatalf("an empty materialisation directory was left behind: %s", entry.Name())
		}
	}
	var _ store.RunStore = ghostListStore{}
}
