package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Phase 0 is the campaign's own bootstrap: three child bots run before
// `preflight`, each SKIPPED on the artefact that makes it unnecessary and
// each verified in git afterwards. Every decision it takes is a tool node
// reading a file, so every decision is falsifiable in both directions —
// which is what this file does. A skip guard that only ever sees the
// artefact present proves nothing about the branch that launches the child,
// and a landed check that only ever sees a well-behaved child proves
// nothing about the refusal it exists for.

// phaseZeroOut is the union of the phase-0 node outputs the tests read.
type phaseZeroOut struct {
	Disabled      bool   `json:"disabled"`
	RunAssessment bool   `json:"run_assessment"`
	RunNet        bool   `json:"run_net"`
	HeadBefore    string `json:"head_before"`
	Mode          string `json:"mode"`
	CatalogPath   string `json:"catalog_path"`
	Notice        string `json:"notice"`
}

// runPhaseZeroNode renders one campaign node's python script — every
// {{…}} reference substituted, none left — and executes it. It returns the
// exit code, the parsed stdout and the stderr, because a refusal is judged
// on BOTH: the JSON carries the notice, the stderr carries the operator's
// copy of it, and a node that refuses silently on one channel is a node an
// operator reads as a success.
func runPhaseZeroNode(t *testing.T, node string, subs map[string]string, path string) (int, phaseZeroOut, string) {
	t.Helper()
	body := toolScript(t, "campaign/main.bot", node)
	for ref, val := range subs {
		body = strings.ReplaceAll(body, ref, val)
	}
	if i := strings.Index(body, "{{"); i >= 0 {
		end := i + 40
		if end > len(body) {
			end = len(body)
		}
		t.Fatalf("%s: unresolved template ref near %q", node, body[i:end])
	}
	scriptPath := filepath.Join(t.TempDir(), node+".py")
	if err := os.WriteFile(scriptPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", scriptPath)
	if path != "" {
		cmd.Env = append(os.Environ(), "PATH="+path)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s failed to execute: %v (stderr %q)", node, err, stderr.String())
		}
		exit = ee.ExitCode()
	}
	var out phaseZeroOut
	if err := json.Unmarshal([]byte(stdout.String()), &out); err != nil {
		t.Fatalf("%s output is not JSON: %v (stdout %q, stderr %q)", node, err, stdout.String(), stderr.String())
	}
	return exit, out, stderr.String()
}

// phaseZeroRepo is a throwaway repository with one committed baseline file:
// the tree every phase-0 node reads.
func phaseZeroRepo(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return gittest.Run(t, ws, args...)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.invalid")
	git("config", "user.name", "t")
	writeUnder(t, ws, "README.md", "baseline\n")
	git("add", "README.md")
	git("commit", "-qm", "baseline")
	return ws, git
}

func writeUnder(t *testing.T, ws, rel, body string) {
	t.Helper()
	full := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const phaseZeroPlan = `version: 1
oracle:
  dir: .golden-master
lots:
  - id: L1
    title: a lot
    status: todo
`

// TestCampaignPhaseZeroDecidesOnTheArtefact falsifies the first skip in both
// directions, and the two refusals that flank it. The contract present and
// PARSEABLE is the only thing that skips the assessment child; a file of the
// right name that does not read is not a programme, and the child that
// writes programmes is exactly who repairs it.
func TestCampaignPhaseZeroDecidesOnTheArtefact(t *testing.T) {
	requireModernizeTools(t)

	type fixture struct {
		name        string
		plan        string // "" = absent
		brief       string // "" = absent
		enabled     bool
		wantExit    int
		wantDisable bool
		wantRun     bool
		wantIn      string // substring of the notice (exit 0) or stderr (exit 1)
	}
	for _, f := range []fixture{
		{
			name: "contract present and parseable: assessment SKIPPED, and the notice names it",
			plan: phaseZeroPlan, brief: "goal: x\n", enabled: true,
			wantExit: 0, wantRun: false, wantIn: "assessment SKIPPED: the programme contract at .modernize/plan.yaml",
		},
		{
			name:  "no contract, a brief: assessment RUN, and the notice names the brief",
			brief: "goal: x\n", enabled: true,
			wantExit: 0, wantRun: true, wantIn: "assessment RUN: no programme contract at .modernize/plan.yaml; the brief at .modernize/brief.yaml",
		},
		{
			name: "contract present but UNPARSEABLE, a brief: assessment RUN, and the notice says why",
			plan: "lots: [\n  - id: L1\n", brief: "goal: x\n", enabled: true,
			wantExit: 0, wantRun: true, wantIn: "does not parse",
		},
		{
			name:     "no contract and NO brief: refuse, naming the brief a plan would have been guessed from",
			enabled:  true,
			wantExit: 1, wantIn: "no brief at .modernize/brief.yaml to derive one from",
		},
		{
			name: "contract unparseable and no brief: refuse too — the same missing input",
			plan: "lots: [\n  - id: L1\n", enabled: true,
			wantExit: 1, wantIn: "does not parse",
		},
		{
			name:     "switch off with NOTHING there: phase 0 is cut, and nothing is refused",
			enabled:  false,
			wantExit: 0, wantDisable: true, wantRun: false, wantIn: "phase 0 OFF (phase_zero: false)",
		},
		{
			name: "switch off with EVERYTHING there: the switch dominates the artefacts",
			plan: phaseZeroPlan, brief: "goal: x\n", enabled: false,
			wantExit: 0, wantDisable: true, wantRun: false, wantIn: "phase 0 OFF (phase_zero: false)",
		},
	} {
		f := f
		t.Run(f.name, func(t *testing.T) {
			ws, git := phaseZeroRepo(t)
			if f.plan != "" {
				writeUnder(t, ws, ".modernize/plan.yaml", f.plan)
			}
			if f.brief != "" {
				writeUnder(t, ws, ".modernize/brief.yaml", f.brief)
			}
			head := strings.TrimSpace(git("rev-parse", "HEAD"))

			exit, out, stderr := runPhaseZeroNode(t, "phase_zero", map[string]string{
				"{{vars.workspace_dir}}": strconv.Quote(ws),
				"{{vars.phase_zero}}":    strconv.FormatBool(f.enabled),
				"{{vars.plan_path}}":     strconv.Quote(".modernize/plan.yaml"),
				"{{vars.brief_path}}":    strconv.Quote(".modernize/brief.yaml"),
			}, "")

			if exit != f.wantExit {
				t.Fatalf("exit = %d, want %d (notice %q, stderr %q)", exit, f.wantExit, out.Notice, stderr)
			}
			if out.Disabled != f.wantDisable {
				t.Errorf("disabled = %v, want %v", out.Disabled, f.wantDisable)
			}
			if out.RunAssessment != f.wantRun {
				t.Errorf("run_assessment = %v, want %v (notice %q)", out.RunAssessment, f.wantRun, out.Notice)
			}
			// A refusal must reach BOTH channels: the run's structured
			// output and the operator's stderr.
			if !strings.Contains(out.Notice, f.wantIn) {
				t.Errorf("notice = %q, want it to contain %q", out.Notice, f.wantIn)
			}
			if f.wantExit != 0 && !strings.Contains(stderr, f.wantIn) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, f.wantIn)
			}
			// The base sha the landed checks measure against is captured
			// HERE, before any child runs — and is not captured at all when
			// the phase is cut, because nothing will be measured.
			switch {
			case f.wantExit == 0 && f.wantDisable && out.HeadBefore != "":
				t.Errorf("head_before = %q on a cut phase, want empty", out.HeadBefore)
			case f.wantExit == 0 && !f.wantDisable && out.HeadBefore != head:
				t.Errorf("head_before = %q, want HEAD %q", out.HeadBefore, head)
			}
		})
	}
}

// TestCampaignPhaseZeroRefusesWithoutYq: with a contract file present and no
// yq, whether that contract READS cannot be decided. Guessing either way
// costs something real — skipping the child that repairs an unparseable
// plan, or running one that overwrites a good one — so the node refuses,
// the way preflight already refuses for the same missing tool.
func TestCampaignPhaseZeroRefusesWithoutYq(t *testing.T) {
	requireModernizeTools(t)
	ws, _ := phaseZeroRepo(t)
	writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan)
	writeUnder(t, ws, ".modernize/brief.yaml", "goal: x\n")

	subs := map[string]string{
		"{{vars.workspace_dir}}": strconv.Quote(ws),
		// The engine renders a bool var as a JSON literal, not a Python one:
		// the fixture must carry the producer's exact spelling or the node's
		// own `true, false, null` prelude would go untested.
		"{{vars.phase_zero}}": "true",
		"{{vars.plan_path}}":  strconv.Quote(".modernize/plan.yaml"),
		"{{vars.brief_path}}": strconv.Quote(".modernize/brief.yaml"),
	}
	// git only: yq is gone, and so is the devbox profile fallback.
	exit, out, stderr := runPhaseZeroNode(t, "phase_zero", subs, restrictedPATH(t, "git", "python3"))
	if exit != 1 {
		t.Fatalf("exit = %d without yq, want 1 (notice %q)", exit, out.Notice)
	}
	if !strings.Contains(out.Notice, "yq is not on PATH") || !strings.Contains(stderr, "yq is not on PATH") {
		t.Errorf("the refusal must name yq on both channels: notice %q, stderr %q", out.Notice, stderr)
	}
	// And the same tree WITH yq skips cleanly — so the refusal above is the
	// missing tool, not the fixture.
	exit, out, _ = runPhaseZeroNode(t, "phase_zero", subs, "")
	if exit != 0 || out.RunAssessment {
		t.Fatalf("with yq back: exit = %d, run_assessment = %v; want 0/false (notice %q)", exit, out.RunAssessment, out.Notice)
	}
}

// TestCampaignPlanLandedReadsGitNotTheChild falsifies the first landed
// check. Campy already refuses to believe a lot that says it landed
// something; a phase-0 child gets exactly the same treatment.
func TestCampaignPlanLandedReadsGitNotTheChild(t *testing.T) {
	requireModernizeTools(t)

	run := func(t *testing.T, ws, before string) (int, phaseZeroOut, string) {
		t.Helper()
		return runPhaseZeroNode(t, "plan_landed", map[string]string{
			"{{vars.workspace_dir}}": strconv.Quote(ws),
			"{{vars.plan_path}}":     strconv.Quote(".modernize/plan.yaml"),
			"{{input.before}}":       strconv.Quote(before),
		}, "")
	}

	t.Run("the child committed the contract: accepted", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan)
		git("add", ".modernize/plan.yaml")
		git("commit", "-qm", "contract")
		exit, out, stderr := run(t, ws, before)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (notice %q, stderr %q)", exit, out.Notice, stderr)
		}
		if !strings.Contains(out.Notice, "assessment landed: .modernize/plan.yaml is committed") {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	t.Run("the child committed NOTHING: refused, HEAD is the measurement", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		exit, out, stderr := run(t, ws, before)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		for _, ch := range []string{out.Notice, stderr} {
			if !strings.Contains(ch, "it committed nothing") {
				t.Errorf("channel = %q, want it to say the child committed nothing", ch)
			}
		}
	})

	t.Run("HEAD moved but the contract is not in the commit: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		writeUnder(t, ws, "NOTES.md", "the child committed something else\n")
		git("add", "NOTES.md")
		git("commit", "-qm", "not the contract")
		exit, out, _ := run(t, ws, before)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "is not in that commit") {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	t.Run("the contract is written but never committed: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		writeUnder(t, ws, "NOTES.md", "a commit that is not the contract\n")
		git("add", "NOTES.md")
		git("commit", "-qm", "moved HEAD")
		writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan)
		exit, out, _ := run(t, ws, before)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "is not in that commit") {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	t.Run("committed then edited: refused, preflight gets a committed tree", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan)
		git("add", ".modernize/plan.yaml")
		git("commit", "-qm", "contract")
		writeUnder(t, ws, ".modernize/plan.yaml", phaseZeroPlan+"# edited after the commit\n")
		exit, out, _ := run(t, ws, before)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "uncommitted changes") {
			t.Errorf("notice = %q", out.Notice)
		}
	})
}

// TestCampaignNetGateDecidesOnTheVerifyScript falsifies the second skip in
// both directions, on the artefact preflight itself looks for — and on a
// non-default oracle_dir, so the decision is read from the var and not from
// a hard-coded `.golden-master`.
func TestCampaignNetGateDecidesOnTheVerifyScript(t *testing.T) {
	requireModernizeTools(t)

	for _, oracle := range []string{".golden-master", ".net"} {
		oracle := oracle
		t.Run("oracle_dir="+oracle, func(t *testing.T) {
			ws, git := phaseZeroRepo(t)
			head := strings.TrimSpace(git("rev-parse", "HEAD"))
			subs := map[string]string{
				"{{vars.workspace_dir}}": strconv.Quote(ws),
				"{{vars.oracle_dir}}":    strconv.Quote(oracle),
			}

			exit, out, stderr := runPhaseZeroNode(t, "net_gate", subs, "")
			if exit != 0 {
				t.Fatalf("exit = %d, want 0 (stderr %q)", exit, stderr)
			}
			if !out.RunNet {
				t.Errorf("run_net = false with no net at %s/verify-oracle.sh (notice %q)", oracle, out.Notice)
			}
			if !strings.Contains(out.Notice, "golden-master RUN: no behavioural net at "+filepath.Join(oracle, "verify-oracle.sh")) {
				t.Errorf("notice = %q", out.Notice)
			}
			if out.HeadBefore != head {
				t.Errorf("head_before = %q, want HEAD %q", out.HeadBefore, head)
			}

			// The one artefact that flips it — nothing else in the directory.
			writeUnder(t, ws, filepath.Join(oracle, "corpus.json"), "{}\n")
			if _, out, _ = runPhaseZeroNode(t, "net_gate", subs, ""); !out.RunNet {
				t.Errorf("a corpus without the entry point must not count as a net (notice %q)", out.Notice)
			}
			writeUnder(t, ws, filepath.Join(oracle, "verify-oracle.sh"), "#!/bin/sh\nexit 0\n")
			exit, out, _ = runPhaseZeroNode(t, "net_gate", subs, "")
			if exit != 0 || out.RunNet {
				t.Fatalf("exit = %d, run_net = %v with the net present; want 0/false (notice %q)", exit, out.RunNet, out.Notice)
			}
			if !strings.Contains(out.Notice, "golden-master SKIPPED: the net's entry point already exists at "+filepath.Join(oracle, "verify-oracle.sh")) {
				t.Errorf("notice = %q, want it to name the artefact that caused the skip", out.Notice)
			}
		})
	}
}

// TestCampaignNetLandedRequiresTheWholeNet falsifies the second landed
// check. The three files are one artefact: the entry point CI runs, the
// corpus it compares, and the feature inventory the docs child's gate reads
// next. Two of three is a partial net, and lots reported done against a
// partial net are lots reported done against nothing.
func TestCampaignNetLandedRequiresTheWholeNet(t *testing.T) {
	requireModernizeTools(t)

	const oracle = ".golden-master"
	run := func(t *testing.T, ws, before string) (int, phaseZeroOut, string) {
		t.Helper()
		return runPhaseZeroNode(t, "net_landed", map[string]string{
			"{{vars.workspace_dir}}": strconv.Quote(ws),
			"{{vars.oracle_dir}}":    strconv.Quote(oracle),
			"{{input.before}}":       strconv.Quote(before),
		}, "")
	}
	commitNet := func(t *testing.T, ws string, git func(...string) string, names ...string) {
		t.Helper()
		for _, n := range names {
			writeUnder(t, ws, filepath.Join(oracle, n), "x\n")
			git("add", filepath.Join(oracle, n))
		}
		git("commit", "-qm", "net")
	}
	whole := []string{"verify-oracle.sh", "corpus.json", "feature-coverage.json"}

	t.Run("the whole net committed: accepted", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		commitNet(t, ws, git, whole...)
		exit, out, stderr := run(t, ws, before)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (notice %q, stderr %q)", exit, out.Notice, stderr)
		}
		if !strings.Contains(out.Notice, "golden-master landed") {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	t.Run("the child committed NOTHING: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		exit, out, _ := run(t, ws, before)
		if exit != 1 || !strings.Contains(out.Notice, "it committed nothing") {
			t.Fatalf("exit = %d, notice = %q; want 1 and a named refusal", exit, out.Notice)
		}
	})

	// Each file alone is the one missing piece, so no single name carries
	// the whole check.
	for _, missing := range whole {
		missing := missing
		t.Run("without "+missing+": refused, and the refusal names it", func(t *testing.T) {
			ws, git := phaseZeroRepo(t)
			before := strings.TrimSpace(git("rev-parse", "HEAD"))
			var partial []string
			for _, n := range whole {
				if n != missing {
					partial = append(partial, n)
				}
			}
			commitNet(t, ws, git, partial...)
			exit, out, _ := run(t, ws, before)
			if exit != 1 {
				t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
			}
			if !strings.Contains(out.Notice, filepath.Join(oracle, missing)+" not committed") {
				t.Errorf("notice = %q, want it to name %s", out.Notice, missing)
			}
		})
	}

	t.Run("the whole net committed, then something left dirty: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		before := strings.TrimSpace(git("rev-parse", "HEAD"))
		commitNet(t, ws, git, whole...)
		writeUnder(t, ws, filepath.Join(oracle, "refs", "stray.txt"), "uncommitted\n")
		exit, out, _ := run(t, ws, before)
		if exit != 1 || !strings.Contains(out.Notice, "uncommitted changes") {
			t.Fatalf("exit = %d, notice = %q; want 1 and a dirt refusal", exit, out.Notice)
		}
	})
}

// TestCampaignDocsGateWritesTheCatalogOutOfTree falsifies the docs
// decision: the mode is read from whether the documentation directory
// already carries pages, and the generated catalog is written OUT of the
// workspace — a catalog written into the tree would be the uncommitted
// change preflight then refuses.
func TestCampaignDocsGateWritesTheCatalogOutOfTree(t *testing.T) {
	requireModernizeTools(t)

	const docsDir = "docs/client"
	scratch := filepath.Join(t.TempDir(), "scratch", "campaign")
	subsFor := func(ws, productID string) map[string]string {
		return map[string]string{
			"{{vars.workspace_dir}}":   strconv.Quote(ws),
			"{{vars.docs_dir}}":        strconv.Quote(docsDir),
			"{{vars.docs_product_id}}": strconv.Quote(productID),
			"{{vars.scratch_dir}}":     strconv.Quote(scratch),
		}
	}

	t.Run("no page yet: full, and the tree stays clean", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		exit, out, stderr := runPhaseZeroNode(t, "docs_gate", subsFor(ws, "product"), "")
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (stderr %q)", exit, stderr)
		}
		if out.Mode != "full" {
			t.Errorf("mode = %q with no page, want full (notice %q)", out.Mode, out.Notice)
		}
		if !filepath.IsAbs(out.CatalogPath) {
			t.Errorf("catalog_path = %q, want an absolute path the child can read as-is", out.CatalogPath)
		}
		if strings.HasPrefix(out.CatalogPath, ws+string(os.PathSeparator)) {
			t.Errorf("catalog_path = %q is inside the workspace %q — it would dirty the tree preflight judges", out.CatalogPath, ws)
		}
		if dirt := strings.TrimSpace(git("status", "--porcelain")); dirt != "" {
			t.Errorf("the workspace is dirty after docs_gate: %q", dirt)
		}

		// The catalog carries the frozen shape, read back through yq —
		// not through a string match on what the node wrote.
		//
		// This asserts the PRODUCER only. `repos[].path` is the docs
		// child's local-source form and its resolver does not read it yet
		// (it reads url / github_repo / gitlab_path, and records anything
		// else `degraded`); the key is being added on that bot's own
		// branch. So a green here is not a working integration — the merge
		// order is, and phase 0 ships off until it holds.
		raw, err := exec.Command("yq", "-o=json", out.CatalogPath).Output()
		if err != nil {
			t.Fatalf("the generated catalog does not parse: %v", err)
		}
		var got struct {
			ID   string `json:"id"`
			Docs struct {
				ProductDir string `json:"product_dir"`
			} `json:"docs"`
			Repos []struct {
				ID   string `json:"id"`
				Path string `json:"path"`
			} `json:"repos"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("catalog JSON: %v (%s)", err, raw)
		}
		if got.ID != "product" || got.Docs.ProductDir != docsDir {
			t.Errorf("catalog id/product_dir = %q/%q, want %q/%q", got.ID, got.Docs.ProductDir, "product", docsDir)
		}
		if len(got.Repos) != 1 || got.Repos[0].ID != "product" || got.Repos[0].Path != "." {
			t.Errorf("catalog repos = %+v, want exactly one local source {id: product, path: .}", got.Repos)
		}
	})

	t.Run("a page already there: incremental", func(t *testing.T) {
		ws, _ := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		exit, out, _ := runPhaseZeroNode(t, "docs_gate", subsFor(ws, "product"), "")
		if exit != 0 || out.Mode != "incremental" {
			t.Fatalf("exit = %d, mode = %q; want 0/incremental (notice %q)", exit, out.Mode, out.Notice)
		}
	})

	t.Run("a non-markdown file is not a page: still full", func(t *testing.T) {
		ws, _ := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, ".gitkeep"), "")
		_, out, _ := runPhaseZeroNode(t, "docs_gate", subsFor(ws, "product"), "")
		if out.Mode != "full" {
			t.Errorf("mode = %q, want full — only pages count (notice %q)", out.Mode, out.Notice)
		}
	})

	t.Run("no product id: refused rather than a catalog naming nothing", func(t *testing.T) {
		ws, _ := phaseZeroRepo(t)
		exit, out, stderr := runPhaseZeroNode(t, "docs_gate", subsFor(ws, "  "), "")
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		if !strings.Contains(out.Notice, "docs_product_id is empty") || !strings.Contains(stderr, "docs_product_id is empty") {
			t.Errorf("notice %q / stderr %q, want both to name the empty id", out.Notice, stderr)
		}
	})
}

// TestCampaignDocsLandedRequiresCommittedPages falsifies the third landed
// check. This child is the one that legitimately commits nothing — an
// incremental pass over documentation nothing changed — so HEAD is NOT the
// measurement here; the pages are.
func TestCampaignDocsLandedRequiresCommittedPages(t *testing.T) {
	requireModernizeTools(t)

	const docsDir = "docs/client"
	run := func(t *testing.T, ws string) (int, phaseZeroOut, string) {
		t.Helper()
		return runPhaseZeroNode(t, "docs_landed", map[string]string{
			"{{vars.workspace_dir}}": strconv.Quote(ws),
			"{{vars.docs_dir}}":      strconv.Quote(docsDir),
		}, "")
	}

	t.Run("pages committed, HEAD not moved by this child: accepted", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		git("add", filepath.Join(docsDir, "guide", "start.md"))
		git("commit", "-qm", "pages")
		exit, out, stderr := run(t, ws)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (notice %q, stderr %q)", exit, out.Notice, stderr)
		}
		if !strings.Contains(out.Notice, "product-docs landed: 1 page(s) committed under "+docsDir) {
			t.Errorf("notice = %q", out.Notice)
		}
	})

	t.Run("no page committed: refused, and the refusal names the branch case", func(t *testing.T) {
		ws, _ := phaseZeroRepo(t)
		exit, out, stderr := run(t, ws)
		if exit != 1 {
			t.Fatalf("exit = %d, want 1 (notice %q)", exit, out.Notice)
		}
		for _, ch := range []string{out.Notice, stderr} {
			if !strings.Contains(ch, "committed no page under "+docsDir) || !strings.Contains(ch, "onto a branch") {
				t.Errorf("channel = %q, want the refusal to name the directory and the branch case", ch)
			}
		}
	})

	t.Run("pages written but never committed: refused", func(t *testing.T) {
		ws, _ := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		exit, out, _ := run(t, ws)
		if exit != 1 || !strings.Contains(out.Notice, "committed no page under") {
			t.Fatalf("exit = %d, notice = %q; want 1 and a named refusal", exit, out.Notice)
		}
	})

	t.Run("one page committed, another left dirty: refused", func(t *testing.T) {
		ws, git := phaseZeroRepo(t)
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "start.md"), "# start\n")
		git("add", filepath.Join(docsDir, "guide", "start.md"))
		git("commit", "-qm", "pages")
		writeUnder(t, ws, filepath.Join(docsDir, "guide", "next.md"), "# next\n")
		exit, out, _ := run(t, ws)
		if exit != 1 || !strings.Contains(out.Notice, "uncommitted changes") {
			t.Fatalf("exit = %d, notice = %q; want 1 and a dirt refusal", exit, out.Notice)
		}
	})
}

// TestCampaignPhaseZeroTopology pins the graph phase 0 adds, against the
// FROZEN interface of the three children — the sources they live at and the
// vars they are handed. A `with:` key that drifts compiles clean and hands
// the child its own default instead, silently documenting or netting
// somewhere the supervisor never looks.
func TestCampaignPhaseZeroTopology(t *testing.T) {
	wf := compileBot(t, "campaign")
	if wf == nil {
		t.Fatal("campaign did not compile")
	}

	if wf.Entry != "phase_zero" {
		t.Errorf("entry = %q, want phase_zero — phase 0 runs BEFORE preflight", wf.Entry)
	}

	for _, want := range []struct {
		node, source string
		with         []string
	}{
		{"assessment", "../assessment/main.bot", []string{"brief_path", "plan_path", "workspace_dir"}},
		{"golden_master", "../golden-master/main.bot", []string{"oracle_dir", "workspace_dir"}},
		{"product_docs", "../product-docs/main.bot", []string{"catalog_path", "mode", "oracle_dir", "product_id", "workspace_dir"}},
	} {
		sb, ok := wf.Nodes[want.node].(*ir.SubbotNode)
		if !ok {
			t.Errorf("%s is %T, want a subbot node", want.node, wf.Nodes[want.node])
			continue
		}
		if sb.Source != want.source {
			t.Errorf("%s source = %q, want %q", want.node, sb.Source, want.source)
		}
		var keys []string
		for _, m := range sb.With {
			keys = append(keys, m.Key)
		}
		sortStrings(keys)
		if strings.Join(keys, ",") != strings.Join(want.with, ",") {
			t.Errorf("%s with: keys = %v, want %v", want.node, keys, want.with)
		}
		// No `output:`: the supervisor believes nothing a child says of
		// itself, and the landed checks measure git instead.
		if sb.OutputSchema != "" {
			t.Errorf("%s declares output: %q — progress is judged in git, never from the child's own report", want.node, sb.OutputSchema)
		}
		if sb.Isolated {
			t.Errorf("%s is isolated: the child must write into THIS checkout", want.node)
		}
	}

	// The order is not a preference: the docs child's gate reads the net's
	// inventory, so it cannot run before the net exists.
	for _, e := range []struct {
		from, to, cond string
		neg            bool
	}{
		{"phase_zero", "preflight", "disabled", false},
		{"phase_zero", "assessment", "run_assessment", false},
		{"phase_zero", "net_gate", "", false},
		{"assessment", "plan_landed", "", false},
		{"plan_landed", "net_gate", "", false},
		{"net_gate", "golden_master", "run_net", false},
		{"net_gate", "docs_gate", "", false},
		{"golden_master", "net_landed", "", false},
		{"net_landed", "docs_gate", "", false},
		{"docs_gate", "product_docs", "", false},
		{"product_docs", "docs_landed", "", false},
		{"docs_landed", "preflight", "", false},
		{"preflight", "run_lot", "", false},
	} {
		if !hasEdge(wf, e.from, e.to, e.cond, e.neg) {
			t.Errorf("missing edge %s -> %s (when %q, negated %v)", e.from, e.to, e.cond, e.neg)
		}
	}

	// The base sha the landed checks measure against travels on the edge,
	// captured by the graph before the child ran — never re-read after.
	for _, e := range []struct{ from, ref string }{
		{"assessment", "{{outputs.phase_zero.head_before}}"},
		{"golden_master", "{{outputs.net_gate.head_before}}"},
	} {
		if !edgeCarries(wf, e.from, "before", e.ref) {
			t.Errorf("the edge out of %s does not carry before: %s", e.from, e.ref)
		}
	}

	// Every new var has a default: a campaign that declared phase 0 without
	// one would refuse to launch rather than skip it.
	for _, name := range []string{"phase_zero", "brief_path", "oracle_dir", "docs_dir", "docs_product_id", "scratch_dir"} {
		v := wf.Vars[name]
		if v == nil {
			t.Errorf("var %q is not declared", name)
			continue
		}
		if !v.HasDefault {
			t.Errorf("var %q has no default", name)
		}
	}
	if wf.Vars["docs_dir"] != nil && wf.Vars["docs_dir"].Default != "docs/client" {
		t.Errorf("docs_dir default = %v, want docs/client", wf.Vars["docs_dir"].Default)
	}
}

// TestCampaignPreflightStillRefusesWhatItAlwaysRefused: phase 0 PRODUCES,
// preflight JUDGES. The node is untouched — same refusals, same position
// ahead of the worker — and phase 0 is wired around it, not through it.
func TestCampaignPreflightStillRefusesWhatItAlwaysRefused(t *testing.T) {
	wf := compileBot(t, "campaign")
	if wf == nil {
		t.Fatal("campaign did not compile")
	}
	if _, ok := wf.Nodes["preflight"].(*ir.ToolNode); !ok {
		t.Fatalf("preflight is %T, want a tool node", wf.Nodes["preflight"])
	}
	script := toolScript(t, "campaign/main.bot", "preflight")
	for _, sentinel := range []string{
		"no programme contract at %s",
		"write the plan first",
		"no behavioural oracle at %s",
		"build the net",
		"uncommitted change(s) at campaign start",
	} {
		if !strings.Contains(script, sentinel) {
			t.Errorf("preflight no longer carries the refusal %q — phase 0 must not soften the node that judges it", sentinel)
		}
	}
	// Nothing enters the worker except through preflight.
	for _, e := range wf.Edges {
		if e.To == "run_lot" && e.From != "preflight" && e.From != "loop_gate" {
			t.Errorf("edge %s -> run_lot bypasses preflight", e.From)
		}
	}
}

func hasEdge(wf *ir.Workflow, from, to, cond string, negated bool) bool {
	for _, e := range wf.Edges {
		if e.From == from && e.To == to && e.Condition == cond && e.Negated == negated && e.Expression == nil {
			return true
		}
	}
	return false
}

func edgeCarries(wf *ir.Workflow, from, key, raw string) bool {
	for _, e := range wf.Edges {
		if e.From != from {
			continue
		}
		for _, m := range e.With {
			if m.Key == key && m.Raw == raw {
				return true
			}
		}
	}
	return false
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
