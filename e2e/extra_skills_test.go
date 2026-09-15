package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// An operator can add a skill-library skill to a run the bot's author never
// declared (`iterion run --skill <name>`, or ITERION_SKILLS) — and the point
// of persisting it on the run record is that it SURVIVES A RESUME.
//
// That is not a nicety. A conversational bot pauses at a human node on every
// turn, and every operator message is a resume; a launch-only list would be
// gone by the second reply, on a run they launched with --skill precisely to
// get it. The unit tests cover the union and the roster — this covers the one
// thing only a real engine can show: pause, resume, still there.
func TestExtraSkillSurvivesAResume(t *testing.T) {
	wf := compileFixture(t, "rewind_human_mini.bot")

	ws := t.TempDir()
	storeDir := t.TempDir()
	writeExtraSkill(t, storeDir, "house-standard", "how this shop authors bots")

	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	exec := newScenarioExecutor()
	// The fixture routes to `fail` unless verify approves; make it approve so
	// the resume runs to a clean terminal and the assertion below is about the
	// skill, not about the fixture's verdict.
	exec.on("verify", func(map[string]any) (map[string]any, error) {
		return map[string]any{"value": "fine", "ok": true}, nil
	})
	const runID = "e2e-extra-skills"
	mirrored := filepath.Join(ws, ".claude", "skills", "house-standard", "SKILL.md")

	eng := runtime.New(wf, st, exec,
		runtime.WithLogger(iterlog.Nop()),
		runtime.WithWorkDir(ws),
		runtime.WithExtraSkills([]string{"house-standard"}, "flag"),
	)
	err = eng.Run(context.Background(), runID, nil)
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected the run to pause at the human gate, got %v", err)
	}

	if _, err := os.Stat(mirrored); err != nil {
		t.Fatalf("the operator's skill was not mirrored at launch: %v", err)
	}

	// Persisted on the run — this is what resume reads back, and the CLI /
	// runview resume paths both re-apply it from here.
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if len(r.ExtraSkills) != 1 || r.ExtraSkills[0] != "house-standard" {
		t.Fatalf("run.ExtraSkills = %v; a resume has nothing to re-apply from", r.ExtraSkills)
	}

	// Simulate what a cloud pod or a fresh CLI process faces: the workspace
	// mirror is gone. Only re-application can put it back — if resume
	// dropped the list, this file never returns and the agent silently
	// answers the rest of the conversation without the skill.
	if err := os.RemoveAll(filepath.Join(ws, ".claude")); err != nil {
		t.Fatalf("clear the mirror: %v", err)
	}

	resumed := runtime.New(wf, st, exec,
		runtime.WithLogger(iterlog.Nop()),
		runtime.WithWorkDir(ws),
		// Exactly what pkg/cli/resume.go and runview's resume path pass.
		runtime.WithExtraSkills(r.ExtraSkills, "resume"),
	)
	if err := resumed.Resume(context.Background(), runID, map[string]any{"value": "ok"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := os.Stat(mirrored); err != nil {
		t.Fatalf("the operator's skill did not survive the resume: %v", err)
	}
}

// The addition is recorded on the run's own event stream. Without it, a run
// carries knowledge its .bot does not mention and a bug report against that
// run is irreproducible — the failure mode the whole seam is meant to avoid.
func TestExtraSkillIsRecordedOnTheRun(t *testing.T) {
	wf := compileFixture(t, "rewind_human_mini.bot")
	ws := t.TempDir()
	storeDir := t.TempDir()
	writeExtraSkill(t, storeDir, "house-standard", "how this shop authors bots")

	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "e2e-extra-skills-event"
	eng := runtime.New(wf, st, newScenarioExecutor(),
		runtime.WithLogger(iterlog.Nop()),
		runtime.WithWorkDir(ws),
		runtime.WithExtraSkills([]string{"house-standard"}, "env"),
	)
	if err := eng.Run(context.Background(), runID, nil); !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected a pause, got %v", err)
	}

	events, err := st.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, e := range events {
		if e.Type != store.EventSkillsInjected {
			continue
		}
		if got, _ := e.Data["origin"].(string); got != "env" {
			t.Errorf("origin: got %q, want %q — the run must say WHERE the list came from", got, "env")
		}
		names, _ := e.Data["skills"].([]any)
		if len(names) != 1 || names[0] != "house-standard" {
			t.Errorf("skills: got %v", e.Data["skills"])
		}
		return
	}
	t.Fatal("no skills_injected event — the addition is invisible on the run")
}

// A run with no operator additions stays silent: an event on every run would
// be noise, and "nothing was added" is the ordinary case.
func TestNoEventWithoutExtraSkills(t *testing.T) {
	wf := compileFixture(t, "rewind_human_mini.bot")
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "e2e-extra-skills-none"
	eng := runtime.New(wf, st, newScenarioExecutor(),
		runtime.WithLogger(iterlog.Nop()),
		runtime.WithWorkDir(t.TempDir()),
	)
	if err := eng.Run(context.Background(), runID, nil); !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected a pause, got %v", err)
	}
	events, err := st.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, e := range events {
		if e.Type == store.EventSkillsInjected {
			t.Fatal("skills_injected fired on a run that added nothing")
		}
	}
}

func writeExtraSkill(t *testing.T, storeDir, name, description string) {
	t.Helper()
	dir := filepath.Join(storeDir, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
}
