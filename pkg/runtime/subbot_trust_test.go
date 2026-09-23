package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A sub-bot child inherits its parent's queue message wholesale (the runner
// does `child := *msg`), so the WIRE carries the trust marker. Its run
// DOCUMENT does not: it is built from a named field list, and every
// enforcement site that resolves a run by id — the publish grant check, the
// merge-time forge token — reads that document. Without this the child
// executed the parent's untrusted tree while its own row read as trusted.
func TestRunResolveDocStampsTrustOntoTheChildDocument(t *testing.T) {
	newEngine := func(opts ...EngineOption) (*Engine, store.RunStore) {
		st := tmpStore(t)
		return New(&ir.Workflow{Name: "child_wf"}, st, nil, opts...), st
	}

	t.Run("the marker and its pin are stamped", func(t *testing.T) {
		e, _ := newEngine(
			WithParentRunID("parent-run-1"),
			WithTrust(store.RunTrustFork, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
		)
		run, err := e.runResolveDoc(context.Background(), "child-trust-1", map[string]any{})
		if err != nil {
			t.Fatalf("runResolveDoc: %v", err)
		}
		if run.Trust != store.RunTrustFork {
			t.Fatalf("child doc Trust = %q, want %q — the wire carried it and the document dropped it", run.Trust, store.RunTrustFork)
		}
		if run.RepoSHAExpected == "" {
			t.Fatal("child doc lost the admitted commit")
		}
	})

	// A trusted launch says nothing and must keep saying nothing: the option
	// ignores empty values, mirroring WithParentRunID, so no existing child
	// acquires a marker it never had.
	t.Run("a trusted child is unchanged", func(t *testing.T) {
		e, _ := newEngine(WithParentRunID("parent-run-2"), WithTrust("", ""))
		run, err := e.runResolveDoc(context.Background(), "child-trust-2", map[string]any{})
		if err != nil {
			t.Fatalf("runResolveDoc: %v", err)
		}
		if !run.Trust.Trusted() || run.RepoSHAExpected != "" {
			t.Fatalf("trust=%q pin=%q, want the trusted default and no pin", run.Trust, run.RepoSHAExpected)
		}
	})
}
