package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// presetBundleBot is a bundle whose main.bot spends `tok` at the command sink
// it grants, and whose preset body spends the same secret at the PROMPT sink
// it does not — the system prompt of the agent the run builds a model task
// for, which is where the body is appended: the compile is clean, and the
// defect lives in a file merged after compilation.
const presetBundleBot = `dsl: 2

secrets:
  tok:
    as: value
    sinks: [command]

schema out:
  ok: bool

agent plan:
  model: "test-model"
  output: out

tool kick:
  command: ` + "`echo {{secrets.tok}}`" + `
  output: out

workflow pb:
  worktree: none
  sandbox: none
  entry: plan
  plan -> kick
  kick -> done
`

// writePresetBundle lays presetBundleBot down as bots/pb — a manifest is what
// makes the directory a bundle (presets/ is no marker), so it is what makes
// its presets reach any surface at all — with the given preset page.
func writePresetBundle(t *testing.T, presetFile, presetBody string) (dir, mainBot string) {
	t.Helper()
	dir = filepath.Join("bots", "pb")
	if err := os.MkdirAll(filepath.Join(dir, "presets"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainBot = filepath.Join(dir, "main.bot")
	if err := os.WriteFile(mainBot, []byte(presetBundleBot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: pb\ndisplay_name: PB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "presets", presetFile), []byte(presetBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, mainBot
}

// `iterion validate` reads the bundle's presets/*.md and hands them to the
// lint, so a preset prompt body that reaches an ungranted sink is said on the
// authoring surface — with the page an operator opens. Without the wiring the
// bundle validates clean and only a run would carry the reference.
func TestRunValidate_HoldsABundlePresetBodyToTheSecretGrants(t *testing.T) {
	inTempWorkspace(t)
	_, mainBot := writePresetBundle(t, "sre.md", "---\nname: sre\n---\nDeploy with {{secrets.tok}} when asked.")

	jp, out := jsonPrinter()
	_ = RunValidate(mainBot, jp) // a bundle error makes the command exit non-zero
	s := out.String()
	for _, want := range []string{"C323", "presets/sre.md", `preset \"sre\"`, "reaches the prompt sink"} {
		if !strings.Contains(s, want) {
			t.Fatalf("the preset body was not held to the grants (%q absent):\n%s", want, s)
		}
	}

	// Granting the sink the body reaches clears it — the finding is the
	// grant's doing, not the preset's existence.
	if err := os.WriteFile(mainBot, []byte(strings.Replace(presetBundleBot, "sinks: [command]", "sinks: [command, prompt]", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	jp, out = jsonPrinter()
	if err := RunValidate(mainBot, jp); err != nil {
		t.Fatalf("validate with the prompt sink granted: %v\n%s", err, out.String())
	}
	if s := out.String(); strings.Contains(s, "C323") {
		t.Fatalf("C323 fires with the prompt sink granted:\n%s", s)
	}
}

// A preset the loader cannot read is absent from the lint's input, and a
// plain `iterion validate` — no dry run asked for — says so, naming the
// file: the verdict on the presets that loaded is never read as a verdict on
// all of them. A warning, not a refusal: `run` logs it and goes on, and the
// dry run, when asked for, is what refuses to run over it.
func TestRunValidate_SaysAPresetTheLoaderCouldNotRead(t *testing.T) {
	inTempWorkspace(t)
	_, mainBot := writePresetBundle(t, "sre.md", "---\nname: sre\nnot_a_preset_key: 1\n---\nDeploy with {{secrets.tok}} when asked.")

	jp, out := jsonPrinter()
	if err := RunValidate(mainBot, jp); err != nil {
		t.Fatalf("an unreadable preset fails validate: %v\n%s", err, out.String())
	}
	s := out.String()
	for _, want := range []string{"sre.md", "not_a_preset_key", "did not run"} {
		if !strings.Contains(s, want) {
			t.Fatalf("the preset the loader could not read went unsaid (%q absent):\n%s", want, s)
		}
	}
	if strings.Contains(s, "C323") {
		t.Fatalf("C323 fires on a body the loader never handed to the lint:\n%s", s)
	}
}

// The presets are held to the program a launch of the validated ENTRY merges
// them into. A sibling entry's manifest-bound checks read main.bot's program,
// but `iterion run bots/pb/extend.bot --preset sre` folds the bundle's presets
// into extend.bot's own workflow — so a secret extend.bot grants is no
// finding on extend.bot, and main.bot's withheld grant stays main.bot's.
func TestRunValidate_JudgesAPresetAgainstTheEntryARunMergesItInto(t *testing.T) {
	inTempWorkspace(t)
	dir, mainBot := writePresetBundle(t, "sre.md", "---\nname: sre\n---\nDeploy with {{secrets.tok}} when asked.")
	extendBot := filepath.Join(dir, "extend.bot")
	if err := os.WriteFile(extendBot, []byte(strings.Replace(presetBundleBot, "sinks: [command]", "sinks: [command, prompt]", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	jp, out := jsonPrinter()
	_ = RunValidate(extendBot, jp)
	if s := out.String(); strings.Contains(s, "C323") {
		t.Fatalf("validating extend.bot, which grants the prompt sink, draws C323 — the preset was judged against main.bot's grants:\n%s", s)
	}
	jp, out = jsonPrinter()
	_ = RunValidate(mainBot, jp)
	if s := out.String(); !strings.Contains(s, "C323") {
		t.Fatalf("validating main.bot, which withholds the prompt sink, draws no C323:\n%s", s)
	}
}

// Under a dry run (`--preset`, `--exec`), a preset the loader cannot read is
// ONE cause said ONCE: the bundle warning carries the file and the reason,
// and the dry run's refusal points at it by count instead of repeating it —
// a reader counting findings counts one defect, not two.
func TestRunValidate_SaysAnUnreadablePresetOnceUnderADryRun(t *testing.T) {
	inTempWorkspace(t)
	dir, mainBot := writePresetBundle(t, "sre.md", "---\nname: sre\n---\nDeploy carefully.")
	if err := os.WriteFile(filepath.Join(dir, "presets", "ops.md"), []byte("---\nname: ops\nnot_a_preset_key: 1\n---\nOperate."), 0o644); err != nil {
		t.Fatal(err)
	}

	jp, out := jsonPrinter()
	err := RunValidateWith(mainBot, jp, ValidateOptions{Preset: "sre"})
	s := out.String()
	if err == nil {
		t.Fatalf("the dry run ran over a preset the loader could not read:\n%s", s)
	}
	if got := strings.Count(s, "not_a_preset_key"); got != 1 {
		t.Fatalf("the unreadable preset's cause is said %d time(s), want once:\n%s", got, s)
	}
	if got := strings.Count(s, "ops.md"); got != 1 {
		t.Fatalf("the unreadable preset is named %d time(s), want once (the bundle warning):\n%s", got, s)
	}
	want := "bundle presets: 1 preset file the loader could not read"
	if !strings.Contains(s, want) {
		t.Fatalf("the dry run's refusal does not point at the warning by count (%q absent):\n%s", want, s)
	}
}
