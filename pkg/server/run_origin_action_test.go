package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// The action probe: the store accesses of a request, as the store sees them
// ---------------------------------------------------------------------------

// storeCall is one store access the probe recorded: the method, the run it
// addressed, whether its context carried a run data binding, and whether it
// came after the handler returned.
type storeCall struct {
	method, runID string
	bound, after  bool
}

// actionProbe records the accesses of the store methods a test double
// overrides (gateStore here, gateMongoStore on Mongo), the writes that
// landed, and can fail one named method once. It sees the overridden
// methods only: a store method no double overrides is not recorded.
type actionProbe struct {
	mu     sync.Mutex
	calls  []storeCall
	landed []string
	fail   map[string]error
	after  atomic.Bool
}

func (p *actionProbe) observe(ctx context.Context, method, id string) error {
	_, bound := store.RunDataBindingFrom(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, storeCall{method: method, runID: id, bound: bound, after: p.after.Load()})
	if err, ok := p.fail[method]; ok {
		delete(p.fail, method)
		return err
	}
	return nil
}

// land records a write that changed the store.
func (p *actionProbe) land(method string, err error, changed bool) {
	if err != nil || !changed {
		return
	}
	p.mu.Lock()
	p.landed = append(p.landed, method)
	p.mu.Unlock()
}

func (p *actionProbe) reset() {
	p.mu.Lock()
	p.calls, p.landed, p.fail = nil, nil, nil
	p.mu.Unlock()
	p.after.Store(false)
}

func (p *actionProbe) failOnce(method string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail == nil {
		p.fail = map[string]error{}
	}
	p.fail[method] = err
}

func (p *actionProbe) recorded() ([]storeCall, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]storeCall(nil), p.calls...), append([]string(nil), p.landed...)
}

// The probed methods of the filesystem gate store: every write a bound row
// makes, and the reads beside LoadRun an execution makes most.

func (s *gateStore) SaveRun(ctx context.Context, r *store.Run) error {
	if err := s.probe.observe(ctx, "SaveRun", r.ID); err != nil {
		return err
	}
	err := s.FilesystemRunStore.SaveRun(ctx, r)
	s.probe.land("SaveRun", err, true)
	return err
}

func (s *gateStore) DeleteRun(ctx context.Context, id string) error {
	if err := s.probe.observe(ctx, "DeleteRun", id); err != nil {
		return err
	}
	err := s.FilesystemRunStore.DeleteRun(ctx, id)
	s.probe.land("DeleteRun", err, true)
	return err
}

func (s *gateStore) ClaimMerge(ctx context.Context, id string, staleBefore time.Time, expect store.ExpectedBirth) (bool, store.MergeStatus, time.Time, error) {
	if err := s.probe.observe(ctx, "ClaimMerge", id); err != nil {
		return false, "", time.Time{}, err
	}
	ok, prior, at, err := s.FilesystemRunStore.ClaimMerge(ctx, id, staleBefore, expect)
	s.probe.land("ClaimMerge", err, ok)
	return ok, prior, at, err
}

func (s *gateStore) UpdateRunOutcome(ctx context.Context, id string, status store.RunStatus, runErr string, meta store.RunOutcomeMeta, expectedFrom []store.RunStatus) (bool, error) {
	if err := s.probe.observe(ctx, "UpdateRunOutcome", id); err != nil {
		return false, err
	}
	changed, err := s.FilesystemRunStore.UpdateRunOutcome(ctx, id, status, runErr, meta, expectedFrom)
	s.probe.land("UpdateRunOutcome", err, changed)
	return changed, err
}

func (s *gateStore) UpdateRunStatus(ctx context.Context, id string, status store.RunStatus, runErr string) error {
	if err := s.probe.observe(ctx, "UpdateRunStatus", id); err != nil {
		return err
	}
	err := s.FilesystemRunStore.UpdateRunStatus(ctx, id, status, runErr)
	s.probe.land("UpdateRunStatus", err, true)
	return err
}

func (s *gateStore) AppendQueuedMessage(ctx context.Context, id string, msg store.QueuedUserMessage) error {
	if err := s.probe.observe(ctx, "AppendQueuedMessage", id); err != nil {
		return err
	}
	err := s.FilesystemRunStore.AppendQueuedMessage(ctx, id, msg)
	s.probe.land("AppendQueuedMessage", err, true)
	return err
}

func (s *gateStore) UpdateQueuedMessageStatus(ctx context.Context, runID, msgID string, status store.QueuedMessageStatus, expectedFrom ...store.QueuedMessageStatus) error {
	if err := s.probe.observe(ctx, "UpdateQueuedMessageStatus", runID); err != nil {
		return err
	}
	err := s.FilesystemRunStore.UpdateQueuedMessageStatus(ctx, runID, msgID, status, expectedFrom...)
	s.probe.land("UpdateQueuedMessageStatus", err, true)
	return err
}

func (s *gateStore) SetRunTags(ctx context.Context, runID string, tags []string) error {
	if err := s.probe.observe(ctx, "SetRunTags", runID); err != nil {
		return err
	}
	err := s.FilesystemRunStore.SetRunTags(ctx, runID, tags)
	s.probe.land("SetRunTags", err, true)
	return err
}

func (s *gateStore) AppendRunNote(ctx context.Context, runID string, note store.RunNote) (store.RunNote, error) {
	if err := s.probe.observe(ctx, "AppendRunNote", runID); err != nil {
		return store.RunNote{}, err
	}
	out, err := s.FilesystemRunStore.AppendRunNote(ctx, runID, note)
	s.probe.land("AppendRunNote", err, true)
	return out, err
}

func (s *gateStore) AddWatchedIssues(ctx context.Context, runID string, ids []string) ([]string, error) {
	if err := s.probe.observe(ctx, "AddWatchedIssues", runID); err != nil {
		return nil, err
	}
	out, err := s.FilesystemRunStore.AddWatchedIssues(ctx, runID, ids)
	s.probe.land("AddWatchedIssues", err, true)
	return out, err
}

func (s *gateStore) RemoveWatchedIssues(ctx context.Context, runID string, ids []string) ([]string, error) {
	if err := s.probe.observe(ctx, "RemoveWatchedIssues", runID); err != nil {
		return nil, err
	}
	out, err := s.FilesystemRunStore.RemoveWatchedIssues(ctx, runID, ids)
	s.probe.land("RemoveWatchedIssues", err, true)
	return out, err
}

func (s *gateStore) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	if err := s.probe.observe(ctx, "AppendEvent", runID); err != nil {
		return nil, err
	}
	out, err := s.FilesystemRunStore.AppendEvent(ctx, runID, evt)
	s.probe.land("AppendEvent", err, true)
	return out, err
}

func (s *gateStore) SaveCheckpoint(ctx context.Context, id string, cp *store.Checkpoint) error {
	if err := s.probe.observe(ctx, "SaveCheckpoint", id); err != nil {
		return err
	}
	err := s.FilesystemRunStore.SaveCheckpoint(ctx, id, cp)
	s.probe.land("SaveCheckpoint", err, true)
	return err
}

func (s *gateStore) LoadEventsRange(ctx context.Context, id string, from, to int64, limit int) ([]*store.Event, error) {
	if err := s.probe.observe(ctx, "LoadEventsRange", id); err != nil {
		return nil, err
	}
	return s.FilesystemRunStore.LoadEventsRange(ctx, id, from, to, limit)
}

// ---------------------------------------------------------------------------
// The rows: every HTTP route that acts on one run under carrier B or P
// ---------------------------------------------------------------------------

// actionNative is the native store a row seeds and replaces its run in,
// filesystem or Mongo.
type actionNative interface {
	store.RunStore
	CreateRunWithValueFormat(ctx context.Context, spec store.RunCreateSpec) (*store.Run, error)
}

// actionEnv is what lives beside the run store: the repository a merge row
// acts on, a run's work directory, a workspace file.
type actionEnv struct {
	repo string
	work string
	file string
}

// actionRow is one §0.2 row: its request, the run both incarnations are
// (fields and status, so the successor is as actionable as the original),
// and HEAD's status for the run it expects.
type actionRow struct {
	n      int
	name   string
	method string
	path   string
	body   string
	status store.RunStatus
	env    func(t *testing.T) actionEnv
	fields func(r *store.Run, env actionEnv)
	seeded func(ctx context.Context, st actionNative) error
	ok     int
}

const actionRunID = "r"

// actionRows lists carrier B (rows 1-16) and carrier P (17-19, 30) of plan
// §0.2 — every route that acts on one run whose handler binds its context.
func actionRows() []actionRow {
	finished, paused := store.RunStatusFinished, store.RunStatusPausedWaitingHuman
	return []actionRow{
		{n: 1, name: "merge", method: http.MethodPost, path: "/merge", body: `{"merge_strategy":"squash"}`, status: finished, env: mergeableActionRepo, fields: pendingMergeFields, ok: http.StatusOK},
		{n: 2, name: "commit-and-finalize", method: http.MethodPost, path: "/commit-and-finalize", body: `{"commit_message":"commit and finalize"}`, status: finished, env: dirtyActionWorktree, fields: dirtyWorktreeFields, ok: http.StatusOK},
		{n: 3, name: "conflicts-resolve", method: http.MethodPost, path: "/merge/conflicts/resolve", body: `{"path":"file.txt","content":"alpha\nresolved\ncharlie\n"}`, status: finished, env: conflictedActionRepo(false), fields: conflictedFields, ok: http.StatusOK},
		{n: 4, name: "resolve-with-agent", method: http.MethodPost, path: "/merge/conflicts/resolve-with-agent", body: `{}`, status: finished, env: conflictedActionRepo(false), fields: conflictedFields, ok: http.StatusInternalServerError},
		{n: 5, name: "conflicts-finalize", method: http.MethodPost, path: "/merge/conflicts/finalize", body: `{}`, status: finished, env: conflictedActionRepo(true), fields: conflictedFields, ok: http.StatusOK},
		{n: 6, name: "conflicts-abort", method: http.MethodPost, path: "/merge/conflicts/abort", status: finished, env: conflictedActionRepo(false), fields: conflictedFields, ok: http.StatusNoContent},
		{n: 7, name: "rename", method: http.MethodPost, path: "/rename", body: `{"name":"renamed"}`, status: finished, ok: http.StatusOK},
		{n: 8, name: "delete", method: http.MethodDelete, path: "", status: finished, ok: http.StatusNoContent},
		{n: 9, name: "tags", method: http.MethodPut, path: "/tags", body: `{"tags":["tagged"]}`, status: finished, ok: http.StatusOK},
		{n: 10, name: "notes", method: http.MethodPost, path: "/notes", body: `{"body":"a note"}`, status: finished, ok: http.StatusCreated},
		{n: 11, name: "watch-add", method: http.MethodPost, path: "/watch/i2", status: finished, ok: http.StatusOK},
		{n: 12, name: "watch-remove", method: http.MethodDelete, path: "/watch/i1", status: finished, fields: watchedFields, ok: http.StatusOK},
		{n: 13, name: "queue-message", method: http.MethodPost, path: "/queue-message", body: `{"text":"hello"}`, status: paused, ok: http.StatusCreated},
		{n: 14, name: "cancel-queued-message", method: http.MethodDelete, path: "/queue-message/m1", status: paused, seeded: queuedMessageSeed, ok: http.StatusNoContent},
		{n: 15, name: "files-content", method: http.MethodPut, path: "/files/content", body: `{"path":"w.txt","content":"new bytes\n"}`, status: finished, env: workspaceActionDir, fields: workspaceFields, ok: http.StatusOK},
		{n: 16, name: "cancel-inactive", method: http.MethodPost, path: "/cancel", status: paused, ok: http.StatusAccepted},
		{n: 17, name: "pause", method: http.MethodPost, path: "/pause", status: paused, ok: http.StatusConflict},
		{n: 18, name: "bump-loop", method: http.MethodPost, path: "/bump-loop", body: `{"loop_name":"l","delta":1}`, status: paused, ok: http.StatusConflict},
		{n: 19, name: "raise-budget", method: http.MethodPost, path: "/raise-budget", body: `{"budget":{"max_tokens":100}}`, status: paused, ok: http.StatusConflict},
		{n: 30, name: "browser-attach", method: http.MethodPost, path: "/browser/attach", status: paused, ok: http.StatusServiceUnavailable},
	}
}

func actionRowByName(t *testing.T, name string) actionRow {
	t.Helper()
	for _, row := range actionRows() {
		if row.name == name {
			return row
		}
	}
	t.Fatalf("no action row %q", name)
	return actionRow{}
}

func pendingMergeFields(r *store.Run, env actionEnv) {
	r.Worktree = true
	r.RepoRoot, r.WorkDir = env.repo, env.repo
	r.FinalBranch = "iterion/run/test"
	r.MergeStatus = store.MergeStatusPending
	r.MergeStrategy = store.MergeStrategySquash
}

func conflictedFields(r *store.Run, env actionEnv) {
	pendingMergeFields(r, env)
	r.MergeStatus = store.MergeStatusConflicted
	r.PendingMergeInto = "main"
	r.PendingMergeMessage = "squash the run"
}

func dirtyWorktreeFields(r *store.Run, env actionEnv) {
	r.Worktree = true
	r.RepoRoot, r.WorkDir = env.repo, env.work
}

func workspaceFields(r *store.Run, env actionEnv) {
	r.WorkDir = env.work
}

func watchedFields(r *store.Run, _ actionEnv) {
	r.WatchedIssueIDs = []string{"i1"}
}

func queuedMessageSeed(ctx context.Context, st actionNative) error {
	return st.AppendQueuedMessage(ctx, actionRunID, store.QueuedUserMessage{ID: "m1", Text: "queued"})
}

// mergeableActionRepo: main plus a storage branch that squashes cleanly.
func mergeableActionRepo(t *testing.T) actionEnv {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) { gittest.Run(t, dir, args...) }
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@t.t")
	git("config", "user.name", "t")
	git("config", "commit.gpgsign", "false")
	mustWS(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("alpha\n"), 0o644))
	git("add", "file.txt")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "iterion/run/test")
	mustWS(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("alpha\nbravo\n"), 0o644))
	git("commit", "-qam", "feat")
	git("checkout", "-q", "main")
	return actionEnv{repo: dir}
}

// conflictedActionRepo: the squash of the storage branch onto main stopped
// on a content conflict, as the merge leaves it; resolved stages the
// resolution, as the resolve step does.
func conflictedActionRepo(resolved bool) func(t *testing.T) actionEnv {
	return func(t *testing.T) actionEnv {
		t.Helper()
		dir := setupConflictingRepo(t)
		if _, err := gittest.Try(dir, "merge", "--squash", "iterion/run/test"); err == nil {
			t.Fatal("fixture: the squash merged cleanly, want a conflict")
		}
		if resolved {
			mustWS(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("alpha\nresolved\ncharlie\n"), 0o644))
			gittest.Run(t, dir, "add", "file.txt")
		}
		return actionEnv{repo: dir}
	}
}

// dirtyActionWorktree: a detached worktree of a repository with an
// uncommitted change, as a run that finished without committing leaves it.
func dirtyActionWorktree(t *testing.T) actionEnv {
	t.Helper()
	root := t.TempDir()
	repo, wt := filepath.Join(root, "repo"), filepath.Join(root, "wt")
	mustWS(t, os.MkdirAll(repo, 0o755))
	git := func(dir string, args ...string) string { return strings.TrimSpace(gittest.Run(t, dir, args...)) }
	git(repo, "init", "-q", "-b", "main")
	git(repo, "config", "commit.gpgsign", "false")
	git(repo, "config", "user.email", "t@t.t")
	git(repo, "config", "user.name", "t")
	mustWS(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("a\n"), 0o644))
	git(repo, "add", "f.txt")
	git(repo, "commit", "-qm", "base")
	git(repo, "worktree", "add", "-q", "--detach", wt, git(repo, "rev-parse", "HEAD"))
	mustWS(t, os.WriteFile(filepath.Join(wt, "f.txt"), []byte("a\nb\n"), 0o644))
	return actionEnv{repo: repo, work: wt}
}

// workspaceActionDir: a run work directory holding one file.
func workspaceActionDir(t *testing.T) actionEnv {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "w.txt")
	mustWS(t, os.WriteFile(file, []byte("old bytes\n"), 0o644))
	return actionEnv{work: dir, file: file}
}

// seedActionRun creates the row's run under actionRunID with marker as its
// workflow name and name, then gives it the row's fields and status.
func seedActionRun(ctx context.Context, st actionNative, row actionRow, env actionEnv, marker string) error {
	if _, err := st.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: actionRunID, WorkflowName: marker, Format: store.LegacyValueFormat}); err != nil {
		return err
	}
	r, err := st.LoadRun(ctx, actionRunID)
	if err != nil {
		return err
	}
	r.Name = marker
	if row.fields != nil {
		row.fields(r, env)
	}
	if env.repo != "" {
		if r.BaseCommit, err = gitOut(env.repo, "rev-parse", "main"); err != nil {
			return err
		}
		if r.FinalBranch != "" {
			if r.FinalCommit, err = gitOut(env.repo, "rev-parse", r.FinalBranch); err != nil {
				return err
			}
		}
	}
	r.Status = row.status
	if err := st.SaveRun(ctx, r); err != nil {
		return err
	}
	if row.seeded != nil {
		return row.seeded(ctx, st)
	}
	return nil
}

// replaceActionRun ends, deletes and prunes the run, then seeds its
// successor — the only way a new birth takes an existing ID.
func replaceActionRun(ctx context.Context, st actionNative, row actionRow, env actionEnv, marker string) error {
	r, err := st.LoadRun(ctx, actionRunID)
	if err != nil {
		return err
	}
	if !r.Status.IsTerminal() {
		if err := st.UpdateRunStatus(ctx, actionRunID, store.RunStatusFinished, ""); err != nil {
			return err
		}
	}
	if err := st.DeleteRun(ctx, actionRunID); err != nil {
		return err
	}
	if _, err := st.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		return err
	}
	return seedActionRun(ctx, st, row, env, marker)
}

// gitOut is one git command's trimmed output, its failure an error: the
// replacement hook runs on the server's goroutine, where a test cannot stop.
func gitOut(dir string, args ...string) (string, error) {
	out, err := gittest.Try(dir, args...)
	if err != nil {
		return "", fmt.Errorf("git %v in %s: %w", args, dir, err)
	}
	return strings.TrimSpace(out), nil
}

// noLLMCredential strips every credential an auxiliary model could resolve
// through, so the resolver row never reaches a live model.
func noLLMCredential(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "ZAI_API_KEY", "AWS_REGION",
		"GOOGLE_CLOUD_PROJECT", "AZURE_OPENAI_API_KEY",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "0")
}

// actionDo sends the row's request with birth in the expectation header
// ("" sends none).
func actionDo(t *testing.T, base string, row actionRow, birth string) gateReply {
	t.Helper()
	req, err := http.NewRequest(row.method, base+"/api/runs/"+actionRunID+row.path, strings.NewReader(row.body))
	mustWS(t, err)
	if row.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if birth != "" {
		req.Header.Set(expectedBirthHeader, birth)
	}
	resp, err := http.DefaultClient.Do(req)
	mustWS(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	mustWS(t, err)
	out := gateReply{status: resp.StatusCode, body: string(raw)}
	var env struct {
		Code  string `json:"error_code"`
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(out.body), &env)
	out.code, out.runID = env.Code, env.RunID
	return out
}

// treeDigest maps every file under root to its sha256 and every directory to
// "dir": a byte-for-byte picture of a run's directory.
func treeDigest(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			out[rel+"/"] = "dir"
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("digest %s: %w", root, err)
	}
	return out, nil
}

func mustTreeDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	out, err := treeDigest(root)
	mustWS(t, err)
	return out
}

func diffDigests(before, after map[string]string) []string {
	var out []string
	for k, v := range before {
		if after[k] != v {
			out = append(out, fmt.Sprintf("%s: %q → %q", k, v, after[k]))
		}
	}
	for k, v := range after {
		if _, ok := before[k]; !ok {
			out = append(out, fmt.Sprintf("%s: added %q", k, v))
		}
	}
	sort.Strings(out)
	return out
}

// repoDigest is what a git row could change in a repository: its HEAD, the
// index with its conflict stages, the work tree's status and the merge
// state files.
func repoDigest(dir string) (string, error) {
	var b strings.Builder
	for _, args := range [][]string{{"rev-parse", "HEAD"}, {"ls-files", "-s"}, {"status", "--porcelain"}} {
		out, err := gitOut(dir, args...)
		if err != nil {
			return "", err
		}
		b.WriteString(strings.Join(args, " ") + "\n" + out + "\n")
	}
	gitDir, err := gitOut(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	for _, name := range []string{"MERGE_HEAD", "SQUASH_MSG", "MERGE_MSG"} {
		raw, err := os.ReadFile(filepath.Join(gitDir, name))
		switch {
		case err == nil:
			b.WriteString(name + "\n" + string(raw) + "\n")
		case errors.Is(err, fs.ErrNotExist):
			b.WriteString(name + " absent\n")
		default:
			return "", fmt.Errorf("read %s: %w", name, err)
		}
	}
	return b.String(), nil
}

// envDigest pictures everything beside the store a row could touch: the
// repository, the run's work directory (a git worktree or plain files).
func envDigest(env actionEnv) (string, error) {
	var b strings.Builder
	if env.repo != "" {
		d, err := repoDigest(env.repo)
		if err != nil {
			return "", err
		}
		b.WriteString("repo\n" + d)
	}
	if env.work != "" && env.work != env.repo {
		if _, err := gitOut(env.work, "rev-parse", "--git-dir"); err == nil {
			d, err := repoDigest(env.work)
			if err != nil {
				return "", err
			}
			b.WriteString("work\n" + d)
		} else {
			d, err := treeDigest(env.work)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "work files %v\n", d)
		}
	}
	return b.String(), nil
}

// actionFixture is the production chain over the gate store with one run,
// born A; birth is A's.
type actionFixture struct {
	*gateFixture
	row   actionRow
	env   actionEnv
	birth string
}

func newActionFixture(t *testing.T, row actionRow) *actionFixture {
	t.Helper()
	noLLMCredential(t)
	fx := newGateFixture(t, true)
	env := actionEnv{}
	if row.env != nil {
		env = row.env(t)
	}
	mustWS(t, seedActionRun(t.Context(), fx.native, row, env, "A"))
	return &actionFixture{gateFixture: fx, row: row, env: env, birth: birthOf(t, fx.native, actionRunID)}
}

// armReplacement replaces A by B right after the nth run load from now and
// pictures B's run directory and the environment at the end of the hook.
type replacementPicture struct {
	mu    sync.Mutex
	run   map[string]string
	env   string
	birth string
}

func (fx *actionFixture) armReplacement(t *testing.T, n int32) *replacementPicture {
	t.Helper()
	pic := &replacementPicture{}
	runDir := filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID)
	fx.store.afterLoadN(n, func() error {
		ctx := context.Background()
		if err := replaceActionRun(ctx, fx.native, fx.row, fx.env, "B"); err != nil {
			return err
		}
		b, err := fx.native.LoadRun(ctx, actionRunID)
		if err != nil {
			return err
		}
		run, err := treeDigest(runDir)
		if err != nil {
			return err
		}
		envPic, err := envDigest(fx.env)
		if err != nil {
			return err
		}
		pic.mu.Lock()
		pic.birth, pic.run, pic.env = b.BirthID, run, envPic
		pic.mu.Unlock()
		fx.store.probe.reset()
		return nil
	})
	return pic
}

// assertSuccessorUntouched is the effect half of a refusal: B's run
// directory and everything beside the store are as the hook left them, and
// no write landed.
func (fx *actionFixture) assertSuccessorUntouched(t *testing.T, pic *replacementPicture) {
	t.Helper()
	mustWS(t, fx.store.failure())
	pic.mu.Lock()
	defer pic.mu.Unlock()
	if pic.birth == "" || pic.birth == fx.birth {
		t.Fatalf("the replacement did not run: successor birth %q, original %q", pic.birth, fx.birth)
	}
	if d := diffDigests(pic.run, mustTreeDigest(t, filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID))); len(d) != 0 {
		t.Fatalf("the successor's run directory changed:\n%s", strings.Join(d, "\n"))
	}
	got, err := envDigest(fx.env)
	mustWS(t, err)
	if got != pic.env {
		t.Fatalf("the successor's repository or workspace changed:\nbefore:\n%s\nafter:\n%s", pic.env, got)
	}
	if _, landed := fx.store.probe.recorded(); len(landed) != 0 {
		t.Fatalf("writes landed on the successor: %v", landed)
	}
}

// assertBoundUntilRefused is the site half: after the replacement, every
// store access of the action carried the binding, and the only unbound one
// is the failure rule's confirm read.
func (fx *actionFixture) assertBoundUntilRefused(t *testing.T) {
	t.Helper()
	calls, _ := fx.store.probe.recorded()
	var unbound []storeCall
	bound := 0
	for _, c := range calls {
		if c.bound {
			bound++
			continue
		}
		unbound = append(unbound, c)
	}
	if bound == 0 || len(unbound) != 1 || unbound[0].method != "LoadRun" {
		t.Fatalf("after the replacement the action made %d bound accesses and these unbound ones %+v, want its accesses bound and one unbound confirm read", bound, unbound)
	}
}

// W2: every carrier-B and carrier-P route, admitted on A's birth, with A
// replaced by an actionable B right after the gate's read. The action is
// refused 409 run_origin_lost naming the run; B's run directory, repository
// and workspace are untouched and no write landed.
func TestBoundMutationsRefuseASuccessorAfterAdmission(t *testing.T) {
	for _, row := range actionRows() {
		t.Run(fmt.Sprintf("%02d-%s", row.n, row.name), func(t *testing.T) {
			fx := newActionFixture(t, row)
			pic := fx.armReplacement(t, 1)
			got := actionDo(t, fx.hs.URL, row, fx.birth)
			wantOriginLost(t, row.name, got, actionRunID)
			fx.assertSuccessorUntouched(t, pic)
			fx.assertBoundUntilRefused(t)
		})
	}
}

// W2 (later windows): the replacement lands after the action's first bound
// read, inside the service.
//   - cancel-inactive: after the branch's own load, before the flip. The
//     flip refuses the successor and the answer is the loss, never the 202
//     of a run left as it was.
//   - delete: another delete of A lands between the service's load and the
//     store's delete. The filesystem store refuses a delete bound to a
//     tombstone (ErrRunDeleted) or to an absent run (ErrRunNotFound): both
//     answer 409, and nothing is written.
func TestBoundMutationsRefuseInsideTheService(t *testing.T) {
	t.Run("16-cancel-inactive-before-the-flip", func(t *testing.T) {
		row := actionRowByName(t, "cancel-inactive")
		fx := newActionFixture(t, row)
		pic := fx.armReplacement(t, 3)
		got := actionDo(t, fx.hs.URL, row, fx.birth)
		wantOriginLost(t, row.name, got, actionRunID)
		fx.assertSuccessorUntouched(t, pic)
		b, err := fx.native.LoadRun(t.Context(), actionRunID)
		mustWS(t, err)
		if b.Status != store.RunStatusPausedWaitingHuman {
			t.Fatalf("the successor's status is %s, want it paused as seeded", b.Status)
		}
	})
	for _, reaped := range []bool{false, true} {
		t.Run(fmt.Sprintf("08-delete-raced/reaped=%t", reaped), func(t *testing.T) {
			row := actionRowByName(t, "delete")
			fx := newActionFixture(t, row)
			fx.store.afterLoadN(2, func() error {
				ctx := context.Background()
				if err := fx.native.DeleteRun(ctx, actionRunID); err != nil {
					return err
				}
				if reaped {
					if _, err := fx.native.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
						return err
					}
				}
				fx.store.probe.reset()
				return nil
			})
			got := actionDo(t, fx.hs.URL, row, fx.birth)
			mustWS(t, fx.store.failure())
			wantOriginLost(t, "raced delete", got, actionRunID)
			if _, landed := fx.store.probe.recorded(); len(landed) != 0 {
				t.Fatalf("a raced delete wrote: %v", landed)
			}
		})
	}
}

// W2b (F2): run_origin_lost answers a loss of the ADDRESSED run only.
//   - (i) an E row reading another run: an assistant watch whose assistant
//     run is deleted answers HEAD's 404, the target intact.
//   - (ii) a B row whose store fails with a not-found error while the
//     addressed run is intact answers the row's own mapping.
//   - (iii) an admitted resume with uploads whose run is replaced by a
//     successor the preflight refuses (not resumable): the loss, though the
//     preflight's own error says nothing of an origin.
func TestActionOriginLostOnlyForTheAddressedRun(t *testing.T) {
	t.Run("i-assistant-watch-missing-assistant", func(t *testing.T) {
		row := actionRow{n: 26, name: "assistant-watch", status: store.RunStatusFailed}
		fx := newActionFixture(t, row)
		ctx := t.Context()
		mustWS(t, seedWSOriginRun(ctx, fx.native, "asst", "assistant", store.LegacyValueFormat, store.RunStatusFinished, 0))
		mustWS(t, fx.native.DeleteRun(ctx, "asst"))
		row.method, row.path, row.body = http.MethodPost, "/assistant-watches", `{"assistant_run_id":"asst","mode":"diagnose"}`
		got := actionDo(t, fx.hs.URL, row, fx.birth)
		if got.status != http.StatusNotFound || got.code == wsErrRunOriginLost || !strings.Contains(got.body, "assistant run not found") {
			t.Fatalf("a missing assistant run answered %d %s, want HEAD's 404 and no run_origin_lost", got.status, got.body)
		}
	})
	for _, tc := range []struct {
		row, method string
		status      int
		code        string
	}{
		{"tags", "SetRunTags", http.StatusInternalServerError, ""},
		{"merge", "ClaimMerge", http.StatusConflict, ""},
		{"rename", "SaveRun", http.StatusNotFound, ""},
	} {
		t.Run("ii-"+tc.row+"-"+tc.method+"-not-found", func(t *testing.T) {
			row := actionRowByName(t, tc.row)
			fx := newActionFixture(t, row)
			fx.store.probe.failOnce(tc.method, fmt.Errorf("probe: %w", store.ErrRunNotFound))
			got := actionDo(t, fx.hs.URL, row, fx.birth)
			if got.status != tc.status || got.code != tc.code {
				t.Fatalf("%s failing ErrRunNotFound on an intact run answered %d %s, want HEAD's %d", tc.method, got.status, got.body, tc.status)
			}
			if birthOf(t, fx.native, actionRunID) != fx.birth {
				t.Fatal("fixture: the addressed run changed")
			}
		})
	}
	t.Run("iii-resume-preflight-refuses-the-successor", func(t *testing.T) {
		fx := newGateFixture(t, true)
		ctx := t.Context()
		seedResumableRun(t, fx, actionRunID, "A", store.RunStatusCancelled)
		expected := birthOf(t, fx.native, actionRunID)
		// The gate's load, then the handler's: the successor lands before
		// the preflight's.
		fx.store.afterLoadN(2, func() error {
			return replaceWSOriginRun(context.Background(), fx.native, actionRunID, "B", store.LegacyValueFormat, store.RunStatusFinished, 0)
		})
		row := actionRow{name: "resume", method: http.MethodPost, path: "/resume", body: `{"attachments":["upload-1"]}`}
		got := actionDo(t, fx.hs.URL, row, expected)
		mustWS(t, fx.store.failure())
		wantOriginLost(t, "resume refused at its preflight", got, actionRunID)
		b, err := fx.native.LoadRun(ctx, actionRunID)
		mustWS(t, err)
		if b.BirthID == expected || b.Status != store.RunStatusFinished {
			t.Fatalf("the successor changed: birth %q status %s", b.BirthID, b.Status)
		}
	})
}

// seedResumableRun persists a run an operator resume reaches in-process: a
// workflow file that compiles, its recorded hash, and a checkpoint.
func seedResumableRun(t *testing.T, fx *gateFixture, id, marker string, status store.RunStatus) {
	t.Helper()
	const source = "workflow resume_probe:\n  entry: done\n"
	path := filepath.Join(fx.srv.cfg.WorkDir, "resume_probe.bot")
	mustWS(t, os.WriteFile(path, []byte(source), 0o644))
	_, hash, err := runview.CompileWorkflowFromSource(path, source, "", "")
	mustWS(t, err)
	ctx := context.Background()
	mustWS(t, seedWSOriginRun(ctx, fx.native, id, marker, store.LegacyValueFormat, "", 0))
	r, err := fx.native.LoadRun(ctx, id)
	mustWS(t, err)
	r.FilePath, r.WorkflowSource, r.WorkflowHash = path, source, hash
	r.Status = status
	mustWS(t, fx.native.SaveRun(ctx, r))
	mustWS(t, fx.native.SaveCheckpoint(ctx, id, &store.Checkpoint{NodeID: "done"}))
}

// withAfterFlag serves h and marks the probe once the handler returned:
// every access recorded after it belongs to work that outlived the request.
func withAfterFlag(t *testing.T, fx *gateFixture) *httptest.Server {
	t.Helper()
	h := fx.srv.Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		fx.store.probe.after.Store(true)
	}))
	t.Cleanup(hs.Close)
	return hs
}

// W3: every row at the run's current birth answers HEAD's status; a bound
// row's accesses address the path's run only; no row leaves a bound access
// behind the response. The resume row runs its workflow in-process: the
// execution it starts never inherits the binding.
func TestBoundMutationsServeTheAdmittedRun(t *testing.T) {
	for _, row := range actionRows() {
		t.Run(fmt.Sprintf("%02d-%s", row.n, row.name), func(t *testing.T) {
			fx := newActionFixture(t, row)
			hs := withAfterFlag(t, fx.gateFixture)
			fx.store.probe.reset()
			got := actionDo(t, hs.URL, row, fx.birth)
			if got.status != row.ok {
				t.Fatalf("%s at the current birth: %d %s, want %d", row.name, got.status, got.body, row.ok)
			}
			calls, _ := fx.store.probe.recorded()
			for _, c := range calls {
				if c.runID != actionRunID {
					t.Fatalf("%s accessed run %q: %+v", row.name, c.runID, c)
				}
				if c.bound && c.after {
					t.Fatalf("%s left a bound access behind the response: %+v", row.name, c)
				}
			}
		})
	}
	t.Run("22-resume-in-process", func(t *testing.T) {
		fx := newGateFixture(t, true)
		seedResumableRun(t, fx, actionRunID, "A", store.RunStatusCancelled)
		birth := birthOf(t, fx.native, actionRunID)
		hs := withAfterFlag(t, fx)
		fx.store.probe.reset()
		got := actionDo(t, hs.URL, actionRow{method: http.MethodPost, path: "/resume", body: `{}`}, birth)
		if got.status != http.StatusAccepted {
			t.Fatalf("resume at the current birth: %d %s, want 202", got.status, got.body)
		}
		waitRunSettled(t, fx.native, actionRunID)
		calls, _ := fx.store.probe.recorded()
		after := 0
		for _, c := range calls {
			if !c.after {
				continue
			}
			after++
			if c.bound {
				t.Fatalf("the resumed execution made a bound access: %+v", c)
			}
		}
		if after == 0 {
			t.Fatal("fixture: the execution made no recorded access after the response")
		}
	})
}

// waitRunSettled waits until no in-process execution holds the run.
func waitRunSettled(t *testing.T, st store.RunStore, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r, err := st.LoadRun(context.Background(), id)
		mustWS(t, err)
		if r.Status != store.RunStatusRunning && r.Status != store.RunStatusQueued && r.Status != store.RunStatusCancelled {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the resumed run did not settle within 30s")
}

// W3b: at the current birth every probed store access of a bound row
// carries the binding — no service path of a B or P row drops it.
func TestBoundMutationsBindEveryStoreAccess(t *testing.T) {
	for _, row := range actionRows() {
		t.Run(fmt.Sprintf("%02d-%s", row.n, row.name), func(t *testing.T) {
			fx := newActionFixture(t, row)
			fx.store.probe.reset()
			if got := actionDo(t, fx.hs.URL, row, fx.birth); got.status != row.ok {
				t.Fatalf("%s at the current birth: %d %s, want %d", row.name, got.status, got.body, row.ok)
			}
			calls, _ := fx.store.probe.recorded()
			// The gate's own read comes first, and a row that fails ends
			// with the failure rule's confirm read: both are unbound by
			// design. Every access between them is the action's.
			if len(calls) == 0 || calls[0].method != "LoadRun" || calls[0].bound {
				t.Fatalf("%s: the gate's read is not the first access: %+v", row.name, calls)
			}
			calls = calls[1:]
			if n := len(calls); row.ok >= http.StatusBadRequest && n > 0 && calls[n-1].method == "LoadRun" && !calls[n-1].bound {
				calls = calls[:n-1]
			}
			if len(calls) == 0 {
				t.Fatalf("%s made no probed store access", row.name)
			}
			for _, c := range calls {
				if !c.bound {
					t.Fatalf("%s made an unbound access: %+v (all: %+v)", row.name, c, calls)
				}
			}
		})
	}
}

// W4: an unchecked request — no expectation, or one under
// ITERION_RUN_BIRTH_CHECK=0 — calls its service as HEAD does: HEAD's status
// and not one bound store access. A failure of an unchecked action reads
// nothing more (no confirm read).
func TestUncheckedMutationsStayUnbound(t *testing.T) {
	for _, off := range []bool{false, true} {
		for _, row := range actionRows() {
			t.Run(fmt.Sprintf("check-off=%t/%02d-%s", off, row.n, row.name), func(t *testing.T) {
				fx := newActionFixture(t, row)
				birth := ""
				if off {
					t.Setenv(runBirthCheckEnv, "0")
					birth = fx.birth
				}
				fx.store.probe.reset()
				if got := actionDo(t, fx.hs.URL, row, birth); got.status != row.ok {
					t.Fatalf("%s unchecked: %d %s, want %d", row.name, got.status, got.body, row.ok)
				}
				calls, _ := fx.store.probe.recorded()
				for _, c := range calls {
					if c.bound {
						t.Fatalf("%s unchecked made a bound access: %+v", row.name, c)
					}
				}
			})
		}
	}
	t.Run("failure-reads-nothing-more", func(t *testing.T) {
		fx := newGateFixture(t, true)
		before := fx.store.loads.Load()
		got := gateDo(t, fx.hs.URL, http.MethodPost, "/api/runs/absent/pause", "", "")
		if got.status != http.StatusNotFound {
			t.Fatalf("pause of an absent run: %d %s, want 404", got.status, got.body)
		}
		if n := fx.store.loads.Load() - before; n != 1 {
			t.Fatalf("an unchecked failure read the store %d times, want the handler's one load", n)
		}
	})
}

// nonEndorsingStore forwards every call with its context but does not
// endorse itself: the method its embedding promotes refuses any receiver
// other than the native store.
type nonEndorsingStore struct{ *store.FilesystemRunStore }

// W5: a request the gate admitted, served by a store that does not endorse
// a bound observation, is refused 503 scope_unavailable and does not act.
func TestBoundActionRefusesAStoreThatDropsItsContext(t *testing.T) {
	for _, name := range []string{"rename", "delete", "queue-message", "tags"} {
		t.Run(name, func(t *testing.T) {
			row := actionRowByName(t, name)
			fx := newActionFixture(t, row)
			fx.srv.runs.Stop(t.Context())
			fx.srv.runs = newTestRunviewService(t, fx.srv.cfg.StoreDir, runview.WithRecoveryPassive(), runview.WithStore(nonEndorsingStore{fx.native}))
			before := mustTreeDigest(t, filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID))
			got := actionDo(t, fx.hs.URL, row, fx.birth)
			if got.status != http.StatusServiceUnavailable || got.code != errCodeScopeUnavailable {
				t.Fatalf("%s over a non-endorsing store: %d %s, want 503 %s", name, got.status, got.body, errCodeScopeUnavailable)
			}
			if d := diffDigests(before, mustTreeDigest(t, filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID))); len(d) != 0 {
				t.Fatalf("%s acted though refused:\n%s", name, strings.Join(d, "\n"))
			}
		})
	}
}
