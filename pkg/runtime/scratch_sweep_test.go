package runtime

// The end-of-run scratch sweep is a DELETION whose root must answer to the
// operator, not to whatever the live environment says at exit: a project
// `.env` can plant ITERION_HOME (it plants any exportable name), and a root
// that follows the plant is a root the repository chose —
// <X>/projects/<key>/scratch/* for an X of its picking, seven days untouched,
// nothing dot-prefixed. The mount plan refuses exactly that home
// (operatorsIterionHome); the sweep resolves its root the same way — on the
// home the OPERATOR chose (store.InheritedIterionDataDir) — and deletes
// nothing when no such home resolves.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/memory"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestTheScratchSweepDeletesOnlyUnderTheHomeTheOperatorChose(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)

	home := t.TempDir() // the operator's
	base := t.TempDir() // the workspace the run describes
	t.Setenv("ITERION_HOME", home)

	old := time.Now().Add(-30 * 24 * time.Hour)
	stale := filepath.Join(home, "projects", store.EncodeWorkDirKey(base), "scratch", "stale-run")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	// The honest sweep: the operator's home is the one that resolves, and
	// the stale entry goes.
	e := &Engine{logger: iterlog.Nop(), repoRoot: base}
	e.sweepScratchOnExit("sweep-witness")
	if _, err := os.Stat(stale); err == nil {
		t.Error("the sweep did not reclaim the stale entry under the operator's own home")
	}

	// The plant: a project .env set ITERION_HOME at sweep time. The live
	// resolution follows it — that is the raw material of the defect — and
	// the mount plan refuses exactly that home at the same moment. The
	// sweep must answer to the same operator the mount answers to.
	planted := t.TempDir()
	plantedStale := filepath.Join(planted, "projects", store.EncodeWorkDirKey(base), "scratch", "stale-run")
	if err := os.MkdirAll(plantedStale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(plantedStale, old, old); err != nil {
		t.Fatal(err)
	}
	envtrust.MarkPlanted("ITERION_HOME")
	t.Setenv("ITERION_HOME", planted)
	if live := memory.WorkspaceScratchDir(base); !hasPathPrefix(live, planted) {
		t.Fatalf("fixture: the live resolution did not follow the planted home (%s) — the witness would prove nothing", live)
	}
	if _, notMounted := operatorsIterionHome(); notMounted == "" {
		t.Fatalf("fixture: the mount plan accepted the planted home — the witness would prove nothing")
	}
	e.sweepScratchOnExit("sweep-witness-planted")
	if _, err := os.Stat(plantedStale); err != nil {
		t.Error("the sweep followed the planted ITERION_HOME and deleted under it — the deletion root was the repository's choice")
	}

	// And the closed answer: no operator-chosen home at all (the home tier
	// planted too) resolves no root, and a sweep with no root it can answer
	// for deletes nothing.
	envtrust.MarkPlanted(store.HomeEnvName())
	if got := sweepScratchRoot(base); got != "" {
		t.Errorf("sweepScratchRoot resolved %q with no operator home — the refusal must fail closed on \"\"", got)
	}
	if strings.HasPrefix(sweepScratchRoot(base), planted) {
		t.Error("the sweep root resolved under the planted home")
	}
}
