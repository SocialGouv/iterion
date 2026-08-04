package server

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=iterion", "GIT_AUTHOR_EMAIL=iterion@example.test",
		"GIT_COMMITTER_NAME=iterion", "GIT_COMMITTER_EMAIL=iterion@example.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeIn(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// snapshotAs mimics what the engine records at a boundary: stage
// everything, commit the tree onto HEAD without moving HEAD, point a ref
// at it.
func snapshotAs(t *testing.T, dir, ref string) string {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	tree := gitIn(t, dir, "write-tree")
	head := gitIn(t, dir, "rev-parse", "HEAD")
	commit := gitIn(t, dir, "commit-tree", tree, "-p", head, "-m", "snapshot "+ref)
	gitIn(t, dir, "update-ref", ref, commit)
	gitIn(t, dir, "reset", "--mixed", "HEAD")
	return commit
}

// TestReviewScope_GroupsByNodeAndKeepsUnattributedWork is the core
// contract of the gate-range design.
//
// The RANGE is a workspace before/after, so it contains everything the run
// did — including work by node kinds that record no boundary (subbots,
// fan-out branches, computes). Grouping by node is presentation on top;
// what cannot be attributed must still appear, or a reviewer approves less
// than what changed.
func TestReviewScope_GroupsByNodeAndKeepsUnattributedWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	wt := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitIn(t, wt, "init", "-q", "-b", "main")
	gitIn(t, wt, "config", "user.email", "iterion@example.test")
	gitIn(t, wt, "config", "user.name", "iterion")
	writeIn(t, wt, "README.md", "base\n")
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "base")
	base := gitIn(t, wt, "rev-parse", "HEAD")

	const runID = "run-review-scope"

	// --- gate 0: the run starts here.
	snapshotAs(t, wt, store.ReviewGateRef(runID, 0))

	// `implement` runs: bracketed by both boundaries.
	snapshotAs(t, wt, store.NodePreSnapshotRef(runID, "implement", 0))
	writeIn(t, wt, "src/feature.go", "package main // by implement\n")
	snapshotAs(t, wt, store.NodeSnapshotRef(runID, "implement", 0))

	// A subbot runs: NO boundary refs at all — the case the per-node
	// design could not cover.
	writeIn(t, wt, "docs/from_subbot.md", "written by a delegated child\n")

	// `write_docs` runs: bracketed.
	snapshotAs(t, wt, store.NodePreSnapshotRef(runID, "write_docs", 0))
	writeIn(t, wt, "docs/guide.md", "by write_docs\n")
	snapshotAs(t, wt, store.NodeSnapshotRef(runID, "write_docs", 0))

	// --- gate 1: the reviewer is paused here.
	snapshotAs(t, wt, store.ReviewGateRef(runID, 1))

	run := &store.Run{ID: runID, WorkDir: wt, BaseCommit: base, Worktree: true}
	scope := buildReviewScope(run, -1, nil)

	if !scope.Available {
		t.Fatalf("scope unavailable: %s", scope.Reason)
	}
	if scope.GateSeq != 1 {
		t.Errorf("GateSeq = %d, want the latest gate (1)", scope.GateSeq)
	}
	// Completeness first: every file changed since gate 0 is in the range.
	if scope.TotalFiles != 3 {
		t.Fatalf("TotalFiles = %d, want 3 (feature.go, from_subbot.md, guide.md)", scope.TotalFiles)
	}

	seen := map[string]string{} // path -> group label
	for _, g := range scope.Groups {
		for _, f := range g.Files {
			seen[f.Path] = g.Label
		}
	}
	if got := seen["src/feature.go"]; got != "implement" {
		t.Errorf("src/feature.go grouped under %q, want implement", got)
	}
	if got := seen["docs/guide.md"]; got != "write_docs" {
		t.Errorf("docs/guide.md grouped under %q, want write_docs", got)
	}
	// The subbot's file has no boundary to attribute it to — it must still
	// be shown, under the catch-all.
	got := seen["docs/from_subbot.md"]
	if got == "" {
		t.Fatal("the subbot's file vanished from the review — a reviewer would approve work they never saw")
	}
	if !strings.Contains(got, "Other changes") {
		t.Errorf("docs/from_subbot.md grouped under %q, want the catch-all group", got)
	}
	// The groups partition the range exactly.
	var total int
	for _, g := range scope.Groups {
		total += len(g.Files)
	}
	if total != scope.TotalFiles {
		t.Errorf("groups hold %d files, range has %d — grouping must partition, not filter", total, scope.TotalFiles)
	}
}

// TestReviewScope_RangeStartsAtPreviousGate: a second reviewer sees only
// what happened since the first approved, not the whole run.
func TestReviewScope_RangeStartsAtPreviousGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	wt := t.TempDir()
	gitIn(t, wt, "init", "-q", "-b", "main")
	gitIn(t, wt, "config", "user.email", "iterion@example.test")
	gitIn(t, wt, "config", "user.name", "iterion")
	writeIn(t, wt, "README.md", "base\n")
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "base")
	base := gitIn(t, wt, "rev-parse", "HEAD")

	const runID = "run-two-gates"
	snapshotAs(t, wt, store.ReviewGateRef(runID, 0))
	writeIn(t, wt, "phase_one.md", "approved by the first reviewer\n")
	snapshotAs(t, wt, store.ReviewGateRef(runID, 1))
	writeIn(t, wt, "phase_two.md", "the second reviewer's business\n")
	snapshotAs(t, wt, store.ReviewGateRef(runID, 2))

	run := &store.Run{ID: runID, WorkDir: wt, BaseCommit: base, Worktree: true}
	scope := buildReviewScope(run, -1, nil)
	if !scope.Available {
		t.Fatalf("unavailable: %s", scope.Reason)
	}
	if scope.TotalFiles != 1 {
		t.Fatalf("TotalFiles = %d, want 1 — the second gate must not re-show what the first approved", scope.TotalFiles)
	}
	if scope.Groups[0].Files[0].Path != "phase_two.md" {
		t.Errorf("range shows %q, want phase_two.md", scope.Groups[0].Files[0].Path)
	}

	// And an explicit gate selects its own range.
	first := buildReviewScope(run, 1, nil)
	if !first.Available || first.TotalFiles != 1 || first.Groups[0].Files[0].Path != "phase_one.md" {
		t.Errorf("gate 1 range = %+v, want just phase_one.md", first.Groups)
	}
}

// TestReviewScope_ReportsWhyItIsEmpty: a review panel that shows nothing
// without saying why is worse than no panel.
func TestReviewScope_ReportsWhyItIsEmpty(t *testing.T) {
	scope := buildReviewScope(&store.Run{ID: "no-workspace"}, -1, nil)
	if scope.Available {
		t.Fatal("expected unavailable for a run with no workspace")
	}
	if !strings.Contains(scope.Reason, "no workspace") {
		t.Errorf("Reason = %q, want it to name the missing workspace", scope.Reason)
	}

	wt := t.TempDir()
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIn(t, wt, "a.txt", "x")

	// In-place run with no tracker: cannot build a range.
	scope = buildReviewScope(&store.Run{ID: "in-place", WorkDir: wt, Status: store.RunStatusPausedWaitingHuman}, -1, nil)
	if scope.Available {
		t.Fatal("expected unavailable when workspace versioning is disabled")
	}
	if !strings.Contains(scope.Reason, "versioning is disabled") {
		t.Errorf("Reason = %q, want it to name disabled versioning", scope.Reason)
	}

	// In-place run with a tracker but no captures yet.
	storeDir := t.TempDir()
	tr := workspacetrack.NewNative(storeDir)
	scope = buildReviewScope(&store.Run{ID: "no-snaps", WorkDir: wt, Status: store.RunStatusPausedWaitingHuman}, -1, tr)
	if scope.Available {
		t.Fatal("expected unavailable when no snapshot was captured")
	}
	if !strings.Contains(scope.Reason, "no workspace snapshots") && !strings.Contains(scope.Reason, "no review gate") {
		t.Errorf("Reason = %q, want it to say no snapshots/gates", scope.Reason)
	}

	// Worktree run with no gate refs.
	gitIn(t, wt, "init", "-q", "-b", "main")
	gitIn(t, wt, "config", "user.email", "iterion@example.test")
	gitIn(t, wt, "config", "user.name", "iterion")
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "base")
	scope = buildReviewScope(&store.Run{ID: "no-gates", WorkDir: wt, Worktree: true}, -1, nil)
	if scope.Available {
		t.Fatal("expected unavailable when no gate was reached")
	}
	if !strings.Contains(scope.Reason, "no review gate") {
		t.Errorf("Reason = %q, want it to say no gate was reached", scope.Reason)
	}
}

// TestReviewScope_InPlaceUsesWorkspaceTracker is the contract that makes
// the review panel work for the default run mode (worktree: none):
// everything changed since the previous gate, including files that
// .gitignore would hide, via workspacetrack + .iterionignore.
func TestReviewScope_InPlaceUsesWorkspaceTracker(t *testing.T) {
	ws := t.TempDir()
	storeDir := t.TempDir()
	tr := workspacetrack.NewNative(storeDir)
	const runID = "run-inplace-review"

	// Project packaging ignores the media tree — git would never show it.
	// iterion's allowlist versions the delivered files anyway.
	writeIn(t, ws, ".gitignore", "runs/\n")
	writeIn(t, ws, ".iterionignore", "runs/**\n!runs/**/audio/*.mp3\n!runs/**/audio/*.txt\n!runs/**/music/*.wav\n")
	writeIn(t, ws, "README.md", "base\n")

	// First capture = run start. Alias it as prepare_media's pre-boundary
	// (nothing touches the tree between nodes).
	start, err := tr.Capture(runID, ws, workspacetrack.Label(workspacetrack.PhasePre, "init", 0))
	if err != nil {
		t.Fatalf("pre capture: %v", err)
	}
	if err := tr.Alias(runID, workspacetrack.Label(workspacetrack.PhasePre, "prepare_media", 0), start.ID); err != nil {
		t.Fatalf("pre alias: %v", err)
	}

	// Pipeline produces the deliverables a human gate must approve.
	writeIn(t, ws, "runs/ep1/audio/narration.mp3", "fake-mp3-bytes")
	writeIn(t, ws, "runs/ep1/audio/narration_input.txt", "the spoken script\n")
	writeIn(t, ws, "runs/ep1/music/rhythm_guide.wav", "fake-wav-bytes")
	// A regenerable intermediate must NOT appear (covered by runs/**).
	writeIn(t, ws, "runs/ep1/logs/agent.json", `{"noise":true}`)
	// Source the bot did not write stays out of the range.
	writeIn(t, ws, "README.md", "base\nedited by the operator meanwhile\n")

	if _, err := tr.Capture(runID, ws, workspacetrack.Label(workspacetrack.PhasePost, "prepare_media", 0)); err != nil {
		t.Fatalf("post capture: %v", err)
	}
	// Human gate anchors with a real capture (gate:0).
	if _, err := tr.Capture(runID, ws, workspacetrack.GateLabel(0)); err != nil {
		t.Fatalf("gate capture: %v", err)
	}

	run := &store.Run{
		ID:      runID,
		WorkDir: ws,
		Status:  store.RunStatusPausedWaitingHuman,
	}
	scope := buildReviewScope(run, -1, tr)
	if !scope.Available {
		t.Fatalf("scope unavailable: %s", scope.Reason)
	}
	if scope.Backend != "workspace" {
		t.Errorf("Backend = %q, want workspace", scope.Backend)
	}
	if scope.GateSeq != 0 {
		t.Errorf("GateSeq = %d, want 0", scope.GateSeq)
	}

	seen := map[string]bool{}
	for _, g := range scope.Groups {
		for _, f := range g.Files {
			seen[f.Path] = true
		}
	}
	for _, want := range []string{
		"runs/ep1/audio/narration.mp3",
		"runs/ep1/audio/narration_input.txt",
		"runs/ep1/music/rhythm_guide.wav",
	} {
		if !seen[want] {
			t.Errorf("missing %s from the review range — a reviewer would approve work they never saw", want)
		}
	}
	if seen["runs/ep1/logs/agent.json"] {
		t.Error("logs/agent.json must stay out: it is excluded by .iterionignore")
	}
	// Operator-side README edit: the tracker versions it (not ignored), so
	// it appears. That is correct for an in-place run — the gate shows the
	// workspace before/after, not a declared product list.
	if scope.TotalFiles < 3 {
		t.Fatalf("TotalFiles = %d, want at least the three deliverables", scope.TotalFiles)
	}

	// Attribution: prepare_media should own the deliverables.
	owner := map[string]string{}
	for _, g := range scope.Groups {
		for _, f := range g.Files {
			owner[f.Path] = g.Label
		}
	}
	if got := owner["runs/ep1/audio/narration.mp3"]; got != "prepare_media" {
		t.Errorf("narration.mp3 grouped under %q, want prepare_media", got)
	}
}

// TestReviewScope_InPlaceFallbackWithoutGateLabel covers runs that paused
// before markReviewGate wrote gate:N labels: the panel still shows
// everything since the first capture when the run is paused_waiting_human.
func TestReviewScope_InPlaceFallbackWithoutGateLabel(t *testing.T) {
	ws := t.TempDir()
	storeDir := t.TempDir()
	tr := workspacetrack.NewNative(storeDir)
	const runID = "run-fallback"

	writeIn(t, ws, "a.txt", "start\n")
	if _, err := tr.Capture(runID, ws, "pre:init:0"); err != nil {
		t.Fatal(err)
	}
	writeIn(t, ws, "b.txt", "produced\n")
	if _, err := tr.Capture(runID, ws, "post:work:0"); err != nil {
		t.Fatal(err)
	}

	// Not paused → no synthetic range (avoids inventing a gate for a
	// still-running run).
	running := buildReviewScope(&store.Run{ID: runID, WorkDir: ws, Status: store.RunStatusRunning}, -1, tr)
	if running.Available {
		t.Fatal("must not synthesise a range for a non-paused run")
	}

	paused := buildReviewScope(&store.Run{ID: runID, WorkDir: ws, Status: store.RunStatusPausedWaitingHuman}, -1, tr)
	if !paused.Available {
		t.Fatalf("unavailable: %s", paused.Reason)
	}
	if paused.TotalFiles != 1 {
		t.Fatalf("TotalFiles = %d, want 1 (b.txt)", paused.TotalFiles)
	}
	if paused.Groups[0].Files[0].Path != "b.txt" {
		t.Errorf("got %q, want b.txt", paused.Groups[0].Files[0].Path)
	}
}

// TestSafeJoinUnder_RejectsSymlinkEscape is Revi's R2eb800.
//
// The containment check was lexical only (Abs + string prefix), so a
// symlink whose own path sits inside the workspace but whose target does
// not passed it, and os.Open then followed it — GET
// /api/runs/{id}/workspace-files/<link> streamed back whatever the link
// pointed at. Agents write freely into a run workspace and the workspace
// may be a checkout of an untrusted repo, so planting that link is inside
// the threat model this surface already defends against.
func TestSafeJoinUnder_RejectsSymlinkEscape(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "credentials")
	if err := os.WriteFile(secret, []byte("aws_secret_access_key=hunter2"), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(ws, "innocent.mp3")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A symlink to a directory, so the escape can also be reached through
	// a parent component rather than the leaf.
	if err := os.Symlink(outside, filepath.Join(ws, "media")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, rel := range []string{"innocent.mp3", "media/credentials"} {
		got, err := safeJoinUnder(ws, rel)
		if err == nil {
			t.Errorf("safeJoinUnder(%q) = %q, want an error — the path resolves outside the workspace "+
				"and the handler streams whatever it opens", rel, got)
		}
	}

	// A genuine file inside the workspace still resolves.
	writeIn(t, ws, "real.txt", "fine")
	if _, err := safeJoinUnder(ws, "real.txt"); err != nil {
		t.Errorf("safeJoinUnder rejected a legitimate workspace file: %v", err)
	}
}

// TestWorkspaceFileHeaders_ForcesDownloadForScriptableTypes is Revi's
// R03b907: the endpoint accepts ANY path under the run workspace, so
// serving .html/.svg inline let a file an agent wrote execute on the
// studio's own origin, against every unauthenticated local /api endpoint.
func TestWorkspaceFileHeaders_ForcesDownloadForScriptableTypes(t *testing.T) {
	cases := []struct {
		path       string
		wantInline bool
	}{
		{"report.html", false},
		{"diagram.svg", false},
		{"notes.md", false},
		{"data.yaml", false},
		{"doc.pdf", false},
		{"episode.mp4", true},
		{"theme.mp3", true},
		{"cover.png", true},
		{"transcript.txt", true},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/runs/r/workspace-files/"+tc.path, nil)
		setWorkspaceFileHeaders(rec, req, tc.path)

		disp := rec.Header().Get("Content-Disposition")
		inline := strings.HasPrefix(disp, "inline")
		if inline != tc.wantInline {
			t.Errorf("%s: disposition = %q, want inline=%v", tc.path, disp, tc.wantInline)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", tc.path, got)
		}
	}

	// ?download=1 still forces attachment for an otherwise-inline type.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/runs/r/workspace-files/a.mp4?download=1", nil)
	setWorkspaceFileHeaders(rec, req, "a.mp4")
	if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Error("?download=1 must still force attachment")
	}
}
