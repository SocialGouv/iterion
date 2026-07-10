package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// nodeFinished builds a node_finished event carrying the given output map.
func nodeFinished(seq int64, nodeID string, output map[string]any) *store.Event {
	return &store.Event{
		Seq:       seq,
		Timestamp: time.Unix(1_700_000_000+seq, 0).UTC(),
		Type:      store.EventNodeFinished,
		NodeID:    nodeID,
		Data:      map[string]any{"output": output},
	}
}

// testStore returns an empty file-backed store rooted at a temp dir (so
// buildReport's collectArtifacts finds no artifacts and returns cleanly).
func testStore(t *testing.T) store.RunStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

// TestReportSurfacesVerifyCommand asserts the verify authoring node's summary
// (its {prepared, summary} contract) is lifted onto a `verify:` header line.
func TestReportSurfacesVerifyCommand(t *testing.T) {
	r := &store.Run{ID: "run-abc", WorkflowName: "feature-dev", CreatedAt: time.Unix(1_700_000_000, 0).UTC()}
	events := []*store.Event{
		nodeFinished(1, "verify_build", map[string]any{
			"prepared": true,
			"summary":  "devbox run -- go build ./...\nexcluded pre-existing failure in pkg/legacy",
		}),
	}

	rpt := buildReport(r, events, testStore(t))
	if rpt.VerifyCommand != "devbox run -- go build ./..." {
		t.Fatalf("VerifyCommand = %q, want the first line of the summary", rpt.VerifyCommand)
	}

	md := renderMarkdown(rpt)
	// The verify line must sit near the top — before the Summary table.
	verifyLine := "verify: devbox run -- go build ./..."
	if !strings.Contains(md, verifyLine) {
		t.Fatalf("rendered report missing %q:\n%s", verifyLine, md)
	}
	if idx, sumIdx := strings.Index(md, verifyLine), strings.Index(md, "## Summary"); idx == -1 || idx > sumIdx {
		t.Fatalf("verify line should appear before the Summary section (verify=%d summary=%d)", idx, sumIdx)
	}
}

// TestReportNoVerifyCommand asserts a run without a verify authoring node
// renders no `verify:` line (the feature is additive, never noisy).
func TestReportNoVerifyCommand(t *testing.T) {
	r := &store.Run{ID: "run-xyz", WorkflowName: "plain", CreatedAt: time.Unix(1_700_000_000, 0).UTC()}
	events := []*store.Event{
		// A node_finished without the {prepared} contract must not trigger it.
		nodeFinished(1, "campaign", map[string]any{"summary": "shipped a slice"}),
	}

	rpt := buildReport(r, events, testStore(t))
	if rpt.VerifyCommand != "" {
		t.Fatalf("VerifyCommand = %q, want empty for a run with no verify node", rpt.VerifyCommand)
	}
	if md := renderMarkdown(rpt); strings.Contains(md, "\nverify:") {
		t.Fatalf("rendered report should have no verify line:\n%s", md)
	}
}
