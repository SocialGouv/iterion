//go:build live

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_Feat_Schedule exercises the host-crontab scheduling trigger end
// to end, in-process and fully isolated: `schedule add` writes a temp
// manifest (ITERION_SCHEDULES_FILE / --manifest), then `schedule run`
// launches the scheduled bot via the same RunRun path cron would use. The
// scheduled bot is a trivial cheap claw agent; the test asserts a run
// landed in the isolated store and finished — proving the schedule→run
// plumbing actually launches a real workflow.
//
// Requires: OpenAI (the scheduled bot is claw openai/gpt-5.5). Expected:
// ~2-5 min.
func TestLive_Feat_Schedule(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-schedule-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	manifest := filepath.Join(workspaceDir, "schedules.yaml")
	storeDir := filepath.Join(workspaceDir, ".iterion")
	botPath, err := filepath.Abs(filepath.Join("testdata", "feat_sched.bot"))
	if err != nil {
		t.Fatalf("abs bot path: %v", err)
	}

	p := cli.NewPrinter(cli.OutputHuman)
	if err := cli.RunScheduleAdd(p, cli.ScheduleAddOptions{
		ScheduleCommonOptions: cli.ScheduleCommonOptions{ManifestPath: manifest},
		Name:                  "live-sched",
		Cron:                  "0 0 * * *",
		Bot:                   botPath,
		Workdir:               workspaceDir,
		StoreDir:              storeDir,
		Timeout:               "5m",
	}); err != nil {
		t.Fatalf("schedule add: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := cli.RunScheduleRun(ctx, p, cli.ScheduleRunOptions{
		ScheduleCommonOptions: cli.ScheduleCommonOptions{ManifestPath: manifest},
		Name:                  "live-sched",
	}); err != nil {
		t.Fatalf("schedule run: %v", err)
	}

	// The scheduled run must have landed in the isolated store + finished.
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New(%s): %v", storeDir, err)
	}
	runs, err := s.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) == 0 {
		t.Fatalf("schedule run produced no run in the isolated store %s", storeDir)
	}
	finished := false
	for _, id := range runs {
		if r, _ := s.LoadRun(context.Background(), id); r != nil && r.Status == store.RunStatusFinished {
			finished = true
		}
	}
	if !finished {
		t.Errorf("expected the scheduled bot run to finish; runs=%v", runs)
	} else {
		t.Logf("schedule trigger launched + finished a bot run in-process (%d run(s))", len(runs))
	}
}
