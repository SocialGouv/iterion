package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// TestReviewPRWorkspaceAnomalyIsNotAPRFinding pins the two halves of #1666:
// a review workspace holding an uncommitted edit the pull request does not
// contain must not turn that edit into a PR finding.
//
// Seen 2026-09-22 on the docs-only PR #1563: two independent runs reviewed a
// working tree that carried an uncommitted edit of
// bots/whats-next/skills/iterion-bot-catalog.md — outside the PR's diff —
// and filed it as a medium finding against the PR. The workspace must equal
// the reviewed tree; when it does not, the drift is workspace state, never
// a defect of the change under review.
//
// The deterministic half lives in diff_precheck (name the tracked
// uncommitted entries) and publish_review (strip findings anchored on them,
// report one workspace anomaly naming the file and the difference). The
// reviewers' prompt scopes them out; these tests wire the guards that hold
// even when a reviewer files one anyway.
func TestReviewPRWorkspaceAnomalyIsNotAPRFinding(t *testing.T) {
	for _, bin := range []string{"python3", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}

	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return gittest.Run(t, ws, args...)
	}
	git("init", "--quiet", "-b", "main")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(ws+"/"+name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("catalog.md", "committed\n")
	git("add", "-A")
	git("commit", "-m", "base")
	head := git("rev-parse", "HEAD")

	substitute := func(t *testing.T, body string, refs map[string]string) string {
		t.Helper()
		for ref, val := range refs {
			if !strings.Contains(body, ref) {
				t.Fatalf("%s is no longer referenced by the command — the test wires nothing", ref)
			}
			// Shell-quote exactly as the engine does: a raw value dropped
			// into a command line is split on spaces and brace-expanded.
			body = strings.ReplaceAll(body, ref, "'"+strings.ReplaceAll(val, "'", `'\''`)+"'")
		}
		return body
	}

	type precheck struct {
		ChangedFiles     int    `json:"changed_files"`
		WorkspaceAnomaly string `json:"workspace_anomaly"`
	}
	// runPrecheck drives the real diff_precheck command; pathPrefix, when
	// given, prepends a shim directory to PATH (a git that fails one
	// subcommand on purpose).
	runPrecheck := func(t *testing.T, base string, pathPrefix ...string) precheck {
		t.Helper()
		body := substitute(t, toolCommand(t, "review-pr/main.bot", "diff_precheck"), map[string]string{
			"{{vars.workspace_dir}}": ws,
			"{{vars.base_ref}}":      base,
		})
		cmd := exec.Command("sh", "-c", body)
		if len(pathPrefix) > 0 {
			cmd.Env = append(os.Environ(), "PATH="+pathPrefix[0]+":"+os.Getenv("PATH"))
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("diff_precheck failed: %v (out %q)", err, out)
		}
		var got precheck
		if e := json.Unmarshal(out, &got); e != nil {
			t.Fatalf("output is not diff_precheck_output JSON: %v (%q)", e, out)
		}
		return got
	}

	t.Run("precheck: a tracked uncommitted edit is named", func(t *testing.T) {
		write("catalog.md", "uncommitted drift\n")
		got := runPrecheck(t, "HEAD")
		// base_ref: HEAD is the review-uncommitted mode: no anomaly there.
		if got.WorkspaceAnomaly != "" {
			t.Fatalf("base_ref HEAD: workspace_anomaly = %q, want empty — uncommitted work is the object in this mode", got.WorkspaceAnomaly)
		}
		got = runPrecheck(t, "main")
		if !strings.Contains(got.WorkspaceAnomaly, "catalog.md") {
			t.Fatalf("workspace_anomaly = %q, want it naming catalog.md — the drifted file must reach the anomaly channel", got.WorkspaceAnomaly)
		}
		git("checkout", "--quiet", "--", "catalog.md")
	})

	// git C-quotes any porcelain name carrying a space or a non-ASCII byte
	// (quotepath): "dir/file name.md", docs/rapport-\303\251t\303\251.md. A
	// finding's `file` is the PLAIN path, so an anomaly entry read in the
	// line form never matches it — the drifted file slips past the strip
	// and its findings post as PR findings. The precheck must read -z
	// (NUL-terminated, never quoted) and transport the plain names.
	t.Run("precheck: quoted names travel plain", func(t *testing.T) {
		for _, name := range []string{"dir/file name.md", "docs/rapport-été.md"} {
			if err := os.MkdirAll(ws+"/"+name[:strings.LastIndex(name, "/")], 0o755); err != nil {
				t.Fatal(err)
			}
			write(name, "committed\n")
		}
		git("add", "-A")
		git("commit", "-q", "-m", "files with hostile names")
		write("dir/file name.md", "uncommitted drift\n")
		write("docs/rapport-été.md", "uncommitted drift\n")

		got := runPrecheck(t, "main")
		for _, name := range []string{"dir/file name.md", "docs/rapport-été.md"} {
			found := false
			for _, l := range strings.Split(got.WorkspaceAnomaly, "\n") {
				if l == " M "+name {
					found = true
				}
			}
			if !found {
				t.Errorf("workspace_anomaly = %q, want one entry exactly %q — the line form's C-quoting would hide it from the strip", got.WorkspaceAnomaly, " M "+name)
			}
		}
		git("checkout", "--quiet", "--", "dir", "docs")
	})

	// In -z a rename is TWO NUL fields — the NEW path, then the OLD one. A
	// finding anchors to the new name, so the anomaly entry keeps the first
	// field and skips the second; carrying the old name would let it
	// reclassify a finding anchored on a path the worktree no longer holds.
	t.Run("precheck: a rename keeps its new path only", func(t *testing.T) {
		write("old.go", "package x\n")
		git("add", "-A")
		git("commit", "-q", "-m", "a file about to be renamed")
		git("mv", "old.go", "new.go")
		write("new.go", "package x // uncommitted drift\n")
		defer git("reset", "--quiet", "--hard", "HEAD")

		got := runPrecheck(t, "main")
		if !strings.Contains(got.WorkspaceAnomaly, "RM new.go") {
			t.Errorf("workspace_anomaly = %q, want the %q entry naming the new path", got.WorkspaceAnomaly, "RM new.go")
		}
		if strings.Contains(got.WorkspaceAnomaly, "old.go") {
			t.Errorf("workspace_anomaly = %q still carries the rename's source — the second -z field must be skipped", got.WorkspaceAnomaly)
		}
	})

	// A failed status read must NOT emit a clean tree: the publish step's
	// strip would be disarmed in silence and the review would read as if
	// the workspace had been verified. The "?" marker travels instead.
	t.Run("precheck: a failed status read emits the marker", func(t *testing.T) {
		realGit, err := exec.LookPath("git")
		if err != nil {
			t.Skipf("git not on PATH: %v", err)
		}
		shim := t.TempDir()
		// Forwards everything to the real git except `status`, which fails.
		script := "#!/bin/sh\nif [ \"$1\" = status ]; then exit 1; fi\nexec " + realGit + " \"$@\"\n"
		if err := os.WriteFile(shim+"/git", []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := runPrecheck(t, "main", shim); got.WorkspaceAnomaly != "?" {
			t.Errorf("workspace_anomaly = %q on a failed status read, want the %q marker", got.WorkspaceAnomaly, "?")
		}
	})

	t.Run("precheck: untracked scratch is not an anomaly", func(t *testing.T) {
		// The bot's own .review-pr/ scratch is untracked by design, and an
		// untracked file never enters the reviewers' diff scope — counting
		// it would raise the anomaly on every clean run.
		write("scratch.tmp", "scratch\n")
		if got := runPrecheck(t, "main"); got.WorkspaceAnomaly != "" {
			t.Fatalf("workspace_anomaly = %q over untracked scratch, want empty", got.WorkspaceAnomaly)
		}
		if err := os.Remove(ws + "/scratch.tmp"); err != nil {
			t.Fatal(err)
		}
	})

	type published struct {
		Summary  string           `json:"summary"`
		Comments []map[string]any `json:"comments"`
		Gate     map[string]any   `json:"gate"`
	}
	// runPublish drives the real publish_review command over one findings
	// set and one anomaly list, against a forge stub that captures the
	// payload. anomaly == nil leaves {{input.workspace_anomaly}} UNRESOLVED.
	runPublish := func(t *testing.T, anomaly *string, findings string, gate bool) published {
		t.Helper()
		var got published
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("publish payload is not JSON: %v (%q)", err, raw)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"published": true, "review_url": "https://forge/review/1",
				"comments_posted": len(got.Comments), "suggestions_posted": 0,
				"gate_posted": got.Gate != nil, "gate_state": "success",
			})
		}))
		defer srv.Close()

		refs := map[string]string{
			"{{vars.workspace_dir}}": ws,
			// The fixture's HEAD moves as subtests commit their own files;
			// the stale-anchor guard compares against the CURRENT one.
			"{{input.reviewed_sha}}":          git("rev-parse", "HEAD"),
			"{{vars.forge_publish_url}}":      srv.URL + "/api/v1/forge/publish-review",
			"{{vars.forge_publish_token}}":    "run-token",
			"{{vars.pr_review_mode}}":         "comment",
			"{{input.effective_review_mode}}": "mono",
			"{{input.pr_url}}":                "https://github.com/acme/widgets/pull/7",
			"{{input.findings}}":              findings,
			"{{input.questions}}":             "",
			"{{input.claude_findings}}":       "[]",
			"{{input.gpt_findings}}":          "[]",
		}
		if gate {
			refs["{{vars.gate_enabled}}"] = "true"
			refs["{{vars.gate_severity}}"] = "high"
			refs["{{vars.gate_context}}"] = "revi/review"
			refs["{{input.scope_files}}"] = "1"
		} else {
			refs["{{vars.gate_enabled}}"] = "false"
		}
		if anomaly != nil {
			refs["{{input.workspace_anomaly}}"] = *anomaly
		}
		out, err := exec.Command("sh", "-c", substitute(t, toolCommand(t, "review-pr/main.bot", "publish_review"), refs)).Output()
		if err != nil {
			t.Fatalf("publish_review failed: %v (out %q)", err, out)
		}
		if got.Summary == "" {
			t.Fatal("the forge stub received no payload — the publish never happened")
		}
		return got
	}
	strPtr := func(s string) *string { return &s }

	t.Run("publish: a finding on the drifted file routes to the anomaly", func(t *testing.T) {
		// The workspace carries the drift so publish's git-diff --stat can
		// name the difference.
		write("catalog.md", "uncommitted drift\n")
		defer git("checkout", "--quiet", "--", "catalog.md")

		onDrift := `{"severity":"medium","category":"correctness","title":"broken relative link","detail":"d","file":"catalog.md","line":1}`
		onPR := `{"severity":"low","category":"style","title":"nit","detail":"d","file":"committed.go","line":3}`
		got := runPublish(t, strPtr(" M catalog.md"), "["+onDrift+","+onPR+"]", false)

		// Zero findings ON THE PR from the drifted file: only the finding on
		// a file the PR contains may be posted as an inline comment.
		if len(got.Comments) != 1 || got.Comments[0]["path"] != "committed.go" {
			t.Fatalf("posted comments = %v, want exactly the finding on the PR's own file", got.Comments)
		}
		if !strings.Contains(got.Summary, "Anomalie workspace") || !strings.Contains(got.Summary, "catalog.md") {
			t.Errorf("the summary must report one workspace anomaly naming the file:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "catalog.md") || !strings.Contains(got.Summary, "Différence") {
			t.Errorf("the anomaly must name the difference, not only the file:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "broken relative link") {
			t.Errorf("the finding the drift drew must stay readable, reclassified as workspace state:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "1 problème à corriger") || strings.Contains(got.Summary, "2 problèmes") {
			t.Errorf("exactly the PR's own finding must count against it — the drift's one was reclassified:\n%s", got.Summary)
		}
	})

	// The strip matches the finding's `file` against the anomaly entry
	// WHOLE: a name git would C-quote in the line form (spaces, non-ASCII)
	// must still match, or the drift's finding posts as a PR finding and
	// the difference goes unnamed — the exact replay the adversarial round
	// ran: inline comment posted, gate red, no "Différence constatée".
	t.Run("publish: hostile names are stripped whole", func(t *testing.T) {
		if err := os.MkdirAll(ws+"/dir", 0o755); err != nil {
			t.Fatal(err)
		}
		write("dir/file name.md", "committed for the publish strip\n")
		git("add", "-A")
		git("commit", "-q", "-m", "a name with spaces")
		write("dir/file name.md", "uncommitted drift\n")
		defer git("checkout", "--quiet", "--", "dir")

		onDrift := `{"severity":"high","category":"correctness","title":"drift finding","detail":"d","file":"dir/file name.md","line":1}`
		got := runPublish(t, strPtr(" M dir/file name.md"), "["+onDrift+"]", false)
		if len(got.Comments) != 0 {
			t.Errorf("posted %d inline comment(s) for a drifted file with a quoted name — the strip missed it: %v", len(got.Comments), got.Comments)
		}
		if !strings.Contains(got.Summary, "dir/file name.md") {
			t.Errorf("the anomaly must name the drifted file plainly:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "Différence constatée") {
			t.Errorf("the difference must be named for a quoted name too:\n%s", got.Summary)
		}
	})

	// The transport is newline-joined, so a filename holding a literal
	// newline arrives as TWO lines: the first (" M weird") is a valid entry
	// and stays; the orphan fragment ("name.go") must NOT parse as an
	// entry — read naively, l[3:] would mangle it into the phantom path
	// "e.go", able to reclassify a LEGITIMATE finding anchored there.
	t.Run("publish: a newline-named file leaves no phantom path", func(t *testing.T) {
		onPhantom := `{"severity":"high","category":"correctness","title":"real bug in e.go","detail":"d","file":"e.go","line":1}`
		got := runPublish(t, strPtr(" M weird\nname.go"), "["+onPhantom+"]", false)
		if len(got.Comments) != 1 || got.Comments[0]["path"] != "e.go" {
			t.Errorf("the legitimate finding on e.go must survive — the orphan fragment must not reclassify it: %v", got.Comments)
		}
		// The first line remains a proper entry: the anomaly section shows it.
		if !strings.Contains(got.Summary, "weird") {
			t.Errorf("the first-line entry must still reach the anomaly section:\n%s", got.Summary)
		}
	})

	// The "?" marker means the precheck's status read FAILED: the strip has
	// nothing to work with, and a silent clean tree would disarm it without
	// a trace. Nothing is stripped, and the summary says the workspace
	// state could not be verified.
	t.Run("publish: the failed-check marker disarms nothing silently", func(t *testing.T) {
		onDrift := `{"severity":"high","category":"correctness","title":"drift finding","detail":"d","file":"catalog.md","line":1}`
		got := runPublish(t, strPtr("?"), "["+onDrift+"]", false)
		if len(got.Comments) != 1 {
			t.Errorf("the marker must strip NOTHING — findings publish as usual: %v", got.Comments)
		}
		if !strings.Contains(got.Summary, "État du workspace non vérifié") {
			t.Errorf("the summary must say the workspace state could not be verified:\n%s", got.Summary)
		}
		if strings.Contains(got.Summary, "Anomalie workspace") {
			t.Errorf("a failed check is not an anomaly section:\n%s", got.Summary)
		}
	})

	// The strip is by whole file: a file that is BOTH in the PR's diff and
	// drifted sees its legitimate, committed-hunk findings reclassified.
	// That must never read as approval by omission — the headline names the
	// reclassification instead of "aucun problème détecté", and the gate
	// note counts it.
	t.Run("publish: a reclassified finding stays visible to the gate", func(t *testing.T) {
		write("feature.go", "package x\n")
		git("add", "-A")
		git("commit", "-q", "-m", "the PR's own change")
		defer git("reset", "--quiet", "--hard", head)
		write("feature.go", "package x // uncommitted drift\n")

		onCommittedHunk := `{"severity":"high","category":"correctness","title":"nil deref on the committed line","detail":"d","file":"feature.go","line":1}`
		got := runPublish(t, strPtr(" M feature.go"), "["+onCommittedHunk+"]", true)

		if len(got.Comments) != 0 {
			t.Errorf("a finding on a drifted file must not post inline, got %v", got.Comments)
		}
		if strings.Contains(got.Summary, "aucun problème détecté dans le périmètre revu") {
			t.Errorf("the headline claims a clean review over a reclassified finding:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "1 finding reclassé") {
			t.Errorf("the headline must count the reclassification:\n%s", got.Summary)
		}
		note, _ := got.Gate["note"].(string)
		if got.Gate == nil || !strings.Contains(note, "1 finding") || !strings.Contains(note, "reclassified") {
			t.Errorf("the gate note must count the reclassified finding, gate = %v", got.Gate)
		}
	})

	t.Run("publish: an unsubstituted ref reads as a clean tree", func(t *testing.T) {
		// A run wired before this input existed reaches the shell with the
		// literal template text — it must not raise a phantom anomaly that
		// strips real findings.
		findings := `[{"severity":"low","category":"style","title":"nit","detail":"d","file":"committed.go","line":3}]`
		got := runPublish(t, nil, findings, false)
		if len(got.Comments) != 1 {
			t.Fatalf("posted comments = %v, want the one real finding kept", got.Comments)
		}
		if strings.Contains(got.Summary, "Anomalie workspace") {
			t.Errorf("an unresolved ref raised a phantom anomaly:\n%s", got.Summary)
		}
	})
}
