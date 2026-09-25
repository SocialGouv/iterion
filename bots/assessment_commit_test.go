package bots

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// An assessment whose documents never landed produced a log line. The tree is
// the deliverable, so the commit node is a gate like any other: it verifies
// what LANDED, not that a command returned zero.
//
// It also commits by EXPLICIT PATH. A workspace carries whatever a previous
// node or the operator left in it, and a commit that swept that up would
// publish work this run neither produced nor reviewed, under a message that
// does not describe it.

// assessmentCommit runs the commit node over a workspace, naming the two
// rendered documents the way the flow does — the paths come from the nodes
// that wrote them — and handing it the fingerprint the probe takes at entry.
func assessmentCommit(t *testing.T, ws string) map[string]any {
	t.Helper()
	return assessmentCommitWith(t, ws,
		`["`+filepath.Join(ws, "docs", "assessment", "00-state-of-the-repository.md")+`"]`,
		`["`+filepath.Join(ws, "docs", "assessment", "01-modernisation-programme.md")+`"]`,
		entryFingerprint(t, ws))
}

// entryFingerprint is what the workflow hands the commit: the probe's own
// fingerprint of the tree the run started on, as a quoted string.
func entryFingerprint(t *testing.T, ws string) string {
	t.Helper()
	return strconv.Quote(assessmentString(t, probe(t, ws), "fingerprint"))
}

func assessmentCommitWith(t *testing.T, ws, documents, planDocuments, fingerprint string) map[string]any {
	t.Helper()
	out, exit, stderr := assessmentRun(t, "commit", map[string]string{
		"{{vars.workspace_dir}}":     ws,
		"{{vars.plan_path}}":         ".modernize/plan.yaml",
		"{{vars.survey_path}}":       ".modernize/survey.json",
		"{{vars.out_dir}}":           "docs/assessment",
		"{{vars.bundle_skills_dir}}": filepath.Join(ws, ".claude", "iterion-skills"),
	}, map[string]string{
		"{{input.documents}}":      documents,
		"{{input.plan_documents}}": planDocuments,
		"{{input.fingerprint}}":    fingerprint,
	})
	if exit != 0 {
		t.Fatalf("commit exited %d: %s", exit, stderr)
	}
	return out
}

// assessedWorkspace is a repository carrying a finished assessment, plus a
// file the run did not write.
func assessedWorkspace(t *testing.T) string {
	t.Helper()
	dir, _ := synthRepo(t, map[string]string{"README.md": "# fixture\n"})
	write := func(rel, body string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".modernize/plan.yaml", aGoodContract)
	write(".modernize/outcomes.json", goodOutcomes)
	write(".modernize/survey.json", `{"version": 1}`)
	write("docs/assessment/00-state-of-the-repository.md", "# State\n")
	write("docs/assessment/01-modernisation-programme.md", "# Programme\n")
	// Somebody else's work in flight, in the same checkout.
	write("src/unrelated.txt", "not this run's work\n")
	return dir
}

func TestAssessmentCommitLandsTheContract(t *testing.T) {
	requireAssessmentTools(t)

	t.Run("the assessment lands and nothing else does", func(t *testing.T) {
		ws := assessedWorkspace(t)
		out := assessmentCommit(t, ws)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a finished assessment was not committed: %s", assessmentString(t, out, "reason"))
		}
		landed := gittest.Run(t, ws, "show", "--stat", "--name-only", "--format=", "HEAD")
		for _, want := range []string{
			".modernize/plan.yaml", ".modernize/outcomes.json", ".modernize/survey.json",
			"docs/assessment/00-state-of-the-repository.md",
		} {
			if !strings.Contains(landed, want) {
				t.Errorf("the commit does not carry %s:\n%s", want, landed)
			}
		}
		if strings.Contains(landed, "src/unrelated.txt") {
			t.Fatalf("the commit swept up a file this run never wrote — the repository is not the bot's alone:\n%s", landed)
		}
	})

	// Somebody else's work already STAGED. `git commit` with no pathspec
	// commits the whole index, under a message that does not describe it.
	t.Run("a file somebody else staged does not land", func(t *testing.T) {
		ws := assessedWorkspace(t)
		gittest.Run(t, ws, "add", "--", "src/unrelated.txt")
		out := assessmentCommit(t, ws)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a finished assessment was not committed: %s", assessmentString(t, out, "reason"))
		}
		landed := gittest.Run(t, ws, "show", "--name-only", "--format=", "HEAD")
		if strings.Contains(landed, "src/unrelated.txt") {
			t.Fatalf("the commit carried a file somebody else had staged:\n%s", landed)
		}
		if staged := gittest.Run(t, ws, "diff", "--cached", "--name-only"); !strings.Contains(staged, "src/unrelated.txt") {
			t.Errorf("the other party's staging was lost: %q", staged)
		}
	})

	// The output directory is the bot's to write INTO, not to sweep. A file
	// under it that no node of this run reported writing is not this run's.
	t.Run("a file under the output directory this run did not write does not land", func(t *testing.T) {
		ws := assessedWorkspace(t)
		notes := filepath.Join(ws, "docs", "assessment", "notes-left-by-someone.md")
		if err := os.WriteFile(notes, []byte("not this run's\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := assessmentCommit(t, ws)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a finished assessment was not committed: %s", assessmentString(t, out, "reason"))
		}
		landed := gittest.Run(t, ws, "show", "--name-only", "--format=", "HEAD")
		if strings.Contains(landed, "notes-left-by-someone.md") {
			t.Fatalf("the commit swept up a file under the output directory that no node wrote:\n%s", landed)
		}
	})

	// An artefact byte-identical to the one already on the branch has LANDED:
	// it is in the tree. A check that read the commit's CHANGED files refused
	// every re-assessment whose outcomes came out the same.
	t.Run("an artefact identical to the committed one has landed", func(t *testing.T) {
		ws := assessedWorkspace(t)
		gittest.Run(t, ws, "add", "--", ".modernize/outcomes.json")
		gittest.Run(t, ws, "commit", "-qm", "a previous assessment's outcomes")
		out := assessmentCommit(t, ws)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("an artefact identical to the one already committed was refused as unlanded: %s",
				assessmentString(t, out, "reason"))
		}
	})

	// The commit runs --no-verify: a hook planted in the repository does not
	// run at all, so the validated contract is what gets committed. The
	// detection layers behind this one (the dirty check, the by-name check)
	// stay, for every writer that is not a hook.
	t.Run("a pre-commit hook planted in the repository does not run", func(t *testing.T) {
		ws := assessedWorkspace(t)
		hooks := filepath.Join(ws, ".git", "hooks")
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			t.Fatal(err)
		}
		// --no-verify covers only the commit-time hooks: a post-commit or
		// prepare-commit-msg hook ran anyway, and one that rewrites the
		// contract and commits the replacement passed every check here.
		// The core.hooksPath override leaves NO hook standing.
		hookBody := "printf 'rewritten by a hook\\n' >> docs/assessment/00-state-of-the-repository.md\n"
		for _, name := range []string{"pre-commit", "prepare-commit-msg", "post-commit"} {
			hook := "#!/bin/sh\n" + hookBody
			if err := os.WriteFile(filepath.Join(hooks, name), []byte(hook), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		out := assessmentCommit(t, ws)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("the commit refused to land with hooks planted in the repository: %s",
				assessmentString(t, out, "reason"))
		}
		body, err := os.ReadFile(filepath.Join(ws, "docs", "assessment", "00-state-of-the-repository.md"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "rewritten by a hook") {
			t.Fatal("the pre-commit hook RAN — the published document is not the one that was measured")
		}
	})

	t.Run("no contract is nothing to land", func(t *testing.T) {
		dir, _ := synthRepo(t, map[string]string{"README.md": "# fixture\n"})
		out := assessmentCommitWith(t, dir, "[]", "[]", entryFingerprint(t, dir))
		if assessmentBool(t, out, "ok") {
			t.Fatal("a run with no contract on disk reported a successful landing")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "nothing to land") {
			t.Errorf("the refusal does not say what is missing: %s", assessmentString(t, out, "reason"))
		}
	})
}

// FINISHED IS NOT `git commit` RETURNING ZERO. With `*.md` in the
// repository's own ignore rules, `git add` on the two rendered documents exits
// 0 and stages nothing: the run finished, the contract landed, and the branch
// carried no assessment beside it. The commit verifies each artefact BY NAME,
// in what landed.
func TestAssessmentCommitRefusesAnIgnoredDeliverable(t *testing.T) {
	requireAssessmentTools(t)

	for _, tc := range []struct{ name, ignore, wants string }{
		{"markdown ignored wholesale", "*.md\n", "IGNORED"},
		{"the output directory ignored", "docs/\n", "IGNORED"},
		{"the survey ignored", ".modernize/survey.json\n", "IGNORED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := assessedWorkspace(t)
			if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte(tc.ignore), 0o644); err != nil {
				t.Fatal(err)
			}
			out := assessmentCommit(t, ws)
			if assessmentBool(t, out, "ok") {
				t.Fatalf("an assessment whose deliverables the repository ignores reported a "+
					"successful landing (%s)", tc.ignore)
			}
			if !strings.Contains(assessmentString(t, out, "reason"), tc.wants) {
				t.Errorf("the refusal does not say why: %s", assessmentString(t, out, "reason"))
			}
		})
	}

	// The by-name check's other face, without the hook vector --no-verify
	// closed: a deliverable removed from the commit BEFORE it runs leaves
	// nothing to be dirty — no dirt, no tracked file, no diff — and the
	// commit lands without it while the command returns zero. Only verifying
	// each artefact BY NAME in what landed catches this.
	t.Run("a pre-commit hook that deletes a deliverable does not run", func(t *testing.T) {
		ws := assessedWorkspace(t)
		hooks := filepath.Join(ws, ".git", "hooks")
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			t.Fatal(err)
		}
		hook := "#!/bin/sh\ngit rm -q --cached -- docs/assessment/01-modernisation-programme.md\n" +
			"rm -f docs/assessment/01-modernisation-programme.md\n"
		if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
		out := assessmentCommit(t, ws)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("the commit refused with a deleting hook planted in the repository: %s",
				assessmentString(t, out, "reason"))
		}
		if _, err := os.Stat(filepath.Join(ws, "docs", "assessment", "01-modernisation-programme.md")); err != nil {
			t.Fatal("the deleting hook RAN — the deliverable it targeted never landed")
		}
	})

	// The case only a read of the TREE sees. A hook that rewinds the branch
	// after the commit leaves nothing dirty — the artefacts are gone from the
	// index and the disk alike — and `git commit` has already returned zero.
	t.Run("a hook that rewinds the branch after the commit", func(t *testing.T) {
		ws := assessedWorkspace(t)
		hooks := filepath.Join(ws, ".git", "hooks")
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			t.Fatal(err)
		}
		hook := "#!/bin/sh\ngit reset -q --hard HEAD~1\n"
		if err := os.WriteFile(filepath.Join(hooks, "post-commit"), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
		out := assessmentCommit(t, ws)
		if assessmentBool(t, out, "ok") {
			t.Fatal("an assessment whose commit a hook rewound was reported as landed — the branch " +
				"carries none of it")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "does not carry") {
			t.Errorf("the refusal is not the tree read's: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("a document the run reported and never wrote", func(t *testing.T) {
		ws := assessedWorkspace(t)
		if err := os.Remove(filepath.Join(ws, "docs", "assessment", "01-modernisation-programme.md")); err != nil {
			t.Fatal(err)
		}
		out := assessmentCommit(t, ws)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a document the rendering node reported writing, absent from disk, was reported as landed")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "not on disk") {
			t.Errorf("the refusal does not name the absence: %s", assessmentString(t, out, "reason"))
		}
	})
}

// THE FINGERPRINT IS CONSUMED, not merely computed. It is taken at the probe
// and compared here, seconds before the figures land: every number about to be
// published was measured at a commit, over tags, against this bundle's skills,
// and a run that continued over a moved one would publish two states as one
// document.
func TestAssessmentCommitRefusesATreeThatMovedUnderTheRun(t *testing.T) {
	requireAssessmentTools(t)

	t.Run("the fingerprint the probe took is the one the commit finds", func(t *testing.T) {
		ws := assessedWorkspace(t)
		before := probe(t, ws)
		out := assessmentCommitWith(t, ws,
			`["`+filepath.Join(ws, "docs", "assessment", "00-state-of-the-repository.md")+`"]`,
			`["`+filepath.Join(ws, "docs", "assessment", "01-modernisation-programme.md")+`"]`,
			strconv.Quote(assessmentString(t, before, "fingerprint")))
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("an unmoved tree was refused: %s", assessmentString(t, out, "reason"))
		}
	})

	// No fingerprint switched the comparison off: the check read `if
	// fingerprint:` and a value that failed to travel skipped it in silence.
	t.Run("no fingerprint is a refusal, never a skipped check", func(t *testing.T) {
		ws := assessedWorkspace(t)
		out := assessmentCommitWith(t, ws,
			`["`+filepath.Join(ws, "docs", "assessment", "00-state-of-the-repository.md")+`"]`,
			`["`+filepath.Join(ws, "docs", "assessment", "01-modernisation-programme.md")+`"]`,
			`""`)
		if assessmentBool(t, out, "ok") {
			t.Fatal("the assessment landed with no input fingerprint — nothing compared the tree it " +
				"was measured on with the tree it was committed to")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "fingerprint") {
			t.Errorf("the refusal does not name the missing fingerprint: %s", assessmentString(t, out, "reason"))
		}
	})

	t.Run("a tag that appeared under the run", func(t *testing.T) {
		ws := assessedWorkspace(t)
		before := probe(t, ws)
		gittest.Run(t, ws, "tag", "v-appeared-mid-run")
		out := assessmentCommitWith(t, ws,
			`["`+filepath.Join(ws, "docs", "assessment", "00-state-of-the-repository.md")+`"]`,
			`["`+filepath.Join(ws, "docs", "assessment", "01-modernisation-programme.md")+`"]`,
			strconv.Quote(assessmentString(t, before, "fingerprint")))
		if assessmentBool(t, out, "ok") {
			t.Fatal("the inputs moved under the run and the assessment landed anyway — the " +
				"published tag count was measured against a tree that no longer exists")
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "moved under this run") {
			t.Errorf("the refusal does not say what happened: %s", assessmentString(t, out, "reason"))
		}
	})
}
