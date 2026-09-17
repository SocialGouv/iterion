package runview

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A run served by a STORED bot tier is refused by --auto unless the caller
// names where that bot's current source is.
//
// Both sides of the diff have to be the same artifact. For a team bot or a
// platform override, resolveWorkflowPath answers with the BAKED catalog twin —
// a fallback written so the studio's diagram view has something compilable to
// draw. Right for a picture, wrong for a rewind: a team bot exists precisely to
// differ from the baked one, so every declaration would read as changed, the
// pivot would land on the entry node, and Rewind would drop every downstream
// output and tombstone its artifacts — with AutoTargeted true.
//
// Before the recorded source reached cloud runs this was unreachable: they
// stopped at ErrRewindNoSourceRecorded. Recording it is what makes the refusal
// necessary.
func TestRewindAuto_RefusesAStoredBotWhoseCurrentSourceIsNotNamed(t *testing.T) {
	for _, tier := range []string{store.BotSourceTierTeam, store.BotSourceTierPlatform} {
		t.Run(tier, func(t *testing.T) {
			svc, _, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")
			st := svc.RunStore()
			ctx := context.Background()
			run, err := st.LoadRun(ctx, runID)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			run.BotSourceTier = tier
			if err := st.SaveRun(ctx, run); err != nil {
				t.Fatalf("save: %v", err)
			}
			if _, err := svc.Rewind(ctx, RewindSpec{RunID: runID, Auto: true}); !errors.Is(err, ErrRewindStoredBotSourceUnresolved) {
				t.Fatalf("err = %v, want ErrRewindStoredBotSourceUnresolved", err)
			}
		})
	}

	// A caller that HAS resolved the stored bot's current version says so
	// through SourcePath, and --auto proceeds — the refusal is about not
	// knowing where the source is, never about the tier itself.
	t.Run("a named source path proceeds", func(t *testing.T) {
		svc, botPath, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")
		st := svc.RunStore()
		ctx := context.Background()
		run, err := st.LoadRun(ctx, runID)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		run.BotSourceTier = store.BotSourceTierTeam
		if err := st.SaveRun(ctx, run); err != nil {
			t.Fatalf("save: %v", err)
		}
		editBot(t, botPath, "agent implement:\n  model: \"claude-opus-4-7\"", "agent implement:\n  model: \"claude-opus-5\"")

		result, err := svc.Rewind(ctx, RewindSpec{RunID: runID, Auto: true, SourcePath: botPath})
		if err != nil {
			t.Fatalf("Rewind with a named source path: %v", err)
		}
		if result.NodeID != "implement" || !result.AutoTargeted {
			t.Fatalf("pivot = %q (auto %v), want implement", result.NodeID, result.AutoTargeted)
		}
	})

	// And a tier that IS on this filesystem is untouched: a baked-catalog run
	// resolves to the real file, and a local run carries no tier at all.
	t.Run("baked and local runs are unaffected", func(t *testing.T) {
		for _, tier := range []string{store.BotSourceTierBaked, ""} {
			svc, botPath, runID := seedAutoRun(t, "verify", "survey", "plan", "implement", "verify")
			st := svc.RunStore()
			ctx := context.Background()
			run, err := st.LoadRun(ctx, runID)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			run.BotSourceTier = tier
			run.FilePath = filepath.Clean(botPath)
			if err := st.SaveRun(ctx, run); err != nil {
				t.Fatalf("save: %v", err)
			}
			editBot(t, botPath, "agent implement:\n  model: \"claude-opus-4-7\"", "agent implement:\n  model: \"claude-opus-5\"")
			result, err := svc.Rewind(ctx, RewindSpec{RunID: runID, Auto: true})
			if err != nil {
				t.Fatalf("tier %q: Rewind: %v", tier, err)
			}
			if result.NodeID != "implement" {
				t.Fatalf("tier %q: pivot = %q, want implement", tier, result.NodeID)
			}
		}
	})
}
