package runview

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The owning ticket is read through the spec's run loader — Runs when the
// caller set it — and not from Store alone: the cloud runner's Store wraps
// the run store and loads no records, so reading Store answered "" on every
// cloud run, and every board transport there stamped no planner provenance
// while the same `.bot` run locally linked each card to its parent.
func TestSourceIssueIDOfReadsTheRunLoaderWhenStoreCannot(t *testing.T) {
	runs := cyclicRuns{"run-1": {ID: "run-1", Source: &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: "native:owning-card"}}}
	spec := ExecutorSpec{Store: eventsOnly{}, Runs: runs, RunID: "run-1"}
	if got := SourceIssueIDOf(spec); got != "native:owning-card" {
		t.Fatalf("SourceIssueIDOf = %q, want the run record's source issue read through ExecutorSpec.Runs", got)
	}
	// The explicit field still wins, with no record load at all.
	spec.SourceIssueID = "native:explicit"
	spec.Runs = nil
	if got := SourceIssueIDOf(spec); got != "native:explicit" {
		t.Fatalf("SourceIssueIDOf = %q, want the explicit SourceIssueID", got)
	}
}

// The catalog gate every local surface reads: a workflow whose only actions
// are native (board.*) builds no catalog resolver — its transport is
// registered by BuildExecutor with no catalog and no connection — even when
// the surface holds a sealer. The HTTP row is the falsifier.
func TestLocalConnectorsBuildsNoResolverForNativeOnlyActions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	// An installed (empty) home tier, so the HTTP row has a catalog to build.
	if err := os.MkdirAll(filepath.Join(home, "connectors"), 0o755); err != nil {
		t.Fatalf("mkdir home tier: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	nativeOnly := &ir.Workflow{Nodes: map[string]ir.Node{
		"plan": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "plan"}, Action: "board.issue.list"},
	}}
	r, client, err := LocalConnectors(nativeOnly, t.TempDir(), t.TempDir(), sealer, connection.TrustOfRun(store.RunTrustDefault))
	if err != nil || r != nil || client != nil {
		t.Fatalf("LocalConnectors(native-only) = (%T, %v, %v), want nothing built", r, client, err)
	}
	withHTTP := &ir.Workflow{Nodes: map[string]ir.Node{
		"plan":    nativeOnly.Nodes["plan"],
		"comment": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "comment"}, Action: "forgejo.issue.comment", Connection: "main"},
	}}
	r, _, err = LocalConnectors(withHTTP, t.TempDir(), t.TempDir(), sealer, connection.TrustOfRun(store.RunTrustDefault))
	if err != nil || r == nil {
		t.Fatalf("LocalConnectors(with an HTTP action) = (%T, %v), want the catalog resolver", r, err)
	}
}
