package delegate

import (
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// allowedFlag parses the value of --allowedTools the way disallowed() parses
// its twin: the FLAG, not the joined argv. A substring probe would be
// satisfied by `--disallowedTools Write` for any name the roster also holds.
func allowedFlag(argv string) []string {
	f := strings.Fields(argv)
	for i, a := range f {
		if a == "--allowedTools" && i+1 < len(f) {
			return strings.Split(f[i+1], ",")
		}
	}
	return nil
}

// schemaForTwoSpawns makes Execute run the structured-output pass as well, so
// every row below is asserted on BOTH spawns of one task.
var schemaForTwoSpawns = []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)

// bothFlagsDisjoint fails when one spawn names the same tool on
// --allowedTools and on --disallowedTools. Names are compared through
// claudeFoldedToolName, the key this backend itself subtracts on: `agent` and
// `Agent` are one tool, `enter_worktree` and `EnterWorktree` are one tool,
// and an invariant that only held for one spelling would be a spelling check,
// not a boundary.
//
// The underscore half of that fold is why this is asserted here and not on
// toolcatalog.CanonicalToolName alone: the shared table has no row for the
// worktree movers, so a `readonly:` node declaring `enter_worktree` put that
// name on --allowedTools while `EnterWorktree` sat on --disallowedTools.
// Measured: putting the table-only key back in claudeApprovableNames reddens
// 12 rows of the matrix below.
func bothFlagsDisjoint(t *testing.T, label string, spawns []string) {
	t.Helper()
	for i, argv := range spawns {
		allow := allowedFlag(argv)
		deny := disallowed(argv)
		canonDeny := make(map[string]string, len(deny))
		for _, name := range deny {
			canonDeny[claudeFoldedToolName(name)] = name
		}
		for _, name := range allow {
			if denied, ok := canonDeny[claudeFoldedToolName(name)]; ok {
				t.Errorf("%s: spawn #%d names %q on --allowedTools and %q on --disallowedTools — the withholding then rests on a CLI precedence nothing here executes (allowed=%v disallowed=%v)",
					label, i+1, name, denied, allow, deny)
			}
		}
	}
}

// TestNoDeclarationLandsOnBothToolFlags is the repo-wide invariant of #1673:
// for every declaration the DSL can carry, under every spawn-bound rule that
// withholds a name without consulting it, no spawn of a claude_code task
// names one tool on both flags.
//
// It drives the real ClaudeCodeBackend.Execute, so what it reads is the argv
// the CLI would receive, on both spawns — the withholding rules live in a
// helper each spawn calls, and an invariant asserted on the helper would not
// see a spawn that forgot to call it.
func TestNoDeclarationLandsOnBothToolFlags(t *testing.T) {
	// EVERY name a spawn-bound rule can withhold — headlessWithheldTools (6),
	// orchestrationTools (4) and the writing roster `readonly:` removes (4) —
	// plus the aliases the spelling table folds onto them and a few ordinary
	// declarations, so the matrix is not made only of the names it is about.
	declarations := [][]string{
		{"Workflow"}, {"Task"}, {"Agent"}, {"TaskOutput"}, {"Monitor"},
		{"Task", "read_file"}, {"agent"}, {"spawn_subagent"},
		{"ScheduleWakeup"}, {"CronCreate"}, {"CronDelete"}, {"CronList"},
		{"RemoteTrigger"}, {"read_file"}, {"bash", "read_file"},
		{"write", "read_file"}, {"edit"}, {"multiedit"}, {"notebookedit"},
		{"write_file", "apply_patch"},
		// The pair `readonly:` withholds whatever the declaration says, in
		// every spelling the backend's fold reads as one name: the only
		// input for which the flag's own half of the subtraction fires. The
		// snake_case rows are the DSL's own convention for a multi-word tool
		// and the ones the shared table does not fold — they are what the
		// underscore half of claudeFoldedToolName exists for.
		{"EnterWorktree"}, {"enterworktree", "read_file"},
		{"enter_worktree"}, {"exit_worktree", "read_file"}, {"Enter_Worktree"},
		{},
	}
	for _, knob := range []string{"", "1"} {
		t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", knob)
		for _, declared := range declarations {
			for _, readonly := range []bool{false, true} {
				label := "tools=" + strings.Join(declared, "+") + " knob=" + knob
				if readonly {
					label += " readonly"
				}
				bothFlagsDisjoint(t, label, spawnArgv(t, Task{
					NodeID:        "n",
					AllowedTools:  declared,
					ToolsDeclared: true,
					Readonly:      readonly,
					OutputSchema:  schemaForTwoSpawns,
				}))
			}
		}
	}

	// Ultracode keeps the orchestration surface the knob would remove, and
	// gating changes which list the formatting spawn carries: both decide
	// what --disallowedTools holds, so both are rows of the same invariant.
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "1")
	for _, declared := range [][]string{{"Agent"}, {"Workflow"}, {"read_file"}} {
		bothFlagsDisjoint(t, "ultracode tools="+strings.Join(declared, "+"), spawnArgv(t, Task{
			NodeID:        "n",
			AllowedTools:  declared,
			ToolsDeclared: true,
			Ultracode:     true,
			OutputSchema:  schemaForTwoSpawns,
		}))
		bothFlagsDisjoint(t, "gated tools="+strings.Join(declared, "+"), spawnArgv(t, Task{
			NodeID:        "n",
			Permission:    mustPolicy(t, permission.ModeDeny),
			AllowedTools:  declared,
			ToolsDeclared: true,
			OutputSchema:  schemaForTwoSpawns,
		}))
	}
}

// TestADeclaredWithheldToolIsNotApprovedEither is the same invariant read from
// the declaration's side: the name a spawn-bound rule removes is gone from
// the approval list, and the names beside it are untouched. Without the
// second half, dropping the whole declaration would pass.
func TestADeclaredWithheldToolIsNotApprovedEither(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "1")
	spawns := spawnArgv(t, Task{
		NodeID:        "n",
		AllowedTools:  []string{"Task", "read_file", "Workflow"},
		ToolsDeclared: true,
		OutputSchema:  schemaForTwoSpawns,
	})
	if len(spawns) < 2 {
		t.Fatalf("expected the main pass and the formatting pass, got %d spawn(s): %v", len(spawns), spawns)
	}
	for i, argv := range spawns {
		allow := allowedFlag(argv)
		for _, withheld := range []string{"Task", "Workflow"} {
			if slices.Contains(allow, withheld) {
				t.Errorf("spawn #%d auto-approves %q, a tool this spawn withholds (allowed=%v)", i+1, withheld, allow)
			}
		}
		if !slices.Contains(allow, "read_file") {
			t.Errorf("spawn #%d dropped read_file, which nothing withholds (allowed=%v)", i+1, allow)
		}
	}
}

// claudeWritingNatives is what `readonly: true` must remove from a
// claude_code spawn, spelled as the CLI spells it.
var claudeWritingNatives = []string{"Write", "Edit", "MultiEdit", "NotebookEdit"}

// claudeWritingDeclarations are the `tools:` spellings a node writes a file
// with — what `readonly: true` must keep off --allowedTools.
//
// DERIVED from the shared spelling table rather than typed: the vocabulary an
// author may write is every key the table folds onto a writing canonical, and
// a literal held 6 of the 14 while calling itself the DSL's vocabulary. The
// canonicals are toolcatalog.WritingCanonicals(), the one set the withholding
// itself reads, so a concept added there widens this assertion in the same
// edit. Sorted, because
// CanonicalRows is a map and a failure has to name the same spelling twice.
func claudeWritingDeclarations() []string {
	var out []string
	for spelling, canonical := range toolcatalog.CanonicalRows() {
		if slices.Contains(toolcatalog.WritingCanonicals(), canonical) {
			out = append(out, spelling)
		}
	}
	slices.Sort(out)
	return out
}

// TestTheWritingVocabularyIsReadFromTheTable floors the derivation above: it
// is the list every readonly row is asserted against, and a table read that
// stopped resolving would leave those rows approving nothing and asserting
// nothing. 12 is under the 14 spellings today, so a synonym may be retired
// without touching this line.
func TestTheWritingVocabularyIsReadFromTheTable(t *testing.T) {
	got := claudeWritingDeclarations()
	if len(got) < 12 {
		t.Fatalf("the spelling table folds %d name(s) onto %v, want at least 12: the table read broke, and every readonly row below now asserts over an empty vocabulary (%v)",
			len(got), toolcatalog.WritingCanonicals(), got)
	}
	// The DSL's own canonical spellings, pinned by name: a table that kept 12
	// rows while dropping the two the catalogue actually writes with would
	// clear the floor above and prove nothing about them.
	for _, want := range []string{"write_file", "file_edit", "apply_patch", "notebook_edit"} {
		if !slices.Contains(got, want) {
			t.Errorf("%q no longer folds onto a writing canonical (%v): it is the spelling the catalogue's own `tools:` lists use, so the readonly rows would stop asserting it", want, got)
		}
	}
}

// TestEverySpawnOfAReadonlyTaskWithholdsTheWritingTools is #1816's witness:
// `readonly: true` is read by this backend, on both spawns, and a readonly
// task does not reach the CLI with a file-writing tool — whatever its own
// `tools:` list says.
//
// Two rows bite. The UNDECLARED surface — omitting `tools:` means the full
// native roster on a CLI backend, which is where the flag used to mean
// nothing at all. And the DECLARED writer: the flag is the node's access
// intent, so it wins over a list that names a writer, the way it already does
// on codex (a read-only OS sandbox) and on pi (a readers-only tool list). The
// declaration is not dropped in silence — see
// TestAReadonlyDeclarationThatNamesAWriterLosesItAndIsToldSo.
func TestEverySpawnOfAReadonlyTaskWithholdsTheWritingTools(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")

	for _, tc := range []struct {
		name string
		task Task
	}{
		{"undeclared surface", Task{NodeID: "n", Readonly: true, OutputSchema: schemaForTwoSpawns}},
		{"undeclared surface, ultracode", Task{
			NodeID: "n", Readonly: true, Ultracode: true, OutputSchema: schemaForTwoSpawns,
		}},
		{"undeclared surface, gated", Task{
			NodeID: "n", Readonly: true, Permission: mustPolicy(t, permission.ModeDeny),
			OutputSchema: schemaForTwoSpawns,
		}},
		{"read-plus-shell declaration (unchanged by the flag: the declaration already withholds these)", Task{
			NodeID: "n", Readonly: true, AllowedTools: []string{"bash", "read_file"},
			ToolsDeclared: true, OutputSchema: schemaForTwoSpawns,
		}},
		{"declaration naming a writer (the flag wins over the list)", Task{
			NodeID: "n", Readonly: true, AllowedTools: []string{"bash", "read_file", "write_file"},
			ToolsDeclared: true, OutputSchema: schemaForTwoSpawns,
		}},
		{"declaration naming an editor, gated", Task{
			NodeID: "n", Readonly: true, AllowedTools: []string{"read_file", "apply_patch"},
			ToolsDeclared: true, Permission: mustPolicy(t, permission.ModeDeny),
			OutputSchema: schemaForTwoSpawns,
		}},
		// Every spelling the table folds onto a writing canonical, declared
		// at once: without this row the --allowedTools half of the assertion
		// is vacuous for the twelve spellings no other row names, and the
		// rule is the fold, not the six names an author happened to type.
		{"declaration naming every writing spelling the table folds", Task{
			NodeID: "n", Readonly: true,
			AllowedTools:  append([]string{"bash", "read_file"}, claudeWritingDeclarations()...),
			ToolsDeclared: true, OutputSchema: schemaForTwoSpawns,
		}},
	} {
		spawns := spawnArgv(t, tc.task)
		if len(spawns) < 2 {
			t.Fatalf("%s: expected two spawns, got %d: %v", tc.name, len(spawns), spawns)
		}
		for i, argv := range spawns {
			got := disallowed(argv)
			for _, writer := range claudeWritingNatives {
				if !slices.Contains(got, writer) {
					t.Errorf("%s: spawn #%d of a readonly task keeps %q (disallowed=%v)", tc.name, i+1, writer, got)
				}
			}
			allow := allowedFlag(argv)
			for _, declared := range claudeWritingDeclarations() {
				if slices.Contains(allow, declared) {
					t.Errorf("%s: spawn #%d approves %q on a readonly task (allowed=%v)", tc.name, i+1, declared, allow)
				}
			}
		}
	}

	// The SAME undeclared surface without the flag keeps the whole roster, so
	// the assertions above cannot pass by being true of every task: this is
	// the one input that decides them.
	plain := spawnArgv(t, Task{NodeID: "n", OutputSchema: schemaForTwoSpawns})
	for i, argv := range plain {
		for _, writer := range claudeWritingNatives {
			if got := disallowed(argv); slices.Contains(got, writer) {
				t.Errorf("spawn #%d of an undeclared surface with NO readonly lost %q (disallowed=%v)", i+1, writer, got)
			}
		}
	}

	// The other half of what this backend promises, and the one every
	// statement of the flag in this repo rests on: the shell SURVIVES the
	// withholding on an undeclared surface. The catalog's injection
	// boundaries, docs/dsl.md and the notice this backend logs all tell a
	// readonly node it keeps its shell; a change that withheld Bash here
	// would make every one of them false in silence, so the sentence is
	// asserted where the argv is built.
	for i, argv := range spawnArgv(t, Task{NodeID: "n", Readonly: true, OutputSchema: schemaForTwoSpawns}) {
		if got := disallowed(argv); slices.Contains(got, "Bash") {
			t.Errorf("spawn #%d of an undeclared readonly surface withheld Bash (disallowed=%v): what the catalog's prompts and the docs say about this flag moves in the same change, or they are lies", i+1, got)
		}
	}
}

// worktreeMovers is what `readonly: true` must remove from a claude_code
// spawn whatever the declaration says, spelled as the CLI spells it — a
// LITERAL, not a read of claudeWorktreeMovingTools.
//
// Reading the production variable made this guard answer "nothing is missing"
// to an emptied pair: both loops below ran zero times and the test passed,
// leaving the arm it exists for covered only by a test about another spawn
// (claude_code_tool_boundary_argv_test.go, the gated formatting pass).
var worktreeMovers = []string{"EnterWorktree", "ExitWorktree"}

// TestEverySpawnOfAReadonlyTaskWithholdsTheWorktreeMovingTools pins the other
// half of what `readonly: true` removes: the two tools that MOVE the worktree
// the session acts in.
//
// They are the write a readonly node can least afford — the flag is also what
// admits the node into a shared workspace as non-mutating — and no `tools:`
// list can withhold them, because no row of claudeNativeForCanonical names
// them: the declaration's half of --disallowedTools is a projection of that
// table, so a reader-only declaration leaves the pair registered. Asserted on
// a DECLARED reader surface for exactly that reason, and on both spawns.
func TestEverySpawnOfAReadonlyTaskWithholdsTheWorktreeMovingTools(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	// The production list and this guard's literal name the same pair: the
	// assertions below are about the argv, and this line is what makes an
	// emptied or renamed variable a failure here rather than a silent
	// no-op.
	if !slices.Equal(claudeWorktreeMovingTools, worktreeMovers) {
		t.Fatalf("claudeWorktreeMovingTools is %v, this guard's literal is %v: the pair this backend withholds moved, so either the argv assertions below no longer cover it, or the live CLI's roster changed and both lists owe an update",
			claudeWorktreeMovingTools, worktreeMovers)
	}
	for _, tc := range []struct {
		name string
		task Task
	}{
		{"reader-only declaration", Task{
			NodeID: "n", Readonly: true, AllowedTools: []string{"read_file", "glob", "grep"},
			ToolsDeclared: true, OutputSchema: schemaForTwoSpawns,
		}},
		{"undeclared surface", Task{NodeID: "n", Readonly: true, OutputSchema: schemaForTwoSpawns}},
	} {
		spawns := spawnArgv(t, tc.task)
		if len(spawns) < 2 {
			t.Fatalf("%s: expected two spawns, got %d: %v", tc.name, len(spawns), spawns)
		}
		for i, argv := range spawns {
			got := disallowed(argv)
			for _, mover := range worktreeMovers {
				if !slices.Contains(got, mover) {
					t.Errorf("%s: spawn #%d of a readonly task keeps %q, which moves the worktree the engine admitted this node into (disallowed=%v)",
						tc.name, i+1, mover, got)
				}
			}
		}
	}

	// The same declaration without the flag keeps the pair: the assertions
	// above read the flag, not a bound every declared surface has.
	plain := spawnArgv(t, Task{
		NodeID: "n", AllowedTools: []string{"read_file", "glob", "grep"},
		ToolsDeclared: true, OutputSchema: schemaForTwoSpawns,
	})
	for i, argv := range plain {
		for _, mover := range worktreeMovers {
			if got := disallowed(argv); slices.Contains(got, mover) {
				t.Errorf("spawn #%d of a declaration with NO readonly lost %q (disallowed=%v)", i+1, mover, got)
			}
		}
	}
}

// TestAReadonlyDeclarationThatNamesAWriterLosesItAndIsToldSo is how this
// backend resolves the one conflict `readonly:` can have with a `tools:`
// list, asserted rather than assumed: the flag wins, on both spawns, and the
// deleted declaration is named on the node's own log.
//
// The flag is the node's ACCESS INTENT, and it is also what admits the node
// into a fan-out branch as non-mutating (pkg/runtime, isMutatingNodeIn), so a
// writer kept beside it would write in a workspace another branch was told
// nobody mutates. codex (a read-only OS sandbox) and pi (a readers-only tool
// list) resolve it the same way; philosophy rule 1 is satisfied by the notice
// and by the escape hatch it names — dropping `readonly:` — not by keeping
// the tool.
func TestAReadonlyDeclarationThatNamesAWriterLosesItAndIsToldSo(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	rec := spawnRecorded(t, Task{
		NodeID: "n", Readonly: true,
		AllowedTools:  []string{"bash", "read_file", "write_file", "glob", "grep"},
		ToolsDeclared: true, OutputSchema: schemaForTwoSpawns,
	})
	if len(rec.argv) < 2 {
		t.Fatalf("expected two spawns, got %d: %v", len(rec.argv), rec.argv)
	}
	for i, argv := range rec.argv {
		got := disallowed(argv)
		for _, writer := range claudeWritingNatives {
			if !slices.Contains(got, writer) {
				t.Errorf("spawn #%d keeps %q on a readonly node (disallowed=%v)", i+1, writer, got)
			}
		}
		// …and the name the author typed is not auto-approved either: a tool
		// on both flags would make the withholding rest on the CLI resolving
		// deny over allow.
		if allow := allowedFlag(argv); slices.Contains(allow, "write_file") {
			t.Errorf("spawn #%d approves the write_file this node declares, which the same spawn withholds (allowed=%v)", i+1, allow)
		}
		// The readers it declared are untouched: the flag withholds the
		// writers, it does not lock the node down.
		if allow := allowedFlag(argv); !slices.Contains(allow, "read_file") {
			t.Errorf("spawn #%d dropped the read_file this node declares (allowed=%v)", i+1, allow)
		}
	}
	if !logged(rec.logs, "readonly: true removes Write") {
		t.Errorf("the node's declared writer was deleted from both flags with no word about it: %v", rec.logs)
	}
	// The way out is named, and it is the only one: no `tools:` list can buy
	// the writer back.
	if !logged(rec.logs, "Drop readonly:") {
		t.Errorf("the notice about the deleted declaration names no way out: %v", rec.logs)
	}
}

// TestAReadonlyClaudeCodeNodeIsToldWhatTheFlagDoesNotCover pins the two
// sentences the backend says about a flag it honours in part: the tools a
// declaration asked for and readonly removed anyway, and the write paths
// readonly cannot close here.
//
// The silence is the defect #1816 reports — an author who relies on the flag
// gets no enforcement AND no word about what is left.
func TestAReadonlyClaudeCodeNodeIsToldWhatTheFlagDoesNotCover(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")

	// A shell survives readonly: this backend cannot make one read-only.
	shell := spawnRecorded(t, Task{
		NodeID: "n", Readonly: true, AllowedTools: []string{"bash", "read_file"},
		ToolsDeclared: true,
	})
	if !logged(shell.logs, "readonly") || !logged(shell.logs, "shell") {
		t.Errorf("a readonly node that keeps a shell was told nothing: %v", shell.logs)
	}
	if !logged(shell.logs, "codex") || !logged(shell.logs, "pi") {
		t.Errorf("the notice names no backend that enforces the flag: %v", shell.logs)
	}

	// A readonly node whose declaration already removed the shell has
	// nothing left to be told: the flag's promise holds there.
	reader := spawnRecorded(t, Task{
		NodeID: "n", Readonly: true, AllowedTools: []string{"read_file"},
		ToolsDeclared: true,
	})
	if logged(reader.logs, "readonly") {
		t.Errorf("a readonly node with no shell was warned anyway: %v", reader.logs)
	}

	// A subagent is the second residual write path: this backend bounds the
	// argv IT spawns, not the one the CLI hands a subagent, and the CLI
	// finalises that subagent's worktree. A node keeping it is told so, with
	// no shell in the picture — otherwise the shell leg alone would satisfy
	// the assertion.
	subagent := spawnRecorded(t, Task{
		NodeID: "n", Readonly: true, AllowedTools: []string{"read_file", "agent"},
		ToolsDeclared: true,
	})
	if !logged(subagent.logs, "subagent") {
		t.Errorf("a readonly node kept the subagent tool and was told nothing about it: %v", subagent.logs)
	}

	// A declaration this backend DELETES: `readonly:` removes the
	// worktree-moving pair whatever the `tools:` list says — and philosophy
	// rule 1 forbids replacing an explicit choice in silence. No shell, no
	// subagent and no declared writer in this shape, so none of the other
	// legs can answer for this one: before the notice existed, this Execute
	// logged nothing at all.
	mover := spawnRecorded(t, Task{
		NodeID: "n", Readonly: true, AllowedTools: []string{"enterworktree", "read_file"},
		ToolsDeclared: true,
	})
	if !logged(mover.logs, "EnterWorktree") {
		t.Errorf("a readonly node's declared EnterWorktree was deleted from both flags with no word about it: %v", mover.logs)
	}
	// …and the way out is named: dropping the flag is the only one.
	if !logged(mover.logs, "Drop readonly:") {
		t.Errorf("the notice about the deleted declaration names no way out: %v", mover.logs)
	}
	// A readonly declaration that does NOT name the pair is not told about
	// it: the notice reads the declaration, it is not printed on every
	// readonly node.
	if logged(reader.logs, "EnterWorktree") {
		t.Errorf("a readonly node that never declared the pair was told about it anyway: %v", reader.logs)
	}

	// …and the spelling the DSL uses for every other multi-word tool draws
	// the same notice: the shared table has no row for the pair, so a notice
	// keyed on the table alone fired for `enterworktree` and said nothing to
	// the author who wrote `enter_worktree`.
	snake := spawnRecorded(t, Task{
		NodeID: "n", Readonly: true, AllowedTools: []string{"enter_worktree", "read_file"},
		ToolsDeclared: true,
	})
	if !logged(snake.logs, "removes EnterWorktree") {
		t.Errorf("a readonly node's declared enter_worktree was deleted from both flags with no word about it: %v", snake.logs)
	}
	// The notice's own claim about that name holds for this spelling too: it
	// is not auto-approved either, which is only true because the same fold
	// drops it from --allowedTools.
	if allow := allowedFlag(snake.argv[0]); slices.Contains(allow, "enter_worktree") {
		t.Errorf("the notice says the name is not auto-approved, but --allowedTools carries it: %v", allow)
	}

	// A declared writer is deleted and named, like the pair: the flag beats
	// the list, and silence there is the defect this test exists for — an
	// author who reads nothing believes the tool their prompt uses is still
	// on the argv.
	writer := spawnRecorded(t, Task{
		NodeID: "n", Readonly: true, AllowedTools: []string{"write", "read_file"},
		ToolsDeclared: true,
	})
	if !logged(writer.logs, "removes Write") {
		t.Errorf("a readonly node's declared Write was deleted and it was told nothing: %v", writer.logs)
	}
	// …and the reason the flag wins is named: its OTHER meaning is the
	// fan-out admission (pkg/runtime, isMutatingNodeIn returns false for a
	// readonly agent/judge), so a writer kept here would run beside a
	// parallel branch in the same workspace.
	if !logged(writer.logs, "shared workspace") {
		t.Errorf("the deleted-writer notice does not say the flag also admits this node into a fan-out branch as non-mutating: %v", writer.logs)
	}

	// A node that declares BOTH a writer and the worktree pair is told about
	// both IN ONE notice: they are one rule, so a second branch that could
	// report one kind and forget the other is what this asserts against.
	both := spawnRecorded(t, Task{
		NodeID: "n", Readonly: true, AllowedTools: []string{"write", "enterworktree"},
		ToolsDeclared: true,
	})
	if !logged(both.logs, "removes Write, EnterWorktree") {
		t.Errorf("the two declarations the flag deletes are not reported together: %v", both.logs)
	}

	// And a task without the flag says nothing about it.
	plain := spawnRecorded(t, Task{
		NodeID: "n", AllowedTools: []string{"bash", "read_file"}, ToolsDeclared: true,
	})
	if logged(plain.logs, "readonly") {
		t.Errorf("a task that never declared readonly was told about it: %v", plain.logs)
	}
}

// logged reports whether any line the backend logged during one Execute holds
// needle. It is case-sensitive: the notices name tools and backends by their
// own spelling.
func logged(lines []string, needle string) bool {
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// TestTheUnderscoreFoldMergesNothingButTheWorktreeMovers is the floor under
// claudeFoldedToolName. The fold is what keeps `enter_worktree` from landing
// on both flags, and it is read by the approval subtraction — so a fold that
// merged two DIFFERENT tools would drop a declared name from --allowedTools
// although the spawn denies nothing of the kind, which is the inverse of
// #1673 and just as silent.
//
// Asserted over every name this repo can put on either flag, not over the
// pair the fold was written for: the claim is about what else it merges.
func TestTheUnderscoreFoldMergesNothingButTheWorktreeMovers(t *testing.T) {
	universe := map[string]bool{}
	for key, value := range toolcatalog.CanonicalRows() {
		universe[key] = true
		universe[value] = true
	}
	for _, name := range claudeNativeTools {
		universe[name] = true
	}
	for _, name := range headlessWithheldTools {
		universe[name] = true
	}
	for _, name := range orchestrationTools {
		universe[name] = true
	}
	for _, name := range claudeWorktreeMovingTools {
		universe[name] = true
	}
	for _, name := range []string{
		"enter_worktree", "exit_worktree", "enterworktree", "exitworktree",
		"mcp__engine__local_run", "mcp_engine_local_run",
		"ask_user", "read_image", "StructuredOutput", "diagnostic_shell",
	} {
		universe[name] = true
	}
	if len(universe) < 60 {
		t.Fatalf("the universe holds %d name(s), want at least 60: the lists this reads moved, and the fold is now checked against nothing", len(universe))
	}

	// The merges this fold is ALLOWED to make, each a set of canonical names
	// that are one tool: the two movers the shared table has no row for, and
	// the two MCP prefixes of one FQN (CanonicalToolName returns those
	// verbatim, and no list here withholds an MCP name).
	allowed := map[string]bool{
		"enter_worktree|enterworktree":                true,
		"exit_worktree|exitworktree":                  true,
		"mcp__engine__local_run|mcp_engine_local_run": true,
	}
	merged := map[string]map[string]bool{}
	for name := range universe {
		canonical := toolcatalog.CanonicalToolName(name)
		folded := claudeFoldedToolName(name)
		if merged[folded] == nil {
			merged[folded] = map[string]bool{}
		}
		merged[folded][canonical] = true
	}
	for folded, canonicals := range merged {
		if len(canonicals) < 2 {
			continue
		}
		names := make([]string, 0, len(canonicals))
		for canonical := range canonicals {
			names = append(names, canonical)
		}
		slices.Sort(names)
		if !allowed[strings.Join(names, "|")] {
			t.Errorf("the underscore fold merges %v onto %q, and they are not the same tool: the approval subtraction reads this key, so one of them would be dropped from --allowedTools although the spawn denies nothing of the kind",
				names, folded)
		}
	}
}
