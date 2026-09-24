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
// that wrote them.
func assessmentCommit(t *testing.T, ws string) map[string]any {
	t.Helper()
	return assessmentCommitWith(t, ws,
		`["`+filepath.Join(ws, "docs", "assessment", "00-state-of-the-repository.md")+`"]`,
		`["`+filepath.Join(ws, "docs", "assessment", "01-modernisation-programme.md")+`"]`,
		`""`)
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

	// A commit hook that rewrites a file the commit was carrying leaves it
	// WRITTEN and not LANDED — and the command still returned zero. Reading the
	// return code would report a successful assessment whose document on the
	// branch is not the document that was measured. So the tree is re-read
	// after the commit, not the exit status.
	t.Run("a hook that rewrites a document after staging is caught", func(t *testing.T) {
		ws := assessedWorkspace(t)
		hooks := filepath.Join(ws, ".git", "hooks")
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			t.Fatal(err)
		}
		hook := "#!/bin/sh\nprintf 'rewritten by a hook\\n' >> docs/assessment/00-state-of-the-repository.md\n"
		if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}

		out := assessmentCommit(t, ws)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a document a hook rewrote out from under the commit was reported as landed — " +
				"the branch would carry a state document nobody measured")
		}
		if code := assessmentString(t, out, "code"); code != "COMMIT_FAILED" {
			t.Fatalf("code = %q, want COMMIT_FAILED", code)
		}
		if !strings.Contains(assessmentString(t, out, "reason"), "uncommitted") {
			t.Errorf("the refusal does not say what is still uncommitted: %s",
				assessmentString(t, out, "reason"))
		}
	})

	t.Run("no contract is nothing to land", func(t *testing.T) {
		dir, _ := synthRepo(t, map[string]string{"README.md": "# fixture\n"})
		out := assessmentCommitWith(t, dir, "[]", "[]", `""`)
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
