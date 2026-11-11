package runview

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// launchLoggingWarnings drives Service.Launch on the bot at path under
// rows and returns the service log's warnings.
func launchLoggingWarnings(t *testing.T, bot, path, src string, rows []ModelOverrideEntry) (*stubLaunchPublisher, string, error) {
	t.Helper()
	var buf bytes.Buffer
	pub := &stubLaunchPublisher{}
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.New(iterlog.LevelWarn, &buf)), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, launchErr := svc.Launch(context.Background(), LaunchSpec{FilePath: path, Source: src, BotID: bot, ModelOverrides: rows})
	return pub, buf.String(), launchErr
}

// A launch's own row the grammar admits and that reaches nothing in the
// run's workflow is admitted and warned, naming the row: a node, a route and
// a supervisor typo alike; a row that reaches a site warns nothing.
func TestLaunchWarnsOnALaunchRowThatReachesNothing(t *testing.T) {
	rows := []ModelOverrideEntry{
		{Selector: "rowreach_workk", Model: "claude-opus-5"},
		{Selector: "work#rowreach_gml", Model: "glm-5.3"},
		{Selector: "supervisor:rowreach_pacr", Model: "anthropic/claude-sonnet-5"},
		{Selector: "wo*k", Effort: "high"},
	}
	pub, log, err := launchLoggingWarnings(t, "gwhint", "gwhint.bot", gatewayHintBot, rows)
	requireAdmitted(t, pub, err)
	for _, sel := range []string{"rowreach_workk", "work#rowreach_gml", "supervisor:rowreach_pacr"} {
		want := `the launch's own override row "` + sel + `" matches no node, fallbacks: route or supervisor of bot "gwhint" — the row pins nothing here (a typo, or a site the workflow does not declare)`
		if !strings.Contains(log, want) {
			t.Errorf("service log = %q, want %q", log, want)
		}
	}
	if strings.Contains(log, `override row "wo*k"`) {
		t.Errorf("service log = %q warns on a row that reaches the node", log)
	}
	// The warning logs once per process: the rule itself, read directly,
	// names no row that reaches a site.
	wf, _, err := compileRouteCrossingBot(t, gatewayHintBot)
	if err != nil {
		t.Fatal(err)
	}
	if got := unreachedLaunchRowWarnings(wf, []ModelOverrideEntry{{Selector: "work"}, {Selector: "*"}, {Selector: "agent"}, {Selector: "work#glm"}}, llmroute.BotIdentity{ID: "gwhint"}); len(got) != 0 {
		t.Errorf("unreachedLaunchRowWarnings = %q, want none for rows that reach a site", got)
	}
}

// A workflow with a subbot node folds every launch row into its children:
// a row that reaches nothing of the parent is named as one that pins a site
// a child declares, never as a typo. The child's own fold reads the root's
// rows and warns on none: a root row names the root's sites too.
func TestALaunchRowThatReachesOnlyASubbotChildIsNamedSo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "parent"), "main.bot", subbotFoldParent)
	writeFile(t, filepath.Join(dir, "child"), "main.bot", subbotFoldAgent)
	pub, log, err := launchLoggingWarnings(t, "parent", filepath.Join(dir, "parent", "main.bot"), subbotFoldParent, []ModelOverrideEntry{{Selector: "act", Model: "claude-opus-5"}})
	requireAdmitted(t, pub, err)
	if want := `the launch's own override row "act" matches no node, fallbacks: route or supervisor of bot "parent" — the row pins nothing here (it still folds into every subbot child, where it pins a site the child declares)`; !strings.Contains(log, want) {
		t.Errorf("service log = %q, want %q", log, want)
	}

	childWf, _, err := compileRouteCrossingBot(t, subbotFoldAgent)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	rows, err := FoldForSubbotChild(childWf, nil, []store.RunModelOverride{{Selector: "rowreach_rootnode", Model: "claude-opus-5"}}, llmroute.BotIdentity{ID: "child"}, "", iterlog.New(iterlog.LevelWarn, &buf))
	if err != nil || len(rows) != 1 {
		t.Fatalf("FoldForSubbotChild = %v, %v; want the root row folded", rows, err)
	}
	if strings.Contains(buf.String(), "launch's own override row") {
		t.Errorf("child fold log = %q, want no row warning", buf.String())
	}
}

// The preview shows the same warning, every time: it folds a launch of its
// own rows.
func TestThePreviewWarnsOnALaunchRowThatReachesNothing(t *testing.T) {
	srv, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(&stubPolicyFoldPublisher{}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	out, err := srv.PreviewCredentials(context.Background(), CredentialPreviewSpec{
		Launch: LaunchSpec{FilePath: "fold.bot", Source: foldBotSource, BotID: "fold", ModelOverrides: []ModelOverrideEntry{{Selector: "rowreach_preview_wrk", Model: "claude-opus-5"}}},
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	want := `routing: the launch's own override row "rowreach_preview_wrk" matches no node, fallbacks: route or supervisor of bot "fold" — the row pins nothing here (a typo, or a site the workflow does not declare)`
	if !slices.Contains(out.Warnings, want) {
		t.Errorf("preview warnings = %q, want %q", out.Warnings, want)
	}
}

// lockedWarnBuffer is a log sink the run goroutines write and the test
// reads, under one lock.
type lockedWarnBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedWarnBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedWarnBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The in-process launch warns the same way, before it builds its
// executor.
func TestAnInProcessLaunchWarnsOnALaunchRowThatReachesNothing(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	dir := t.TempDir()
	writeFile(t, dir, "main.bot", subbotFoldAgent)
	sink := &lockedWarnBuffer{}
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.New(iterlog.LevelWarn, sink)), WithWorkDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer stopService(t, svc)
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: filepath.Join(dir, "main.bot"), BotID: "inproc", ModelOverrides: []ModelOverrideEntry{{Selector: "rowreach_inproc_actt", Model: "claude-opus-5"}}})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	awaitRunCompletion(t, res.Done, "the in-process run did not finish")
	want := `the launch's own override row "rowreach_inproc_actt" matches no node, fallbacks: route or supervisor of bot "inproc" — the row pins nothing here (a typo, or a site the workflow does not declare)`
	if !strings.Contains(sink.String(), want) {
		t.Errorf("service log = %q, want %q", sink.String(), want)
	}
}
