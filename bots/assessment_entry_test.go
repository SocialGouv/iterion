package bots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The two entry preconditions, and both are NAMED refusals rather than
// degraded modes.
//
// A run that assesses a truncated clone is confident and wrong: a --depth=1
// clone answers every history question about the CLONE, and the sentence that
// comes out — "the delivered history is a single squashed commit" — is a claim
// about a repository that may carry thousands.
// A run with no declared brief publishes a programme it invented, which is
// indistinguishable on the page from one somebody agreed to.

func probe(t *testing.T, ws string) map[string]any {
	t.Helper()
	out, exit, stderr := assessmentRun(t, "workspace_probe",
		map[string]string{"{{vars.workspace_dir}}": ws}, nil)
	if exit != 0 {
		t.Fatalf("workspace_probe exited %d: %s", exit, stderr)
	}
	return out
}

func TestAssessmentWorkspaceProbe(t *testing.T) {
	requireAssessmentTools(t)

	t.Run("a full repository pins its commit", func(t *testing.T) {
		dir, sha := synthRepo(t, map[string]string{"README.md": "# fixture\n"})
		out := probe(t, dir)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a plain repository was refused: %s", assessmentString(t, out, "reason"))
		}
		if assessmentString(t, out, "base_sha") != sha {
			t.Fatalf("base_sha = %q, want %q — every later measurement is taken AT this commit",
				assessmentString(t, out, "base_sha"), sha)
		}
		if assessmentString(t, out, "fingerprint") == "" {
			t.Fatal("no input fingerprint: a resume could continue over a moved tree without saying so")
		}
	})

	t.Run("the fingerprint moves when the inputs move", func(t *testing.T) {
		dir, _ := synthRepo(t, map[string]string{"README.md": "# fixture\n"})
		before := assessmentString(t, probe(t, dir), "fingerprint")

		// A skill edited between two passes changes what the extractors DO.
		// A fingerprint blind to that would certify two different runs alike.
		skills := filepath.Join(dir, ".claude", "skills")
		if err := os.MkdirAll(skills, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skills, "stack-x.md"), []byte("v1"), 0o644); err != nil {
			t.Fatal(err)
		}
		after := assessmentString(t, probe(t, dir), "fingerprint")
		if before == after {
			t.Fatal("the fingerprint did not move when a mirrored extractor skill appeared — it is not fingerprinting the inputs it claims to")
		}
	})

	t.Run("a shallow clone", func(t *testing.T) {
		origin, _ := synthRepo(t, map[string]string{"README.md": "one\n"})
		if err := os.WriteFile(filepath.Join(origin, "second.txt"), []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, origin, "add", "second.txt")
		gittest.Run(t, origin, "commit", "-qm", "second")

		shallow := filepath.Join(t.TempDir(), "shallow")
		gittest.Run(t, t.TempDir(), "clone", "-q", "--depth=1", "file://"+origin, shallow)

		out := probe(t, shallow)
		if assessmentBool(t, out, "ok") {
			t.Fatal("a SHALLOW clone was accepted — every history measurement it can answer is a property of the clone, not of the repository")
		}
		if code := assessmentString(t, out, "code"); code != "WORKSPACE_SHALLOW" {
			t.Fatalf("code = %q, want WORKSPACE_SHALLOW", code)
		}
	})

	t.Run("a directory that is not a repository", func(t *testing.T) {
		out := probe(t, t.TempDir())
		if assessmentBool(t, out, "ok") {
			t.Fatal("a directory with no git repository in it was accepted")
		}
		if code := assessmentString(t, out, "code"); code != "WORKSPACE_NOT_A_REPO" {
			t.Fatalf("code = %q, want WORKSPACE_NOT_A_REPO", code)
		}
	})
}

const aGoodBrief = `version: 1
objective: |
  The served application must reach a runtime under active support before its
  current one stops receiving fixes.
goals:
  - id: supported-runtime
    statement: "the application runs on a runtime under active support"
    rationale: "support for the current one ends on the date below"
targets:
  - component: "the runtime"
    current: "2.9"
    target: "5.1"
    decided_by: "the platform group"
    decided_on: "2026-01-31"
permitted_changes:
  - "dependency majors, when the behavioural net stays green"
forbidden_changes:
  - "the public interface of the reporting module"
decisions:
  - id: datastore-engine
    decision: "the alternative engine"
    decided_by: "the platform group"
    decided_on: "2026-01-31"
owner: "the platform group"
`

func readBrief(t *testing.T, body string) map[string]any {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.MkdirAll(filepath.Join(dir, ".modernize"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".modernize", "brief.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, exit, stderr := assessmentRun(t, "brief_read", map[string]string{
		"{{vars.workspace_dir}}": dir,
		"{{vars.brief_path}}":    ".modernize/brief.yaml",
	}, nil)
	if exit != 0 {
		t.Fatalf("brief_read exited %d: %s", exit, stderr)
	}
	return out
}

func TestAssessmentBriefRefusals(t *testing.T) {
	requireAssessmentTools(t, "python3", "git", "yq")

	t.Run("a complete brief is read and summarised", func(t *testing.T) {
		out := readBrief(t, aGoodBrief)
		if !assessmentBool(t, out, "ok") {
			t.Fatalf("a complete brief was refused: %s", assessmentString(t, out, "reason"))
		}
		summary := assessmentString(t, out, "summary")
		for _, want := range []string{"Objective", "Goals", "Decisions already taken", "Owner"} {
			if !strings.Contains(summary, want) {
				t.Errorf("the digest handed to the agents carries no %q section:\n%s", want, summary)
			}
		}
		if !strings.Contains(summary, "NOT decided") {
			t.Error("the digest does not tell the agents that anything unstated is undecided — that sentence is what keeps a proposal from becoming a lot")
		}
	})

	t.Run("an absent brief", func(t *testing.T) {
		out := readBrief(t, "")
		if assessmentBool(t, out, "ok") {
			t.Fatal("the assessment ran with NO declared brief — it would publish a programme it invented")
		}
		if assessmentBool(t, out, "present") {
			t.Fatal("present is true for a file that does not exist; the operator would be told to repair a file they never wrote")
		}
		if code := assessmentString(t, out, "code"); code != "BRIEF_ABSENT" {
			t.Fatalf("code = %q, want BRIEF_ABSENT", code)
		}
	})

	t.Run("a brief missing what only a human can state", func(t *testing.T) {
		for _, tc := range []struct{ name, body, wants string }{
			{"no objective", strings.Replace(aGoodBrief, "objective: |", "unused: |", 1), "objective"},
			{"no goals", strings.Replace(aGoodBrief, "goals:", "notgoals:", 1), "goals"},
			{"no owner", strings.Replace(aGoodBrief, `owner: "the platform group"`, "", 1), "owner"},
			{"a goal with no statement", strings.Replace(aGoodBrief,
				`    statement: "the application runs on a runtime under active support"`, "", 1), "statement"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out := readBrief(t, tc.body)
				if assessmentBool(t, out, "ok") {
					t.Fatalf("a brief with %s was accepted", tc.name)
				}
				if !assessmentBool(t, out, "present") {
					t.Error("present is false for a file that IS there — the operator would be told to write one they already wrote")
				}
				if code := assessmentString(t, out, "code"); code != "BRIEF_INCOMPLETE" {
					t.Fatalf("code = %q, want BRIEF_INCOMPLETE", code)
				}
				if !strings.Contains(assessmentString(t, out, "reason"), tc.wants) {
					t.Errorf("the refusal does not name the missing field: %s", assessmentString(t, out, "reason"))
				}
			})
		}
	})

	t.Run("two goals under one id", func(t *testing.T) {
		doubled := strings.Replace(aGoodBrief, "targets:", `  - id: supported-runtime
    statement: "a second claim on the same key"
targets:`, 1)
		out := readBrief(t, doubled)
		if assessmentBool(t, out, "ok") {
			t.Fatal("two goals under one id were accepted — the contract's outcomes reference that id")
		}
	})
}
