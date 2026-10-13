package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	// freeText holds the three reviewer-authored channels publish prints as
	// free text beside the findings array, none carrying a file anchor: the
	// questions and ticket verdicts (newline-joined scalars) and the audited
	// perimeter (' ; '-joined scanned_areas). The zero value is the empty set
	// the findings-only subtests publish with; an empty scope leaves
	// {{input.review_scope}} UNRESOLVED, as a run wired before it existed.
	type freeText struct{ questions, tickets, scope string }
	// runPublish drives the real publish_review command over one findings
	// set and one anomaly list, against a forge stub that captures the
	// payload. anomaly == nil leaves {{input.workspace_anomaly}} UNRESOLVED;
	// free, when given, fills the questions / ticket_conformance channels.
	runPublish := func(t *testing.T, anomaly *string, findings string, gate bool, free ...freeText) published {
		t.Helper()
		var scalars freeText
		if len(free) > 0 {
			scalars = free[0]
		}
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
			"{{input.questions}}":             scalars.questions,
			"{{input.ticket_conformance}}":    scalars.tickets,
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
		if scalars.scope != "" {
			refs["{{input.review_scope}}"] = scalars.scope
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
		// The candidates come from the PARSED entries, which a marker run has
		// none of: a matcher rebuilt from the RAW channel would take the
		// marker itself, reclassify the maintainer's list on exactly the runs
		// where nothing was verified and, with no entry to print it under,
		// drop it. The sentence closes on a bracket to keep its "?" matchable
		// by the boundaries — one preceded by a word character is rejected —
		// so this case drives that path. A second rule closes it as well: a
		// caseless one-segment candidate like "?" is matched only rooted, so
		// the rebuild alone leaves this case green and reddens it only with
		// that rule lifted too. This is the drop's outer guard, not a witness
		// of either rule.
		const asked = "Should the retry budget cover the new route (the one added here)?"
		got := runPublish(t, strPtr("?"), "["+onDrift+"]", false, freeText{questions: asked})
		if len(got.Comments) != 1 {
			t.Errorf("the marker must strip NOTHING — findings publish as usual: %v", got.Comments)
		}
		if !strings.Contains(got.Summary, "À confirmer") || !strings.Contains(got.Summary, asked) {
			t.Errorf("the marker must reclassify NOTHING — the question stays the maintainer's:\n%s", got.Summary)
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

	// The findings strip is one half of the channel (#1674). #1671's verdict
	// asked the author to answer for two uncommitted catalog regenerations
	// as a QUESTION — free text, no file anchor — and the question printed
	// verbatim under the maintainer's list. A question naming a drifted
	// entry is workspace state by the same rule as a finding anchored on
	// one: it belongs under the anomaly section, where it stays readable,
	// never in the list the author must answer. Same for a ticket verdict
	// that rests on one.
	const drifted = "bots/whats-next/skills/iterion-bot-catalog.md"
	if err := os.MkdirAll(ws+"/bots/whats-next/skills", 0o755); err != nil {
		t.Fatal(err)
	}
	write(drifted, "committed catalog\n")
	git("add", "-A")
	git("commit", "-q", "-m", "the catalog no pull request touches")
	drift := func(t *testing.T) {
		t.Helper()
		write(drifted, "regenerated by a stale binary\n")
		t.Cleanup(func() { git("checkout", "--quiet", "--", drifted) })
	}

	// The reviewer names the file the way prose does — the basename, not the
	// whole porcelain path — which is exactly how #1671's question read. The
	// second line puts the name at the END of a sentence: the match tolerates
	// the full stop that closes it, or the routing misses every question
	// written that way.
	t.Run("publish: a question naming the drifted file routes to the anomaly", func(t *testing.T) {
		drift(t)
		const question = "Are the two uncommitted iterion-bot-catalog.md working-tree changes intended to be part of this PR, or leftover regeneration output?"
		const trailing = "Please confirm the regeneration of iterion-bot-catalog.md."
		got := runPublish(t, strPtr(" M "+drifted), "[]", false, freeText{questions: question + "\n" + trailing})

		if strings.Contains(got.Summary, "À confirmer") {
			t.Errorf("the drifted file's question reached the maintainer's list — #1671's verdict verbatim:\n%s", got.Summary)
		}
		anomalyAt := strings.Index(got.Summary, "Anomalie workspace")
		if anomalyAt < 0 {
			t.Fatalf("the drifted entry raised no anomaly section:\n%s", got.Summary)
		}
		for _, q := range []string{question, trailing} {
			at := strings.Index(got.Summary, q)
			if at < 0 {
				t.Fatalf("question %q was dropped instead of reclassified:\n%s", q, got.Summary)
			}
			if at < anomalyAt {
				t.Errorf("question %q must stay readable UNDER the workspace anomaly section (anomaly at %d, question at %d):\n%s", q, anomalyAt, at, got.Summary)
			}
		}
		if !strings.Contains(got.Summary, "Question reclassée") {
			t.Errorf("the reclassification must be named, so the reader knows why the question is there:\n%s", got.Summary)
		}
	})

	t.Run("publish: a ticket verdict resting on the drifted file routes to the anomaly", func(t *testing.T) {
		drift(t)
		const verdict = "#1674: not covered — iterion-bot-catalog.md still drops sec-audit-source's launch vars"
		got := runPublish(t, strPtr(" M "+drifted), "[]", false, freeText{tickets: verdict})

		if strings.Contains(got.Summary, "Écarts au ticket") {
			t.Errorf("a ticket verdict resting on workspace drift was charged to the pull request:\n%s", got.Summary)
		}
		anomalyAt := strings.Index(got.Summary, "Anomalie workspace")
		verdictAt := strings.Index(got.Summary, verdict)
		if verdictAt < 0 {
			t.Fatalf("the verdict was dropped instead of reclassified:\n%s", got.Summary)
		}
		if anomalyAt < 0 || verdictAt < anomalyAt {
			t.Errorf("the verdict must stay readable UNDER the workspace anomaly section (anomaly at %d, verdict at %d):\n%s", anomalyAt, verdictAt, got.Summary)
		}
		if !strings.Contains(got.Summary, "Écart au ticket reclassé ici (nomme un fichier non commité listé ci-dessus) — "+verdict) {
			t.Errorf("the reclassified verdict must carry the reason the router actually has, not a claim about the PR's diff:\n%s", got.Summary)
		}
	})

	// The routing is by NAME, so it must name the drifted file and nothing
	// else: a question about a file the workspace did not drift, one about a
	// SIBLING sharing the drifted file's basename, three about LONGER names
	// that merely start or end with it (a prefix, a dotted compound, a
	// trailing extension or dash), and one about a path that merely ENDS
	// with the drifted path whole are ordinary maintainer questions.
	// Reclassifying them would bury a real decision in a section about
	// workspace state — and the last form is reachable 548 times over in this
	// tree, where that many tracked paths end with another tracked path.
	t.Run("publish: a question about another file stays a question", func(t *testing.T) {
		drift(t)
		const onPR = "Should pkg/server/proxy.go keep the 30s timeout on the new route?"
		const onSibling = "Does bots/issue-triage/skills/iterion-bot-catalog.md need the same regeneration in this PR?"
		const onLongerName = "Is iterion-bot-catalog.mdx the template this PR should render from?"
		const onPrefixedName = "Is my-iterion-bot-catalog.md generated from the same template?"
		const onSuffixedName = "Should iterion-bot-catalog.md-old be deleted in this PR?"
		const onDottedName = "Is draft.iterion-bot-catalog.md the pre-regeneration copy?"
		const onPathSuffix = "Is vendor/" + drifted + " regenerated by the same script?"
		// Second drifted entry, at the repo ROOT: its whole path IS a
		// basename, and a nested file sharing it (203 README.md in this
		// tree) must not be swept in by the rooted forms either.
		const onNestedNamesake = "Should docs/catalog.md document the new input?"
		got := runPublish(t, strPtr(" M "+drifted+"\n M catalog.md"), "[]", false, freeText{questions: strings.Join([]string{onPR, onSibling, onLongerName, onPrefixedName, onSuffixedName, onDottedName, onPathSuffix, onNestedNamesake}, "\n")})

		anomalyAt := strings.Index(got.Summary, "Anomalie workspace")
		if anomalyAt < 0 {
			t.Fatalf("the drifted entry must still raise its anomaly section:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "À confirmer") {
			t.Fatalf("both questions were reclassified — the maintainer's list is gone:\n%s", got.Summary)
		}
		for _, q := range []string{onPR, onSibling, onLongerName, onPrefixedName, onSuffixedName, onDottedName, onPathSuffix, onNestedNamesake} {
			at := strings.Index(got.Summary, q)
			if at < 0 || at > anomalyAt {
				t.Errorf("question %q must stay under the maintainer's list (at %d, anomaly at %d):\n%s", q, at, anomalyAt, got.Summary)
			}
		}
	})

	// An entry whose basename carries no extension (docs/changes) is a name
	// as a WHOLE PATH only: its bare last segment is an ordinary English
	// word, and taking it for a name would reclassify every question that
	// happens to use it.
	t.Run("publish: an extension-less entry is a name only as a whole path", func(t *testing.T) {
		if err := os.MkdirAll(ws+"/docs", 0o755); err != nil {
			t.Fatal(err)
		}
		write("docs/changes", "committed\n")
		git("add", "-A")
		git("commit", "-q", "-m", "a tracked file with no extension")
		write("docs/changes", "uncommitted drift\n")
		t.Cleanup(func() { git("checkout", "--quiet", "--", "docs/changes") })

		const prose = "Are the retry changes intended for this release?"
		const named = "Why does docs/changes carry an uncommitted edit?"
		// A path UNDER a drifted name is another file: the PR may well turn
		// docs/changes into a directory, and the right boundary's slash is
		// the only thing that tells the two apart.
		const under = "Why is docs/changes/legacy.md still tracked after this PR?"
		got := runPublish(t, strPtr(" M docs/changes"), "[]", false, freeText{questions: prose + "\n" + named + "\n" + under})

		anomalyAt := strings.Index(got.Summary, "Anomalie workspace")
		proseAt := strings.Index(got.Summary, prose)
		namedAt := strings.Index(got.Summary, named)
		underAt := strings.Index(got.Summary, under)
		if anomalyAt < 0 || proseAt < 0 || namedAt < 0 || underAt < 0 {
			t.Fatalf("anomaly at %d, prose at %d, named at %d, under at %d — no question may be dropped:\n%s", anomalyAt, proseAt, namedAt, underAt, got.Summary)
		}
		if proseAt > anomalyAt {
			t.Errorf("a question merely using the word changes was reclassified as workspace state:\n%s", got.Summary)
		}
		if underAt > anomalyAt {
			t.Errorf("a question about a path UNDER the drifted name was reclassified as workspace state:\n%s", got.Summary)
		}
		if namedAt < anomalyAt {
			t.Errorf("the question naming the drifted path whole must be reclassified:\n%s", got.Summary)
		}
	})

	// The reviewer renders the path the way it reached it, and the prompt
	// hands it the workspace as an ABSOLUTE path with an order to run
	// `git -C <that path>`: a question naming the file it just read is as
	// likely to be rooted as relative. The left boundary excludes a preceding
	// slash on purpose (the subtest above pins what that buys), so each
	// rooted form is matched explicitly — or #1671's verdict publishes
	// unchanged under the maintainer's list.
	t.Run("publish: a rooted rendering of the drifted path routes to the anomaly", func(t *testing.T) {
		for _, c := range []struct{ name, form string }{
			{"dot-slash", "./" + drifted},
			{"repo-rooted", "/" + drifted},
			{"workspace-absolute", ws + "/" + drifted},
			{"backticked absolute", "`" + ws + "/" + drifted + "`"},
		} {
			t.Run(c.name, func(t *testing.T) {
				drift(t)
				question := "Are the two uncommitted " + c.form + " working-tree changes intended to be part of this PR, or leftover regeneration output?"
				got := runPublish(t, strPtr(" M "+drifted), "[]", false, freeText{questions: question})

				if strings.Contains(got.Summary, "À confirmer") {
					t.Errorf("the %s rendering reached the maintainer's list — #1671's verdict verbatim:\n%s", c.name, got.Summary)
				}
				anomalyAt := strings.Index(got.Summary, "Anomalie workspace")
				at := strings.Index(got.Summary, question)
				if at < 0 {
					t.Fatalf("the question was dropped instead of reclassified:\n%s", got.Summary)
				}
				if anomalyAt < 0 || at < anomalyAt {
					t.Errorf("the question must stay readable UNDER the anomaly section (anomaly at %d, question at %d):\n%s", anomalyAt, at, got.Summary)
				}
			})
		}
	})

	// Routing a verdict the printing would NOT have charged to the pull
	// request invents a defect: a covered line is printed nowhere and an
	// unverifiable one keeps its own list in the details fold, so routing
	// either publishes "Écart au ticket" — a declared ticket gap — over a
	// verdict that claims no gap. The trigger is the ordinary run where the
	// catalogue a ticket is verified against is the file that drifted.
	t.Run("publish: a ticket verdict claiming no gap is not reclassified as one", func(t *testing.T) {
		drift(t)
		const covered = "#1674: covered — iterion-bot-catalog.md now lists sec-audit-source"
		const unverifiable = "#1674: unverifiable — the tracker returned 503; iterion-bot-catalog.md unread"
		// The ref travels in whatever rendering the reviewer collected it in,
		// and rule 3 collects it from the PR body, where a Closes line is an
		// issue URL. Read only up to the FIRST colon, a URL ref carries no
		// readable verdict at all: the covered line below publishes as a
		// declared ticket gap — and, once the catalogue drifts, as a gap
		// reclassified into the workspace anomaly.
		const coveredURL = "https://github.com/acme/widgets/issues/1674: covered — iterion-bot-catalog.md lists every launch var"
		const unverifiableURL = "https://github.com/acme/widgets/issues/1674: unverifiable — the forge returned 503; iterion-bot-catalog.md unread"
		got := runPublish(t, strPtr(" M "+drifted), "[]", false, freeText{tickets: strings.Join([]string{covered, unverifiable, coveredURL, unverifiableURL}, "\n")})

		if strings.Contains(got.Summary, "Écart au ticket") {
			t.Errorf("a verdict claiming no gap was published as a ticket gap:\n%s", got.Summary)
		}
		// A covered verdict prints nowhere, with or without the routing:
		// ticket_gaps excludes it and so does ticket_unverified.
		for _, v := range []string{covered, coveredURL} {
			if strings.Contains(got.Summary, v) {
				t.Errorf("the covered verdict %q must keep publishing nowhere, not move into the anomaly:\n%s", v, got.Summary)
			}
		}
		listAt := strings.Index(got.Summary, "Vérifications de ticket indisponibles")
		for _, v := range []string{unverifiable, unverifiableURL} {
			at := strings.Index(got.Summary, v)
			if listAt < 0 || at < 0 || at < listAt {
				t.Errorf("the unverifiable verdict %q must stay in its own list (list at %d, verdict at %d):\n%s", v, listAt, at, got.Summary)
			}
		}
	})

	// The URL alternative is confined to the line's FIRST whitespace-free run,
	// and its scheme is anchored on a run carrying no colon of its own. A ref
	// read with a lazy prefix instead would let any later colon supply the
	// verdict, so a real gap whose reason happens to mention `covered` would
	// print nowhere at all — the one failure direction that is silent, and
	// strictly worse than the gap this admission fixes. The second line is the
	// same trap read from the other end: a LABELLED ref, whose first field is
	// the label, carries no readable verdict — so it publishes as the gap it
	// claims not to be, visibly, instead of being swallowed as covered.
	t.Run("publish: a URL-ref gap is still charged as a gap", func(t *testing.T) {
		for _, gap := range []string{
			"https://github.com/acme/widgets/issues/1674: not covered — the launch vars are absent; the ticket's own wording mentions: covered",
			"ref:https://github.com/acme/widgets/issues/1674: covered — the catalogue lists every launch var",
		} {
			got := runPublish(t, strPtr(""), "[]", false, freeText{tickets: gap})

			gapsAt := strings.Index(got.Summary, "**Écarts au ticket**")
			at := strings.Index(got.Summary, gap)
			if gapsAt < 0 || at < 0 || at < gapsAt {
				t.Errorf("%q must publish under the ticket-gaps list (list at %d, verdict at %d):\n%s", gap, gapsAt, at, got.Summary)
			}
		}
	})

	// What the match establishes is that the line NAMES a drifted entry —
	// nothing more. 5056 of this tree's 13993 tracked files share their
	// basename (203 README.md, 49 manifest.yaml), so a question about the
	// pull request's OWN file lands here whenever a namesake drifted. It is
	// reclassified — the name is all a free-text channel carries — under a
	// label that says exactly that, and never under a claim that the question
	// is outside the PR's diff, which would invite the author to dismiss a
	// real decision about their own file.
	t.Run("publish: the reclassification claims the name, not the PR's diff", func(t *testing.T) {
		const namesake = "bots/whats-next/manifest.yaml"
		write(namesake, "version: 0.1.0\n")
		git("add", "-A")
		git("commit", "-q", "-m", "a manifest no pull request touches")
		write(namesake, "version: 0.1.1\n")
		t.Cleanup(func() { git("checkout", "--quiet", "--", namesake) })

		const question = "Should manifest.yaml bump the minor version for this new input, or is a patch bump the convention?"
		got := runPublish(t, strPtr(" M "+namesake), "[]", false, freeText{questions: question})

		anomalyAt := strings.Index(got.Summary, "Anomalie workspace")
		at := strings.Index(got.Summary, question)
		if at < 0 || anomalyAt < 0 || at < anomalyAt {
			t.Fatalf("anomaly at %d, question at %d — a question naming a drifted basename is reclassified, never dropped:\n%s", anomalyAt, at, got.Summary)
		}
		if strings.Contains(got.Summary, "hors diff de la PR) — "+question) {
			t.Errorf("the published line claims the question is outside the PR's diff, which the name match never established:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "Question reclassée ici (nomme un fichier non commité listé ci-dessus) — "+question) {
			t.Errorf("the reclassified question must carry the reason the router actually has:\n%s", got.Summary)
		}
	})

	// The label the two perimeter subtests below pin, shared so the second
	// cannot drift from the first.
	const perimeterReclassified = "Périmètre déclaré nommant un fichier non commité listé ci-dessus, donc non publié comme revu : "

	// scanned_areas is the third reviewer-authored free text publish prints,
	// and the only one that is a CLAIM rather than a request: the perimeter
	// audited. An area naming a drifted entry claims a read of a file rule 2d
	// puts out of scope WHOLE — a façade in the channel built to be
	// falsifiable. Reclassified like a question: still printed, under what it
	// is, never under "Périmètre revu".
	t.Run("publish: an audited perimeter naming the drifted file is not published as reviewed", func(t *testing.T) {
		drift(t)
		const onPR = "tests:pkg/server/proxy.go"
		const onDrift = "correctness:" + drifted
		got := runPublish(t, strPtr(" M "+drifted), "[]", false, freeText{scope: onDrift + " ; " + onPR})

		reviewed := ""
		for _, l := range strings.Split(got.Summary, "\n") {
			if strings.HasPrefix(l, "Périmètre revu :") {
				reviewed = l
			}
		}
		if !strings.Contains(reviewed, onPR) {
			t.Errorf("the perimeter line lost the area the PR's own file was audited under (%q):\n%s", reviewed, got.Summary)
		}
		if strings.Contains(reviewed, drifted) {
			t.Errorf("the published review claims to have audited a file rule 2d puts out of scope (%q):\n%s", reviewed, got.Summary)
		}
		if !strings.Contains(got.Summary, perimeterReclassified+onDrift) {
			t.Errorf("the declared area must stay printed, under what it is — reclassified, never dropped:\n%s", got.Summary)
		}
	})

	// An area carries exactly what a question carries: a NAME. 5056 of this
	// tree's 13993 tracked files share a basename and the reviewer records
	// an area the way prose does, so the colliding area is as likely to be
	// the pull request's OWN file — audited — as the drifted one. The
	// published line says what the match establishes and no more: claiming
	// the file was not reviewed would disclaim coverage the review
	// delivered, and send the reader re-reviewing a file nobody drifted.
	t.Run("publish: the perimeter reclassification claims the name, not a missing audit", func(t *testing.T) {
		const namesake = "bots/issue-triage/skills/iterion-bot-catalog.md"
		if err := os.MkdirAll(ws+"/bots/issue-triage/skills", 0o755); err != nil {
			t.Fatal(err)
		}
		write(namesake, "the catalogue this pull request regenerates\n")
		git("add", "-A")
		git("commit", "-q", "-m", "the PR's own catalogue, committed and clean")
		drift(t)

		const area = "correctness:iterion-bot-catalog.md"
		got := runPublish(t, strPtr(" M "+drifted), "[]", false, freeText{scope: area + " ; tests:pkg/server/proxy_test.go"})

		if strings.Contains(got.Summary, "donc non revu") {
			t.Errorf("the published line disclaims an audit the name match never established — %s is committed, clean and audited:\n%s", namesake, got.Summary)
		}
		if !strings.Contains(got.Summary, perimeterReclassified+area) {
			t.Errorf("the declared area must print under the reason the router actually has:\n%s", got.Summary)
		}
	})

	// A one-segment extension-less entry is a path AND an ordinary English
	// word — the imperative-verb script a project root carries (build, test,
	// run, configure). Matched bare, it hands every question using that word
	// to the anomaly section under a label claiming the line names an
	// uncommitted file, and swallows the AXIS token of a scanned area, which
	// can never be a filename. All lowercase, it is a name only in a rooted
	// rendering, which prose does not produce; a capital names the file in
	// prose (LICENSE, Makefile, Dockerfile) and is matched bare. The test is
	// the ABSENCE of an uppercase letter, not lowercase-ness: a caseless
	// name (a 2024 release note) reads as prose exactly as build does.
	t.Run("publish: a root-level extension-less entry is a word, not a name", func(t *testing.T) {
		write("build", "#!/bin/sh\nexec go build ./...\n")
		write("tests", "#!/bin/sh\nexec go test ./...\n")
		write("LICENSE", "MIT\n")
		write("2024", "release notes\n")
		git("add", "-A")
		git("commit", "-q", "-m", "a root-level script pair, a licence and a year")
		for _, f := range []string{"build", "tests", "LICENSE", "2024"} {
			write(f, "uncommitted drift\n")
		}
		t.Cleanup(func() { git("checkout", "--quiet", "--", "build", "tests", "LICENSE", "2024") })

		const onPR = "Does the new flag need a build tag, or is the runtime check enough?"
		const onPRToo = "Is pkg/server/proxy.go meant to build under the race detector too?"
		const caseless = "Does the 2024 migration still apply to the new route?"
		const rooted = "Why does ./build carry an uncommitted edit?"
		const onLicence = "Does the new vendored module need a line in LICENSE?"
		const area = "tests:pkg/server/proxy_test.go"
		got := runPublish(t, strPtr(" M build\n M tests\n M LICENSE\n M 2024"), "[]", false, freeText{
			questions: strings.Join([]string{onPR, onPRToo, caseless, rooted, onLicence}, "\n"),
			scope:     area + " ; docs:README.md",
		})

		anomalyAt := strings.Index(got.Summary, "Anomalie workspace")
		if anomalyAt < 0 {
			t.Fatalf("the drifted entries raised no anomaly section:\n%s", got.Summary)
		}
		for _, q := range []string{onPR, onPRToo, caseless} {
			at := strings.Index(got.Summary, q)
			if at < 0 || at > anomalyAt {
				t.Errorf("question %q merely USES a drifted entry's bare name as a word and was reclassified as workspace state (at %d, anomaly at %d):\n%s", q, at, anomalyAt, got.Summary)
			}
		}
		for _, q := range []string{rooted, onLicence} {
			at := strings.Index(got.Summary, q)
			if at < 0 || at < anomalyAt {
				t.Errorf("question %q names a drifted entry and must be reclassified (at %d, anomaly at %d):\n%s", q, at, anomalyAt, got.Summary)
			}
		}
		reviewed := ""
		for _, l := range strings.Split(got.Summary, "\n") {
			if strings.HasPrefix(l, "Périmètre revu :") {
				reviewed = l
			}
		}
		if !strings.Contains(reviewed, area) {
			t.Errorf("the area's AXIS token was taken for the drifted file name, so the review disclaims auditing %s (%q):\n%s", area, reviewed, got.Summary)
		}
	})

	// A rooted rendering is only this workspace's own file when the slash
	// OPENS the word: the left boundary admits every character outside
	// [\w./-] before it, and ~/catalog.md is the operator's own copy while
	// file:/catalog.md is a URL — both other files, whose questions stay the
	// maintainer's.
	t.Run("publish: a home- or scheme-relative rendering is another file", func(t *testing.T) {
		write("catalog.md", "uncommitted drift\n")
		t.Cleanup(func() { git("checkout", "--quiet", "--", "catalog.md") })

		const atHome = "Is ~/catalog.md the operator's own copy, or should this PR regenerate it?"
		const scheme = "Does file:/catalog.md resolve to the same document the loader reads?"
		// The residual of that closed set, pinned so widening it is a choice:
		// a quote-delimited rooted rendering stays the maintainer's.
		const quoted = `Is the loader's "/catalog.md" default still right here?`
		const inWorkspace = "Why does /catalog.md carry an uncommitted edit?"
		// The other members of that opening set, and the bare form of a
		// ROOT-LEVEL dotted entry: each is the only witness of its own term.
		// A question OPENS on the rendering as often as it buries it mid
		// sentence, prose puts a path in parens or brackets, and a root-level
		// entry named by its bare name is this tree's likeliest real case —
		// CHANGELOG.md, go.mod, go.sum and devbox.lock all sit at the root and
		// are generator output, so #1674's failure mode reopens exactly there.
		const lineStartRooted = "/catalog.md carries an uncommitted edit — intended?"
		const lineStartDotted = "./catalog.md carries an uncommitted edit — intended?"
		const inParens = "Was the regeneration (./catalog.md) meant to ship here?"
		const inBrackets = "Was the regeneration [./catalog.md] meant to ship here?"
		const bareName = "Was catalog.md regenerated on purpose in this PR?"
		reclassified := []string{inWorkspace, lineStartRooted, lineStartDotted, inParens, inBrackets, bareName}
		got := runPublish(t, strPtr(" M catalog.md"), "[]", false, freeText{questions: strings.Join(append([]string{atHome, scheme, quoted}, reclassified...), "\n")})

		anomalyAt := strings.Index(got.Summary, "Anomalie workspace")
		if anomalyAt < 0 {
			t.Fatalf("the drifted entry raised no anomaly section:\n%s", got.Summary)
		}
		for _, q := range []string{atHome, scheme, quoted} {
			at := strings.Index(got.Summary, q)
			if at < 0 || at > anomalyAt {
				t.Errorf("question %q does not name this workspace's file, yet it was published as workspace drift (at %d, anomaly at %d):\n%s", q, at, anomalyAt, got.Summary)
			}
		}
		for _, q := range reclassified {
			at := strings.Index(got.Summary, q)
			if at < 0 || at < anomalyAt {
				t.Errorf("question %q names this workspace's drifted file and must be reclassified (at %d, anomaly at %d):\n%s", q, at, anomalyAt, got.Summary)
			}
		}
	})

	// A finding is routed on its ANCHOR, and the reviewer is handed the
	// workspace as an absolute path with an order to run `git -C` there: the
	// anchor it writes may carry ./, / or that directory. The same file
	// under another rendering is the same file, so the rendering is
	// normalized before the comparison — or the drift's finding posts
	// inline, reddens the gate and leaves the difference unnamed, #1666's
	// failure mode under a path the strip never recognised.
	t.Run("publish: a rooted rendering of a finding's anchor is the same file", func(t *testing.T) {
		for _, form := range []string{"./" + drifted, "/" + drifted, ws + "/" + drifted} {
			drift(t)
			f := `{"severity":"high","category":"correctness","title":"stale launch vars","detail":"d","file":"` + form + `","line":1}`
			got := runPublish(t, strPtr(" M "+drifted), "["+f+"]", true)

			if len(got.Comments) != 0 {
				t.Errorf("anchor %q: a finding on the drifted file posted inline: %v", form, got.Comments)
			}
			if blocking, _ := got.Gate["blocking_count"].(float64); blocking != 0 {
				t.Errorf("anchor %q: the drift's finding armed the merge gate (blocking_count = %v):\n%s", form, got.Gate["blocking_count"], got.Summary)
			}
			// Printed under the rendering the drifted entries above it carry,
			// so the reader matches the reclassified finding to its drift
			// instead of comparing two spellings of one path.
			if !strings.Contains(got.Summary, "Finding reclassé ici (hors diff de la PR) — "+drifted+":1") {
				t.Errorf("anchor %q: the finding must stay readable, reclassified as workspace state and named like the entry above it:\n%s", form, got.Summary)
			}
		}
	})

	// A KEPT finding's anchor reaches the forge, which anchors an inline
	// comment on a REPO-RELATIVE path. The renderings the strip normalizes
	// away must not be published raw: a create-review call is all-or-nothing,
	// so one rooted anchor 422s the whole review and the retry folds EVERY
	// inline comment into the body (pkg/forge/github/reviews.go) — the review
	// posts zero inline comments over one path the reviewer spelled rooted.
	t.Run("publish: a kept finding's rooted anchor is published repo-relative", func(t *testing.T) {
		const onPR = "pkg/server/proxy.go"
		if err := os.MkdirAll(ws+"/pkg/server", 0o755); err != nil {
			t.Fatal(err)
		}
		write(onPR, "package server\n")
		git("add", "-A")
		git("commit", "-q", "-m", "the file the pull request changes")

		// Every rendering the reviewer can copy out of what the prompt hands
		// it: the workspace variable (rooted), a diff file header (a/ and b/),
		// and the two combined — the normalization runs to a fixpoint, or a
		// doubled rendering reaches the forge still rooted.
		for _, form := range []string{"./" + onPR, "/" + onPR, ws + "/" + onPR, "b/" + onPR, "a/" + onPR, ws + "/./" + onPR, "//" + onPR} {
			f := `{"severity":"high","category":"correctness","title":"stale timeout","detail":"d","file":"` + form + `","line":12}`
			got := runPublish(t, strPtr(""), "["+f+"]", false)

			if len(got.Comments) != 1 {
				t.Fatalf("anchor %q: want the one finding posted inline, got %v", form, got.Comments)
			}
			if got.Comments[0]["path"] != onPR {
				t.Errorf("anchor %q published as %q, want %q — a rooted anchor 422s the review and costs every inline comment", form, got.Comments[0]["path"], onPR)
			}
		}
	})

	// git renders every file header of a diff as a/<path> and b/<path>, and
	// rule 2 of REVIEW SCOPE orders the reviewer to read that diff: the anchor
	// it copies out of one carries that prefix. Unstripped, the drift test
	// misses the drifted file and the finding posts inline and arms the merge
	// gate over an edit the pull request does not contain — #1666's failure
	// mode on the channel that decides the merge. The prefix is dropped
	// against the WORKSPACE, never blindly: a repo owning a top-level a/ or b/
	// keeps its own anchor, and an anchor the workspace resolves neither way
	// stays exactly as the reviewer wrote it.
	t.Run("publish: a diff-header rendering of a finding's anchor is the same file", func(t *testing.T) {
		const owned = "b/own.md"
		if err := os.MkdirAll(ws+"/b", 0o755); err != nil {
			t.Fatal(err)
		}
		// Another tree holding this very path, reachable from the workspace
		// through .. — the shape every run has (the engine gives each run a
		// worktree beside the others).
		sibling := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(sibling+"/"+drifted), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sibling+"/"+drifted, []byte("another run's tree\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		toSibling, err := filepath.Rel(ws, sibling)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(toSibling, "..") {
			t.Fatalf("the second tree must be reached through .. for this case to mean anything, got %q", toSibling)
		}
		write("own.md", "committed\n")
		write(owned, "a file this repo owns under its own b/ directory\n")
		git("add", "-A")
		git("commit", "-q", "-m", "a repo owning a top-level b/")

		for _, c := range []struct {
			name, anchor, path string
		}{
			{"b/ prefix", "b/" + drifted, ""},
			{"a/ prefix", "a/" + drifted, ""},
			// own.md is the drifted entry and b/own.md a file this repo holds:
			// a blind strip would reclassify a real finding on a real file and
			// green the gate over it.
			{"a path the repo owns under b/", owned, owned},
			// Neither rendering is a path of this workspace: there is nothing to
			// resolve, and inventing one would publish an anchor nobody wrote (a
			// path the change deletes reads the same way).
			{"a prefixed path the workspace does not hold", "b/absent.go", "b/absent.go"},
			// A rendering that climbs OUT of the workspace is not a path of it,
			// even when it RESOLVES: the tree it lands on is another run's. Left
			// as written — loud, beside the anomaly section that names the drift
			// — never published as an anchor into a tree the PR does not hold.
			{"a prefixed rendering reaching another tree", "b/" + toSibling + "/" + drifted, "b/" + toSibling + "/" + drifted},
		} {
			t.Run(c.name, func(t *testing.T) {
				drift(t)
				write("own.md", "uncommitted drift\n")
				t.Cleanup(func() { git("checkout", "--quiet", "--", "own.md") })

				f := `{"severity":"high","category":"correctness","title":"stale launch vars","detail":"d","file":"` + c.anchor + `","line":1}`
				got := runPublish(t, strPtr(" M "+drifted+"\n M own.md"), "["+f+"]", true)
				blocking, _ := got.Gate["blocking_count"].(float64)

				if c.path == "" {
					if len(got.Comments) != 0 || blocking != 0 {
						t.Fatalf("anchor %q: the drift's finding posted inline (%v) or armed the gate (blocking_count = %v):\n%s", c.anchor, got.Comments, got.Gate["blocking_count"], got.Summary)
					}
					if !strings.Contains(got.Summary, "Finding reclassé ici (hors diff de la PR) — "+drifted+":1") {
						t.Errorf("anchor %q: the finding must stay readable, reclassified and named like the entry above it:\n%s", c.anchor, got.Summary)
					}
					return
				}
				if len(got.Comments) != 1 || got.Comments[0]["path"] != c.path {
					t.Fatalf("anchor %q: posted comments = %v, want exactly one on %q", c.anchor, got.Comments, c.path)
				}
				if blocking != 1 {
					t.Errorf("anchor %q: a finding the workspace does not resolve to a drifted entry left the merge gate (blocking_count = %v):\n%s", c.anchor, got.Gate["blocking_count"], got.Summary)
				}
				if strings.Contains(got.Summary, "Finding reclassé") {
					t.Errorf("anchor %q: the finding left the pull request over a prefix the workspace does not resolve away:\n%s", c.anchor, got.Summary)
				}
			})
		}
	})

	// The deliberate asymmetry between the channels, witnessed. A finding
	// anchored on a BARE basename is name-shaped, exactly like a question —
	// but routing it by name would decide a MERGE on a basename, and 5056 of
	// this tree's 13993 tracked files share one: the pull request's own
	// namesake defect would leave the findings and green the gate. On the
	// blocking channel the ambiguity resolves loud: the finding stays the
	// pull request's, the anomaly section names the drift beside it, and the
	// author arbitrates with both in front of them.
	t.Run("publish: a finding anchored on a bare basename stays the pull request's", func(t *testing.T) {
		drift(t)
		const onNamesake = `{"severity":"high","category":"correctness","title":"the catalogue drops the launch vars","detail":"d","file":"iterion-bot-catalog.md","line":1}`
		const question = "Should iterion-bot-catalog.md keep the two-column layout in this PR?"
		got := runPublish(t, strPtr(" M "+drifted), "["+onNamesake+"]", true, freeText{questions: question})

		if blocking, _ := got.Gate["blocking_count"].(float64); blocking != 1 {
			t.Errorf("a name match greened the merge gate over a finding whose anchor could be the PR's own namesake (blocking_count = %v):\n%s", got.Gate["blocking_count"], got.Summary)
		}
		if strings.Contains(got.Summary, "Finding reclassé") {
			t.Errorf("the finding left the pull request on a basename the match cannot tie to the drifted file:\n%s", got.Summary)
		}
		if !strings.Contains(got.Summary, "Anomalie workspace") || !strings.Contains(got.Summary, "Question reclassée") {
			t.Errorf("the drift must still be named beside the finding, with the free-text channel routed:\n%s", got.Summary)
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
