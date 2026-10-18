package toolcatalog

import (
	"slices"
	"strings"
	"testing"
)

// TestTheShellClassHoldsExactlyTheCodeExecutors pins the class:shell
// membership: the three built-ins that execute model-supplied code, and
// nothing else. A builtin tagged wrongly here is the false positive (a
// bounded tool denied by a class row) and the false negative (a second
// shell a deny row never reaches) for everything downstream — the gate, the
// grammar and the C167 warning all read this table.
func TestTheShellClassHoldsExactlyTheCodeExecutors(t *testing.T) {
	got, err := ClassMembers(ClassShell)
	if err != nil {
		t.Fatalf("ClassMembers(shell): %v", err)
	}
	want := []string{"bash", "diagnostic_shell", "repl"}
	if !slices.Equal(got, want) {
		t.Fatalf("class:shell = %v, want %v — the class is the anti-drift point for the ticket's cell", got, want)
	}
	for _, m := range want {
		if !ClassContains(ClassShell, m) {
			t.Errorf("ClassContains(shell, %q) = false", m)
		}
		if !ClassContains("SHELL", m) {
			t.Errorf("class names are case-insensitive, like tool spellings: %q", m)
		}
	}
	for _, not := range []string{
		"workspace_grep", // content search, no process spawn
		"screenshot",     // display capture
		"computer_use",   // fixed xdotool argv; the model supplies coordinates, not code
		"skill",          // fixed git invocations on a URL, not a command line
		"read_file", "web_fetch", "grep", "glob", "agent", "todo_write",
	} {
		if ClassContains(ClassShell, not) {
			t.Errorf("ClassContains(shell, %q) = true — %q is bounded by its own name, not a code executor", not, not)
		}
	}
	// The canonical table routes the shell spellings onto bash; the class
	// must match them through it, not by literal name.
	for _, spelling := range []string{"Bash", "sh", "shell", "run_command", "run_terminal_command"} {
		if !ClassContains(ClassShell, spelling) {
			t.Errorf("ClassContains(shell, %q) = false — the canonical table routes it onto bash", spelling)
		}
	}
	// An unknown class contains nothing and errors on its members: a typo'd
	// class must read as empty, never as "everything".
	if ClassContains("shel", "bash") {
		t.Error("an unknown class contains nothing")
	}
	if _, err := ClassMembers("shel"); err == nil {
		t.Error("ClassMembers on an unknown class must error, naming the known ones")
	} else if !strings.Contains(err.Error(), "shell") {
		t.Errorf("the error must name the known classes, got: %v", err)
	}
	if KnownClasses() == nil || !slices.Contains(KnownClasses(), "shell") {
		t.Errorf("KnownClasses() = %v, want shell among them", KnownClasses())
	}
	if !IsKnownClass(" shell ") {
		t.Error("IsKnownClass trims and folds case, like every other name read")
	}
}

// TestAFutureBuiltinLandsInTheClassWithOneCapabilityRow is the acceptance
// test of the design: the class resolves from the registry's capability
// metadata at match time, so a built-in that registers with the capability
// AFTER the rules were written is covered — no permissions-package change,
// no grammar change, no workflow edit. The test adds the exact rows a real
// future built-in would add (one capability row beside its catalog row) and
// reads the class back.
func TestAFutureBuiltinLandsInTheClassWithOneCapabilityRow(t *testing.T) {
	const future = "future_shell"
	if IsBuiltin(future) {
		t.Fatalf("%q is already in the catalog", future)
	}
	// The rows a real registration adds. clawBuiltins is the catalog row the
	// conformance test guards; builtinCapabilities is the capability row the
	// class reads.
	clawBuiltins[future] = true
	builtinCapabilities[future] = map[Capability]bool{CapShellExec: true}
	t.Cleanup(func() {
		delete(clawBuiltins, future)
		delete(builtinCapabilities, future)
	})

	got, err := ClassMembers(ClassShell)
	if err != nil {
		t.Fatalf("ClassMembers(shell): %v", err)
	}
	if !slices.Contains(got, future) {
		t.Fatalf("a built-in registering with the capability must land in class:shell with no other change; got %v", got)
	}
	if !ClassContains(ClassShell, future) {
		t.Error("ClassContains(shell, future_shell) = false")
	}
}

// TestACapabilityTagNeverOutlivesItsCatalog refuses drift inside the leaf: a
// capability row for a name the catalog does not hold would be a class
// member nothing can register — the exact shape that let a class go stale
// before the conformance guard existed.
func TestACapabilityTagNeverOutlivesItsCatalog(t *testing.T) {
	for name, caps := range builtinCapabilities {
		if !clawBuiltins[name] {
			t.Errorf("builtinCapabilities tags %q, which clawBuiltins does not hold — the class covers a tool nothing registers", name)
		}
		if len(caps) == 0 {
			t.Errorf("builtinCapabilities[%q] is an empty set — tag the capability or drop the row", name)
		}
	}
	if !HasCapability("bash", CapShellExec) {
		t.Error("HasCapability(bash, shell-exec) = false")
	}
	if HasCapability("nope", CapShellExec) {
		t.Error("a name outside the catalog carries no capability")
	}
	// CapabilityClass is what the warning's remedy reads: the suggestion
	// must follow the table, not a hardcoded string.
	class, ok := CapabilityClass(CapShellExec)
	if !ok || class != ClassShell {
		t.Errorf("CapabilityClass(shell-exec) = %q, %v; want %q", class, ok, ClassShell)
	}
}

// TestClassRuleForFollowsTheClassTable pins the remedy's source: the
// `class:<name>` advice a diagnostic suggests is read from the class table,
// so renaming the class moves the advice with it instead of leaving every
// message suggesting a rule the parser refuses. Red when the advice is
// hardcoded beside the table it describes.
func TestClassRuleForFollowsTheClassTable(t *testing.T) {
	if got := ClassRuleFor(CapShellExec); got != "class:shell" {
		t.Fatalf("ClassRuleFor(shell-exec) = %q, want class:shell", got)
	}
	renamed := map[string]Capability{"command_execution": CapShellExec}
	saved := ruleClasses
	ruleClasses = renamed
	t.Cleanup(func() { ruleClasses = saved })

	if got := ClassRuleFor(CapShellExec); got != "class:command_execution" {
		t.Errorf("a renamed class must rename the advice; got %q, want class:command_execution", got)
	}
	if _, ok := CapabilityClass(CapShellExec); !ok {
		t.Error("CapabilityClass lost the capability its table holds")
	}
	// A capability no class backs still answers: the callers need a total
	// string, and the fallback names the class constant.
	ruleClasses = map[string]Capability{}
	if got := ClassRuleFor(CapShellExec); got != "class:"+ClassShell {
		t.Errorf("ClassRuleFor with no backing class = %q, want the ClassShell fallback", got)
	}
}
