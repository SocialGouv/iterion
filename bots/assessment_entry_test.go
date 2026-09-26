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

// probe runs the entry node the way the engine sets a run up: the
// engine-owned skills copy always exists — it is reset and refilled on every
// mirror pass, even for a run with no bundle.
func probe(t *testing.T, ws string) map[string]any {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(ws, ".claude", "iterion-skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, exit, stderr := assessmentRun(t, "workspace_probe",
		map[string]string{"{{vars.workspace_dir}}": ws,
			"{{vars.bundle_skills_dir}}": filepath.Join(ws, ".claude", "iterion-skills")}, nil)
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

	t.Run("the fingerprint moves with the BUNDLE's skills and not the checkout's", func(t *testing.T) {
		dir, _ := synthRepo(t, map[string]string{"README.md": "# fixture\n"})
		before := assessmentString(t, probe(t, dir), "fingerprint")

		// What the repository under assessment puts in the workspace mirror is
		// not an input to this bot: the blocks it executes come from the
		// engine-owned copy. A fingerprint that moved here would be recording
		// the audited tree's edits as the bundle's.
		checkout := filepath.Join(dir, ".claude", "skills")
		if err := os.MkdirAll(checkout, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(checkout, "stack-x.md"), []byte("supplied by the checkout"), 0o644); err != nil {
			t.Fatal(err)
		}
		if assessmentString(t, probe(t, dir), "fingerprint") != before {
			t.Fatal("the fingerprint moved when the CHECKOUT's skills mirror changed — the probe is " +
				"fingerprinting the repository under assessment as though it were the bundle")
		}

		// A skill edited between two passes changes what the extractors DO.
		// A fingerprint blind to that would certify two different runs alike.
		bundle := filepath.Join(dir, ".claude", "iterion-skills")
		if err := os.MkdirAll(bundle, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, "stack-x.md"), []byte("v1"), 0o644); err != nil {
			t.Fatal(err)
		}
		after := assessmentString(t, probe(t, dir), "fingerprint")
		if before == after {
			t.Fatal("the fingerprint did not move when a bundle extractor skill appeared — it is not fingerprinting the inputs it claims to")
		}
	})

	t.Run("no bundle-owned skills directory is a named refusal", func(t *testing.T) {
		dir, _ := synthRepo(t, map[string]string{"README.md": "# fixture\n"})
		out, exit, stderr := assessmentRun(t, "workspace_probe",
			map[string]string{"{{vars.workspace_dir}}": dir, "{{vars.bundle_skills_dir}}": ""}, nil)
		if exit != 0 {
			t.Fatalf("workspace_probe exited %d: %s", exit, stderr)
		}
		if assessmentBool(t, out, "ok") {
			t.Fatal("a run with no engine-owned skills directory was accepted — every later node " +
				"would read the blocks it executes from the repository under assessment")
		}
		if code := assessmentString(t, out, "code"); code != "BUNDLE_SKILLS_UNAVAILABLE" {
			t.Fatalf("code = %q, want BUNDLE_SKILLS_UNAVAILABLE", code)
		}
		if !assessmentBool(t, out, "skills_missing") {
			t.Fatal("the refusal does not route to its own fail node: skills_missing is false, so the " +
				"operator is told the workspace is not a repository")
		}
	})

	// An engine that predates the owned copy leaves the reference unexpanded
	// or pointing nowhere. Refused HERE, by its cause: past the probe, each node
	// would refuse with its own symptom — no manifest shape, no profile.
	t.Run("an owned skills directory that is not there is a named refusal", func(t *testing.T) {
		dir, _ := synthRepo(t, map[string]string{"README.md": "# fixture\n"})
		out, exit, stderr := assessmentRun(t, "workspace_probe",
			map[string]string{"{{vars.workspace_dir}}": dir,
				"{{vars.bundle_skills_dir}}": filepath.Join(dir, ".claude", "no-such-directory")}, nil)
		if exit != 0 {
			t.Fatalf("workspace_probe exited %d: %s", exit, stderr)
		}
		if assessmentBool(t, out, "ok") {
			t.Fatal("a skills directory that does not exist was accepted — the floor would then " +
				"refuse for want of a manifest shape, naming a symptom instead of the cause")
		}
		if code := assessmentString(t, out, "code"); code != "BUNDLE_SKILLS_UNAVAILABLE" {
			t.Fatalf("code = %q, want BUNDLE_SKILLS_UNAVAILABLE", code)
		}
		if !assessmentBool(t, out, "skills_missing") {
			t.Fatal("skills_missing is false: the refusal routes to the wrong fail node")
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

// THE SWEEP DECISION TRAVELS: crosses_major is what makes a sweep mandatory,
// and the lint enforces it — so the summary the drafting agent reads must
// carry the decision the brief already took, not leave it to be guessed.
func TestAssessmentBriefSummaryCarriesTheSweepDecision(t *testing.T) {
	requireAssessmentTools(t)
	with := strings.Replace(aGoodBrief, `    target: "5.1"`,
		"    target: \"5.1\"\n    crosses_major: true", 1)
	if with == aGoodBrief {
		t.Fatal("the mutation did not apply")
	}
	out := readBrief(t, with)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("brief refused: %s", assessmentString(t, out, "reason"))
	}
	if !strings.Contains(assessmentString(t, out, "summary"), "IS due") {
		t.Errorf("the summary drops the explicit crosses_major: true — the agent would guess a decision the brief took")
	}

	without := strings.Replace(aGoodBrief, `    target: "5.1"`,
		"    target: \"5.1\"\n    crosses_major: false", 1)
	out = readBrief(t, without)
	if !assessmentBool(t, out, "ok") {
		t.Fatalf("brief refused: %s", assessmentString(t, out, "reason"))
	}
	if !strings.Contains(assessmentString(t, out, "summary"), "NOT due") {
		t.Errorf("the summary drops the explicit crosses_major: false: %s", assessmentString(t, out, "summary"))
	}
}

func TestAssessmentBriefRefusesATargetWithoutAComponent(t *testing.T) {
	requireAssessmentTools(t)
	broken := strings.Replace(aGoodBrief, `  - component: "the runtime"`,
		"  - name: node", 1)
	if broken == aGoodBrief {
		t.Fatal("the mutation did not apply")
	}
	out := readBrief(t, broken)
	if assessmentBool(t, out, "ok") {
		t.Fatal("a target entry without a component was accepted — it disappears from the programme")
	}
	if !strings.Contains(assessmentString(t, out, "reason"), "component") {
		t.Errorf("the refusal does not name the missing component: %s", assessmentString(t, out, "reason"))
	}
}
