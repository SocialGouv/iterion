package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// In cloud the launcher and the runner are two deployments that move
// independently: a server following `:edge` walked five releases ahead of
// a digest-pinned runner fleet between two pod recreations, and the IR one
// compiled would not load on the other. The runs said nothing — the
// operator had to compare the healthz of two deployments to find out.
//
// The pair on the document is what makes it answerable from the run alone,
// and a WARN is what makes it findable in the log. Never a refusal: skew
// is normal for the whole length of every rolling deploy.
func TestRecordRunnerBuild_StampsTheExecutingBuildAndWarnsOnSkew(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	const id = "run-skew"
	if err := s.SaveRun(ctx, &store.Run{
		ID: id, TenantID: "team-1", OwnerID: "u1",
		Status:         store.RunStatusRunning,
		IterionVersion: "v3.111.0+2ecc75b1aaaa",
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	var log bytes.Buffer
	r := &Runner{cfg: Config{Store: s, Logger: iterlog.New(iterlog.LevelWarn, &log)}}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}
	launched, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}

	r.recordRunnerBuild(context.Background(), msg, launched)

	got, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun after: %v", err)
	}
	if got.RunnerVersion != appinfo.FullVersion() {
		t.Errorf("RunnerVersion = %q, want the executing build %q", got.RunnerVersion, appinfo.FullVersion())
	}
	if got.IterionVersion != "v3.111.0+2ecc75b1aaaa" {
		t.Errorf("IterionVersion = %q, want the launcher's build untouched — the PAIR is the signal", got.IterionVersion)
	}
	if got.Status != store.RunStatusRunning {
		t.Errorf("status = %q, want running — stamping a build must not end the run", got.Status)
	}
	out := log.String()
	if !strings.Contains(out, "v3.111.0+2ecc75b1aaaa") || !strings.Contains(out, appinfo.FullVersion()) {
		t.Errorf("log = %q, want a warning naming BOTH builds", out)
	}
}

// The same build on both sides is the overwhelmingly common case (every
// local run, every aligned fleet): stamp, say nothing.
func TestRecordRunnerBuild_SilentWhenTheBuildsAgree(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	const id = "run-aligned"
	if err := s.SaveRun(ctx, &store.Run{
		ID: id, TenantID: "team-1", OwnerID: "u1",
		Status:         store.RunStatusRunning,
		IterionVersion: appinfo.FullVersion(),
	}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	var log bytes.Buffer
	r := &Runner{cfg: Config{Store: s, Logger: iterlog.New(iterlog.LevelWarn, &log)}}
	launched, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}

	r.recordRunnerBuild(context.Background(), &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}, launched)

	if log.Len() != 0 {
		t.Errorf("log = %q, want silence when the two builds agree", log.String())
	}
	got, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun after: %v", err)
	}
	if got.RunnerVersion != appinfo.FullVersion() {
		t.Errorf("RunnerVersion = %q, want it stamped even when aligned", got.RunnerVersion)
	}
}

// A legacy row carries no launcher build. Unknown is not skew: stamp what
// we know and stay quiet, rather than warn on every run of an older fleet.
func TestRecordRunnerBuild_UnknownLauncherIsNotSkew(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	const id = "run-legacy"
	if err := s.SaveRun(ctx, &store.Run{ID: id, TenantID: "team-1", OwnerID: "u1", Status: store.RunStatusRunning}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	var log bytes.Buffer
	r := &Runner{cfg: Config{Store: s, Logger: iterlog.New(iterlog.LevelWarn, &log)}}
	launched, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}

	r.recordRunnerBuild(context.Background(), &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}, launched)

	if log.Len() != 0 {
		t.Errorf("log = %q, want silence for a run whose launcher build is unknown", log.String())
	}
}
