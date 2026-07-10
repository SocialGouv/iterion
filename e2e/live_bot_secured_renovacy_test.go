//go:build live

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_SecuredRenovacy runs the secured-renovacy bundle against a
// real LLM, exercising the patch-fast-track end-to-end on a tiny seeded
// npm project. The bot is compiled via bundle.OpenDir so prompts/skills
// merge correctly.
//
// Requires:
//   - `claude` CLI in PATH (OAuth or ZAI_API_KEY in env).
//   - `docker` in PATH (the bot declares sandbox: image:).
//   - OPENAI_API_KEY for the GPT branches.
//
// Heavy test — Docker container startup, npm/yarn installs, real LLM
// reasoning. Expect 30 min – 2 h, $5–50. The bot itself caps at 12 h /
// $100; the test context wraps at 3 h.
func TestLive_SecuredRenovacy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireBinaryInPath(t, "docker")
	requireOpenAI(t)

	// Compile via bundle.OpenDir so manifest + prompts + skills are
	// wired into the workflow exactly as `iterion run secured-renovacy/`
	// would do.
	bDir, err := filepath.Abs("../bots/secured-renovacy")
	if err != nil {
		t.Fatalf("abs bundle dir: %v", err)
	}
	b, err := bundle.OpenDir(bDir)
	if err != nil {
		t.Fatalf("bundle.OpenDir: %v", err)
	}
	wf, _, err := runview.CompileBundleWorkflow(b.IterPath, b)
	if err != nil {
		t.Fatalf("CompileBundleWorkflow: %v", err)
	}

	workspaceDir, err := os.MkdirTemp("", "iterion-secured-renovacy-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)
	seedGitRepo(t, workspaceDir)
	// One patch-tier outdated dep is enough to exercise the fast-track.
	seedNpmProject(t, workspaceDir, map[string]string{
		"left-pad": "1.0.0",
	})
	runCmd(t, workspaceDir, "git", "add", "package.json", "index.js")
	runCmd(t, workspaceDir, "git", "commit", "-m", "chore: seed npm fixture")

	storeDir := resolveLiveStoreDir(t, workspaceDir)
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runID := "live-secured-renovacy"

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}
	executor := newLiveExecutor(t, wf, s, runID, workspaceDir)
	defer executor.Close()
	// Do NOT override workspace_dir here. The bot declares
	// `workspace_dir: "${PROJECT_DIR}"` and the engine remaps
	// ${PROJECT_DIR} to /workspace (the container bind-mount target)
	// once the sandbox is up. Passing the host tempdir path would
	// break that remap — every in-container shell tool would receive
	// a path that exists only on the host and fails with "no such
	// directory".
	executor.SetVars(map[string]any{
		"scope":                "patch",
		"max_packages_per_run": 1,
		"major_policy":         "skip",
		"fix_loop_default":     1,
		"update_scope":         "libraries",
	})

	// WithWorkDir is required for sandbox-backed workflows: the docker
	// driver bind-mounts engine.workDir → /workspace inside the
	// container. Without it, engine.workDir defaults to os.Getwd() — the
	// iterion repo root — so the container mounts the wrong tree and
	// the bot inspects iterion source instead of the seeded fixture.
	// Also affects worktree:auto's repo-root resolution.
	eng := runtime.New(wf, s, executor, runtime.WithWorkDir(workspaceDir))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	inputs := map[string]any{
		"user_prompt":          "",
		"scope":                "patch",
		"max_packages_per_run": 1,
		"major_policy":         "skip",
		"fix_loop_default":     1,
		"update_scope":         "libraries",
	}

	t.Log("Starting secured-renovacy live run…")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run finished in %s", elapsed.Round(time.Second))

	acceptable, reason := liveRunResultAcceptable(runErr)
	if !acceptable {
		t.Fatalf("unacceptable run error: %v", runErr)
	}
	t.Logf("Run result: %s", reason)

	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}

	// Probe whether ANY upgrade landed: count commits beyond the seed.
	cmd := exec.Command("git", "-C", workspaceDir, "rev-list", "--count", "HEAD")
	out, err := cmd.CombinedOutput()
	commitCount := strings.TrimSpace(string(out))
	if err != nil {
		t.Errorf("git rev-list failed: %v\n%s", err, out)
	} else {
		t.Logf("Commits in workspace: %s (≥3 expected: seed + npm-fixture + ≥1 upgrade)", commitCount)
	}

	writeLiveTestReport(t, runID, workspaceDir, storeDir, s, events)
	assessQualityRaw(t, "secured-renovacy", "Renovacy", "Upgrade an outdated npm dependency via the patch fast-track", runID, workspaceDir, storeDir, s, events, time.Since(start), reason, gitArtifactEvidence(t, workspaceDir))
}

// TestLive_SecuredRenovacy_Real runs secured-renovacy against the
// polyglot fixture under e2e/fixtures/renovacy-multi-stack (npm +
// python + go + rust, all pinning real CVE-flagged versions + the
// node-ipc 10.1.1 sabotage release for the anti-malware heuristic).
//
// Expected duration: 1-3h, $20-80. Heavy — docker required.
func TestLive_SecuredRenovacy_Real(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireBinaryInPath(t, "docker")
	requireOpenAI(t)

	bDir, err := filepath.Abs("../bots/secured-renovacy")
	if err != nil {
		t.Fatalf("abs bundle dir: %v", err)
	}
	b, err := bundle.OpenDir(bDir)
	if err != nil {
		t.Fatalf("bundle.OpenDir: %v", err)
	}
	wf, _, err := runview.CompileBundleWorkflow(b.IterPath, b)
	if err != nil {
		t.Fatalf("CompileBundleWorkflow: %v", err)
	}

	workspaceDir := seedFromFixture(t, "renovacy-multi-stack")

	storeDir := resolveLiveStoreDir(t, workspaceDir)
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runID := uniqueRunID("live-secured-renovacy-real")

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}
	// Tee logger to <storeDir>/runs/<runID>/run.log so the desktop
	// app's cross-store log pane sees live test output instead of
	// "No log captured." — see prepareLiveRunLog godoc.
	teeLogger := prepareLiveRunLog(t, storeDir, runID)
	executor := newLiveExecutor(t, wf, s, runID, workspaceDir, withLiveLogger(teeLogger))
	defer executor.Close()
	// Cap per-run to a handful of packages so the test is bounded.
	// `scope: "patch,minor,major"` lets the bot tackle any tier — the
	// node-ipc malware signal will only surface if `major` is allowed
	// (the upgrade target is node-ipc@11+ on a different protest
	// branch and 9.x is the last clean line).
	executor.SetVars(map[string]any{
		"scope":                "patch,minor,major",
		"max_packages_per_run": 6,
		"major_policy":         "attempt",
		"fix_loop_default":     2,
		"fix_loop_major":       3,
		"update_scope":         "libraries",
		"user_prompt":          "Multi-stack fixture with deliberately vulnerable pins. Pay extra attention to node-ipc — version 10.1.1 is the documented protestware/sabotage release; upgrade it past the compromised range.",
	})

	eng := runtime.New(wf, s, executor,
		runtime.WithWorkDir(workspaceDir),
		runtime.WithLogger(teeLogger),
		// Attach the bundle so the runtime mirrors `<bundle>/skills/*.md`
		// into `<workspaceDir>/.claude/skills/`. The bot's
		// `batch_upgrade_patches` agent reads
		// `package-manager-upgrades.md` from there; without this option
		// the agent runs blind on ecosystem conventions and stops on
		// missing toolchain (observed 2026-05-15 23:05).
		runtime.WithBundle(b),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	inputs := map[string]any{
		"user_prompt":          "Multi-stack fixture with deliberately vulnerable pins. Pay extra attention to node-ipc — version 10.1.1 is the documented protestware/sabotage release; upgrade it past the compromised range.",
		"scope":                "patch,minor,major",
		"max_packages_per_run": 6,
		"major_policy":         "attempt",
		"fix_loop_default":     2,
		"fix_loop_major":       3,
		"update_scope":         "libraries",
	}

	commitsBefore := workspaceCommitCount(t, workspaceDir)
	t.Log("Starting secured-renovacy (real polyglot fixture) live run…")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	t.Logf("Run finished in %s", time.Since(start).Round(time.Second))

	acceptable, reason := liveRunResultAcceptableReal(runErr)
	if !acceptable {
		captureSandboxDiagnostics(t, runID)
		t.Fatalf("unacceptable run error: %v", runErr)
	}
	t.Logf("Run result: %s", reason)

	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	requireWorkspaceCommitGrowth(t, workspaceDir, commitsBefore)
	cmd := exec.Command("git", "-C", workspaceDir, "log", "--oneline", "-30")
	out, _ := cmd.CombinedOutput()
	t.Logf("Commits in workspace:\n%s", string(out))

	writeLiveTestReport(t, runID, workspaceDir, storeDir, s, events)
	assessQualityRaw(t, "secured-renovacy", "Renovacy", "Upgrade polyglot CVE-flagged dependencies incl. node-ipc malware screen", runID, workspaceDir, storeDir, s, events, time.Since(start), reason, gitArtifactEvidence(t, workspaceDir))
}

// TestLive_SecuredRenovacy_Protestware exercises the bot's
// anti-malware heuristic with a single-package npm fixture pinning
// `node-ipc@10.1.1` — the documented peacenotwar sabotage release
// (GHSA-97m3-w2cp-4xx6). Where TestLive_SecuredRenovacy_Real runs
// the broad multi-stack pipeline (with a max-packages cap that lets
// node-ipc fall out of scope), THIS test forces the bot to land on
// node-ipc and run `security_audit` on it — `osv-scanner` (and any
// secondary auditor available in the sandbox) MUST surface the
// advisory.
//
// Expected duration: 5-15 min, ~$2-5.
//
// Assertion: at least one of the security_audit verdict's three
// indicator fields (safe=false, malware_signals non-empty, or cves
// containing a node-ipc-related advisory) MUST fire. Failure on all
// three means the heuristic is silently broken.
func TestLive_SecuredRenovacy_Protestware(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireBinaryInPath(t, "docker")
	requireOpenAI(t)

	bDir, err := filepath.Abs("../bots/secured-renovacy")
	if err != nil {
		t.Fatalf("abs bundle dir: %v", err)
	}
	b, err := bundle.OpenDir(bDir)
	if err != nil {
		t.Fatalf("bundle.OpenDir: %v", err)
	}
	wf, _, err := runview.CompileBundleWorkflow(b.IterPath, b)
	if err != nil {
		t.Fatalf("CompileBundleWorkflow: %v", err)
	}

	workspaceDir := seedFromFixture(t, "protestware-node-ipc")

	storeDir := resolveLiveStoreDir(t, workspaceDir)
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runID := uniqueRunID("live-secured-renovacy-protestware")

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}
	teeLogger := prepareLiveRunLog(t, storeDir, runID)
	executor := newLiveExecutor(t, wf, s, runID, workspaceDir, withLiveLogger(teeLogger))
	defer executor.Close()
	// `scope: "major"` skips the patch/minor fast-tracks entirely so
	// node-ipc (the lone major in the fixture) is the very first
	// thing select_candidate hands to security_audit. The 3-package
	// cap is a safety floor; we expect the bot to terminate after
	// node-ipc anyway.
	const userPrompt = "Single-package npm fixture pinning node-ipc@10.1.1, the documented peacenotwar / protestware sabotage release. The security_audit node MUST detect it via osv-scanner --lockfile=package-lock.json (advisory GHSA-97m3-w2cp-4xx6) and refuse the upgrade as malware-flagged."
	executor.SetVars(map[string]any{
		"scope":                "major",
		"max_packages_per_run": 3,
		"major_policy":         "attempt",
		"fix_loop_default":     1,
		"fix_loop_major":       1,
		"update_scope":         "libraries",
		"user_prompt":          userPrompt,
	})

	eng := runtime.New(wf, s, executor,
		runtime.WithWorkDir(workspaceDir),
		runtime.WithLogger(teeLogger),
		runtime.WithBundle(b),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	inputs := map[string]any{
		"user_prompt":          userPrompt,
		"scope":                "major",
		"max_packages_per_run": 3,
		"major_policy":         "attempt",
		"fix_loop_default":     1,
		"fix_loop_major":       1,
		"update_scope":         "libraries",
	}

	t.Log("Starting secured-renovacy (protestware fixture) live run…")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	t.Logf("Run finished in %s", time.Since(start).Round(time.Second))

	acceptable, reason := liveRunResultAcceptableReal(runErr)
	if !acceptable {
		captureSandboxDiagnostics(t, runID)
		t.Fatalf("unacceptable run error: %v", runErr)
	}
	t.Logf("Run result: %s", reason)

	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}

	// Assert the anti-malware heuristic fired. We read the verdict
	// from the node_finished event (events.jsonl) rather than the
	// `artifacts/security_audit/<v>.json` file: in this run shape
	// only detect_stack's artifact persisted to disk (suspected
	// runtime bug unrelated to the heuristic — the verdict payload
	// IS in event.Data["output"]). Walking events makes the test
	// resilient to that.
	var verdict map[string]any
	for _, ev := range events {
		if ev.Type != store.EventNodeFinished || ev.NodeID != "security_audit" {
			continue
		}
		if out, ok := ev.Data["output"].(map[string]any); ok {
			verdict = out
			break
		}
	}
	if verdict == nil {
		writeLiveTestReport(t, runID, workspaceDir, storeDir, s, events)
		t.Fatalf("security_audit node_finished event missing — bot did not reach the audit step")
	}
	safe, _ := verdict["safe"].(bool)
	malware, _ := verdict["malware_signals"].([]any)
	cves, _ := verdict["cves"].([]any)

	cveText := func() string {
		b := strings.Builder{}
		for _, v := range cves {
			if s, ok := v.(string); ok {
				if b.Len() > 0 {
					b.WriteString(",")
				}
				b.WriteString(s)
			}
		}
		return b.String()
	}()
	malwareText := func() string {
		b := strings.Builder{}
		for _, v := range malware {
			if s, ok := v.(string); ok {
				if b.Len() > 0 {
					b.WriteString("; ")
				}
				b.WriteString(s)
			}
		}
		return b.String()
	}()
	cveTextLower := strings.ToLower(cveText)
	mentionsNodeIPCAdvisory := strings.Contains(cveTextLower, "node-ipc") ||
		strings.Contains(cveTextLower, "ghsa-97m3-w2cp-4xx6") ||
		strings.Contains(cveTextLower, "peacenotwar") ||
		strings.Contains(strings.ToLower(malwareText), "node-ipc") ||
		strings.Contains(strings.ToLower(malwareText), "peacenotwar") ||
		strings.Contains(strings.ToLower(malwareText), "protestware")

	flagged := !safe || len(malware) > 0 || mentionsNodeIPCAdvisory
	t.Logf("security_audit verdict: safe=%v cves=%q malware_signals=%q", safe, cveText, malwareText)
	if !flagged {
		writeLiveTestReport(t, runID, workspaceDir, storeDir, s, events)
		t.Fatalf("anti-malware heuristic silent: security_audit returned safe=true with no malware_signals / no node-ipc cve for the peacenotwar protestware release — this is the very signal the bot exists to surface")
	}
	t.Logf("✅ Anti-malware heuristic fired (safe=%v, malware_signals=%d, mentions_advisory=%v)", safe, len(malware), mentionsNodeIPCAdvisory)

	writeLiveTestReport(t, runID, workspaceDir, storeDir, s, events)
	assessQualityRaw(t, "secured-renovacy", "Renovacy", "Screen node-ipc@10.1.1 protestware via the anti-malware heuristic", runID, workspaceDir, storeDir, s, events, time.Since(start), reason, gitArtifactEvidence(t, workspaceDir))
}
