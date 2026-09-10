package cloudpublisher

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pluginsource"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The launch-path half of #536, end to end through SubmitLaunch: a team whose
// registered source stopped parsing (the manifest broke after registration)
// still launches — the run is queued, the message carries no contribution
// from that source — and the source reads degraded with the parse error.

// recordingSourceStore is the minimum store the resolver needs: one enabled
// source, and a record of the health stamps it receives. The memory store
// cannot be used here because its Validate refuses a local git origin.
type recordingSourceStore struct {
	pluginsource.Store
	mu     sync.Mutex
	src    pluginsource.PluginSource
	marked []string
}

func (r *recordingSourceStore) ListEnabledByTenant(context.Context, string) ([]pluginsource.PluginSource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return []pluginsource.PluginSource{r.src}, nil
}

func (r *recordingSourceStore) MarkDegraded(_ context.Context, tenantID, id, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tenantID != r.src.TenantID || id != r.src.ID {
		return pluginsource.ErrNotFound
	}
	r.src.DegradedReason = reason
	r.marked = append(r.marked, reason)
	return nil
}

func (r *recordingSourceStore) ClearDegraded(_ context.Context, tenantID, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tenantID != r.src.TenantID || id != r.src.ID {
		return pluginsource.ErrNotFound
	}
	r.src.DegradedReason = ""
	return nil
}

func TestSubmitLaunch_BrokenPluginSourceIsSkippedNotFatal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	origin := t.TempDir()
	// NoAutoMaintenance: `git commit` below otherwise detaches a
	// `git maintenance run --auto` that writes under this origin's
	// `.git/objects` after the command returned — into a directory
	// t.TempDir()'s cleanup is about to remove.
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", gitlib.NoAutoMaintenance(args...)...)
		cmd.Dir = origin
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "--quiet", "-b", "main")
	if err := os.MkdirAll(filepath.Join(origin, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "skills", "deploy.md"), []byte("# deploy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The incident's manifest: an unquoted `: ` inside a value.
	if err := os.WriteFile(filepath.Join(origin, "plugin.yaml"),
		[]byte("name: deploy-onyxia\nversion: 0.1.0\ndescription: deploy: to onyxia\ncontributes:\n  skills:\n    - skills/deploy.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "broken manifest")
	git("tag", "v0.1.0")

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sources := &recordingSourceStore{src: pluginsource.PluginSource{
		ID: "ps-1", TenantID: "team-a", Name: "deploy-onyxia", GitURL: origin, Ref: "v0.1.0", Enabled: true,
	}}
	var published *queue.RunMessage
	p := &Publisher{
		store:  st,
		logger: iterlog.New(iterlog.LevelError, io.Discard),
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			published = msg
			return nil
		},
		pluginSources: &pluginsource.Resolver{Store: sources, Fetcher: &pluginsource.Fetcher{CacheDir: t.TempDir()}},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	if _, err := p.SubmitLaunch(ctx, "run-quarantine", runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}, &ir.Workflow{Name: "wf"}, "hash"); err != nil {
		t.Fatalf("one team's broken plugin source failed the launch: %v", err)
	}
	r, err := st.LoadRun(ctx, "run-quarantine")
	if err != nil || r.Status != store.RunStatusQueued {
		t.Fatalf("the run must be queued: %+v (%v)", r, err)
	}
	if published == nil {
		t.Fatal("no queue message was published")
	}
	if published.Contributions != nil {
		for _, f := range published.Contributions.Plugin {
			if f.Name == "deploy.md" {
				t.Fatalf("a contribution from the broken source rode the message: %+v", f)
			}
		}
	}
	if len(sources.marked) != 1 || !strings.Contains(sources.marked[0], "parse") {
		t.Fatalf("the source must be flagged degraded with the parse error, got %v", sources.marked)
	}
}
