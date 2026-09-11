package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/server/projects"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	delegationFailureContextVar = "failure_context"
	delegationInstructionsVar   = "delegation_instructions"
)

type delegatedFailureContext struct {
	Kind        string                `json:"kind"`
	SourceRun   *assistantResolvedRun `json:"source_run"`
	BotOrigin   *store.BotOrigin      `json:"bot_origin"`
	Fingerprint string                `json:"fingerprint"`
	Authority   string                `json:"authority"`
}

type delegationLaunchTarget struct {
	WorkDir            string
	WorktreeBaseCommit string
	RepoURL            string
	RepoRef            string
	ConnectionID       string
}

func projectForRepo(repoRoot string) (id string, known bool) {
	cfg, err := projects.Load()
	if err != nil {
		return "", false
	}
	repoRoot, _ = filepath.Abs(repoRoot)
	bestLen := -1
	for _, p := range cfg.RecentProjects {
		dir, err := filepath.Abs(p.Dir)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(dir, repoRoot)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && len(dir) > bestLen {
			id, known, bestLen = p.ID, true, len(dir)
		}
	}
	return id, known
}

func inferRunBotOrigin(run *store.Run) (*store.BotOrigin, error) {
	if run == nil {
		return nil, errors.New("missing run")
	}
	if run.BotOrigin != nil {
		origin := *run.BotOrigin
		return &origin, nil
	}
	if strings.TrimSpace(run.FilePath) == "" {
		return nil, errors.New("run predates bot provenance and has no workflow path")
	}
	p, err := gitlib.Describe(run.FilePath)
	if err != nil {
		return nil, fmt.Errorf("workflow source repository is unavailable: %w", err)
	}
	rel, err := filepath.Rel(p.RepoRoot, run.FilePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("workflow path is outside its repository")
	}
	projectID, known := projectForRepo(p.RepoRoot)
	if !known {
		return nil, errors.New("workflow source repository is not known to this host")
	}
	rel = filepath.ToSlash(rel)
	origin := &store.BotOrigin{
		Kind: "git", ProjectID: projectID, RepoRoot: p.RepoRoot, Commit: p.Commit,
		TreeHash: p.TreeHash, WorkflowPath: rel, Package: strings.Split(rel, "/")[0], Dirty: p.Dirty,
	}
	if b := runview.ResolveBundleFromFilePath(run.FilePath); b != nil && b.SourcePath != "" {
		if installed, ierr := botinstall.ReadOrigin(b.SourcePath); ierr == nil {
			origin.Kind = "installed_package"
			origin.InstallSource = installed.Source
			origin.InstallRef = installed.Ref
			origin.InstallPath = installed.SourcePath
			// A local install's copied .botz directory lives in the CONSUMER
			// repo. Repair must target the source repository recorded by the
			// sidecar, never that copy. Re-resolve local sources; URL sources
			// remain portable identities and require a cloud forge connection.
			if sourceProv, sourceErr := gitlib.Describe(installed.Source); sourceErr == nil {
				origin.RepoRoot, origin.Commit, origin.TreeHash, origin.Dirty = sourceProv.RepoRoot, sourceProv.Commit, sourceProv.TreeHash, sourceProv.Dirty
				origin.ProjectID, _ = projectForRepo(sourceProv.RepoRoot)
			} else if strings.Contains(installed.Source, "://") || strings.HasPrefix(installed.Source, "git@") {
				origin.RepoRoot = ""
				origin.RepoURL = installed.Source
				origin.Commit = installed.Ref
			}
		}
	}
	return origin, nil
}

func compileDelegatedWorker(filePath, source, bundleDir string) (*ir.Workflow, error) {
	var (
		wf  *ir.Workflow
		err error
	)
	if bundleDir != "" {
		b, openErr := bundle.OpenDir(bundleDir)
		if openErr != nil {
			return nil, openErr
		}
		wf, _, err = runview.CompileBundleWorkflow(filepath.Join(bundleDir, "main.bot"), b)
	} else if source != "" {
		wf, _, err = runview.CompileWorkflowFromSource(filePath, source)
	} else {
		wf, _, err = runview.CompileWorkflowWithHash(filePath)
	}
	if err != nil {
		return nil, err
	}
	if wf.Worktree != "auto" {
		return nil, errors.New("delegated worker must declare worktree: auto")
	}
	if _, ok := wf.Vars[delegationFailureContextVar]; !ok {
		return nil, fmt.Errorf("delegated worker must declare var %s", delegationFailureContextVar)
	}
	if _, ok := wf.Vars[delegationInstructionsVar]; !ok {
		return nil, fmt.Errorf("delegated worker must declare var %s", delegationInstructionsVar)
	}
	return wf, nil
}

func delegationRefName(fingerprint string) string {
	sum := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(sum[:12])
}

func (s *Server) prepareRunDelegation(ctx context.Context, sourceRunID, instructions string, vars map[string]string) (map[string]string, delegationLaunchTarget, string, *store.RunDelegation, error) {
	sourceRun, err := s.runs.LoadRunCtx(ctx, strings.TrimSpace(sourceRunID))
	if err != nil {
		return nil, delegationLaunchTarget{}, "", nil, fmt.Errorf("source run not found")
	}
	if sourceRun.Status != store.RunStatusFailed && sourceRun.Status != store.RunStatusFailedResumable {
		return nil, delegationLaunchTarget{}, "", nil, fmt.Errorf("source run must be failed, got %s", sourceRun.Status)
	}
	resolved, err := loadAssistantRun(ctx, sourceRun.ID, s.runs.RunStore())
	if err != nil {
		return nil, delegationLaunchTarget{}, "", nil, err
	}
	fingerprint := assistantFailureFingerprint(resolved)
	ids, err := s.runs.RunStore().ListRuns(ctx)
	if err != nil {
		return nil, delegationLaunchTarget{}, "", nil, err
	}
	attempt := 1
	for _, id := range ids {
		r, loadErr := s.runs.RunStore().LoadRun(ctx, id)
		if loadErr != nil || r.Delegation == nil || r.Delegation.SourceRunID != sourceRun.ID || r.Delegation.EpisodeFingerprint != fingerprint {
			continue
		}
		if !r.Status.IsTerminal() {
			return nil, delegationLaunchTarget{}, "", nil, fmt.Errorf("a delegated worker is already active for this failure episode: %s", r.ID)
		}
		if r.Delegation.Attempt >= attempt {
			attempt = r.Delegation.Attempt + 1
		}
	}
	origin, err := inferRunBotOrigin(sourceRun)
	if err != nil {
		return nil, delegationLaunchTarget{}, "", nil, err
	}
	target := delegationLaunchTarget{}
	if origin.RepoRoot != "" {
		baseCommit, tree, snapshotErr := gitlib.SnapshotWorkingTree(origin.RepoRoot, delegationRefName(sourceRun.ID+":"+fingerprint))
		if snapshotErr != nil {
			return nil, delegationLaunchTarget{}, "", nil, fmt.Errorf("snapshot source repository: %w", snapshotErr)
		}
		origin.Commit, origin.TreeHash = baseCommit, tree
		origin.Dirty = false
		target.WorkDir, target.WorktreeBaseCommit = origin.RepoRoot, baseCommit
	} else if origin.RepoURL != "" && origin.Commit != "" && origin.ConnectionID != "" {
		target.RepoURL, target.RepoRef, target.ConnectionID = origin.RepoURL, origin.Commit, origin.ConnectionID
	} else {
		return nil, delegationLaunchTarget{}, "", nil, errors.New("source repository is unavailable or lacks a forge connection")
	}
	publicOrigin := *origin
	publicOrigin.RepoRoot = ""
	publicOrigin.ConnectionID = ""
	envelope, err := json.Marshal(delegatedFailureContext{
		Kind: "run_failure", SourceRun: resolved, BotOrigin: &publicOrigin,
		Fingerprint: fingerprint, Authority: "iterion-host",
	})
	if err != nil {
		return nil, delegationLaunchTarget{}, "", nil, err
	}
	out := make(map[string]string, len(vars)+2)
	for k, v := range vars {
		if k == delegationFailureContextVar || k == delegationInstructionsVar {
			continue
		}
		out[k] = v
	}
	out[delegationFailureContextVar] = string(envelope)
	out[delegationInstructionsVar] = strings.TrimSpace(instructions)
	delegatedRunID := fmt.Sprintf("repair-%s-%d", delegationRefName(sourceRun.ID+":"+fingerprint), attempt)
	return out, target, delegatedRunID, &store.RunDelegation{
		SourceRunID: sourceRun.ID, Kind: "run_failure", EpisodeFingerprint: fingerprint, Attempt: attempt,
	}, nil
}
