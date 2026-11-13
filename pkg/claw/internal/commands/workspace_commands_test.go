package commands

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// writeCommand puts a command file under dir's .claude/commands, creating
// the namespace directories the name implies.
func writeCommand(t *testing.T, dir, rel, body string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude", "commands", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A prose prompt that opens with a filesystem path is not an invocation.
// Mutation: widen the name charset to accept "/" and "/usr/bin/foo is
// broken" starts resolving as the command "usr".
func TestParseInvocationRejectsPathLikeToken(t *testing.T) {
	for _, prompt := range []string{
		"/usr/bin/foo is broken, fix it",
		"/etc/hosts is broken",
		"/..",
		"/.",
		"/a:..:b",
		"/",
		"//double",
		"/trailing:",
		"/:leading",
		"no slash at all",
		"",
	} {
		if _, _, ok := ParseInvocation(prompt); ok {
			t.Errorf("ParseInvocation(%q) = invocation, want prose", prompt)
		}
	}
}

// Mutation: drop ':' from the name charset and a namespaced command stops
// parsing, so nothing under a subdirectory can ever resolve.
func TestParseInvocationAcceptsNamespacedName(t *testing.T) {
	name, args, ok := ParseInvocation("/sub:nested")
	if !ok || name != "sub:nested" || args != "" {
		t.Fatalf("ParseInvocation = (%q, %q, %v), want (\"sub:nested\", \"\", true)", name, args, ok)
	}
}

// Mutation: return the whole remainder as the name and the argument string
// is lost, so $ARGUMENTS expands to nothing.
func TestParseInvocationSplitsArguments(t *testing.T) {
	name, args, ok := ParseInvocation("/probe-args   hello world  ")
	if !ok {
		t.Fatal("ParseInvocation: not recognised")
	}
	if name != "probe-args" {
		t.Errorf("name = %q, want probe-args", name)
	}
	if args != "hello world  " {
		t.Errorf("args = %q, want %q", args, "hello world  ")
	}
}

// A multi-line prompt invokes on its FIRST line; the rest is argument text.
func TestParseInvocationStopsAtNewline(t *testing.T) {
	name, args, ok := ParseInvocation("/review\nsecond line")
	if !ok || name != "review" {
		t.Fatalf("name = %q ok = %v, want review/true", name, ok)
	}
	if args != "\nsecond line" {
		t.Errorf("args = %q, want %q", args, "\nsecond line")
	}
}

// Mutation: join the namespace with "-" or ":" instead of a path separator
// and the file is never found.
func TestLookupWorkspaceMapsNamespaceToDirectory(t *testing.T) {
	dir := t.TempDir()
	want := writeCommand(t, dir, filepath.Join("sub", "nested.md"), "Reply NESTED-4412\n")

	cmd, ok, err := LookupWorkspace(dir, "sub:nested", 0)
	if err != nil || !ok {
		t.Fatalf("LookupWorkspace = (%v, %v), want found", ok, err)
	}
	if cmd.Path != want {
		t.Errorf("Path = %q, want %q", cmd.Path, want)
	}
	if cmd.Body != "Reply NESTED-4412" {
		t.Errorf("Body = %q", cmd.Body)
	}
}

// The workspace is a checkout the caller does not control; resolution must
// not climb out of it. Mutation: re-introduce the ancestor walk that
// LoadDirCommands uses and the parent's command resolves from the child.
func TestLookupWorkspaceDoesNotWalkAncestors(t *testing.T) {
	parent := t.TempDir()
	writeCommand(t, parent, "outside.md", "must not be reachable\n")
	child := filepath.Join(parent, "ws")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := LookupWorkspace(child, "outside", 0); ok || err != nil {
		t.Fatalf("LookupWorkspace from child = (%v, %v), want not found", ok, err)
	}
	// The control: the same file IS reachable from the directory that owns it.
	if _, ok, err := LookupWorkspace(parent, "outside", 0); !ok || err != nil {
		t.Fatalf("LookupWorkspace from owner = (%v, %v), want found", ok, err)
	}
}

// A name cannot SPELL traversal: a separator is refused outright, and a
// component that is nothing but dots is refused too. (os.Root is what makes
// containment a guarantee — see the symlink tests — but a name has no
// business expressing it.)
//
// Mutation: drop the all-dots component check and `a:..:b` resolves to
// `b.md`, climbing out of its namespace.
func TestLookupWorkspaceRejectsTraversalName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../secret", "..", ".", "a/../../b", "a:..:b"} {
		if _, ok, err := LookupWorkspace(dir, name, 0); err == nil || ok {
			t.Errorf("LookupWorkspace(%q, 0) = (%v, %v), want an error", name, ok, err)
		}
	}
}

// Expand's second return is the ONLY answer to "did a placeholder take the
// arguments" — a separate predicate over the body disagreed with this loop
// at both ends ($0 and an out-of-range $N are placeholders a reader sees and
// the loop leaves literal), and on that disagreement the caller neither
// substituted nor appended: the operator's message was deleted.
//
// Mutation: report consumed=true whenever the body carries a "$" and the
// caller stops appending for $0 / out-of-range bodies.
func TestExpandReportsWhetherItTookTheArguments(t *testing.T) {
	consuming := []string{"ARGS=[$ARGUMENTS]", "FIRST=[$1]", "[$2]"}
	for _, body := range consuming {
		if _, consumed, _ := Expand(WorkspaceCommand{Body: body}, "alpha beta", 0); !consumed {
			t.Errorf("Expand(%q) reported consumed=false, want true", body)
		}
	}
	// Every one of these LOOKS like it takes an argument and does not: the
	// caller has to append, or the arguments vanish.
	literal := []string{"plain body", "index $0", "out of range $9", "a price $100", "an id $1_x"}
	for _, body := range literal {
		got, consumed, _ := Expand(WorkspaceCommand{Body: body}, "alpha beta", 0)
		if consumed {
			t.Errorf("Expand(%q) reported consumed=true, want false", body)
		}
		if got != body {
			t.Errorf("Expand(%q) = %q, want it unchanged", body, got)
		}
	}
}

// A placeholder's meaning is a property of the placeholder, never of the
// text around it. Mutation: bound the index by len(body) — "$3" then
// substitutes in a long body and stays literal in a short one, so padding a
// body with prose changes what it means.
func TestExpandPositionalDoesNotDependOnBodyLength(t *testing.T) {
	for _, body := range []string{"$3", "$3 with a good deal more prose after it to pad the body out"} {
		got, consumed, _ := Expand(WorkspaceCommand{Body: body}, "a b c", 0)
		if !consumed || !strings.HasPrefix(got, "c") {
			t.Errorf("Expand(%q) = (%q, %v), want the third argument substituted", body, got, consumed)
		}
	}
}

// Mutation: drop the $ARGUMENTS branch and the placeholder reaches the model
// as literal text.
func TestExpandSubstitutesArguments(t *testing.T) {
	cmd := WorkspaceCommand{Body: "ARGS=[$ARGUMENTS] done"}
	if got, _, _ := Expand(cmd, "hello world", 0); got != "ARGS=[hello world] done" {
		t.Errorf("Expand = %q", got)
	}
}

// Claude Code 2.1.220 resolves $1 to the SECOND argument (measured:
// "/probe-three alpha beta gamma" on "[$1][$2][$3]" expands to
// "[beta][gamma][$3]"). That is off by one against its own documented
// contract, and this test is the guard that we implement the documented
// reading. Mutation: shift the index by one — the claude_code behaviour —
// and this reddens.
func TestExpandPositionalIsOneBased(t *testing.T) {
	cmd := WorkspaceCommand{Body: "[$1][$2][$3]"}
	if got, _, _ := Expand(cmd, "alpha beta gamma", 0); got != "[alpha][beta][gamma]" {
		t.Errorf("Expand = %q, want [alpha][beta][gamma]", got)
	}
}

// Mutation: substitute an out-of-range index with the empty string and the
// body silently loses the text an author wrote on purpose.
func TestExpandLeavesOutOfRangePositionalLiteral(t *testing.T) {
	cmd := WorkspaceCommand{Body: "[$1][$2][$3]"}
	if got, _, _ := Expand(cmd, "only-one", 0); got != "[only-one][$2][$3]" {
		t.Errorf("Expand = %q, want [only-one][$2][$3]", got)
	}
}

// An argument that itself contains a placeholder must not be re-scanned.
// Mutation: implement Expand as successive strings.ReplaceAll passes — the
// "$2" carried INTO the body by the argument text is then seen by the
// positional pass and expands a second time, to "beta beta".
//
// The fixture is chosen so the two implementations disagree: an argument of
// "$1 literal" would substitute to itself under both, and prove nothing.
func TestExpandDoesNotRescanSubstitutedArguments(t *testing.T) {
	cmd := WorkspaceCommand{Body: "<$ARGUMENTS>"}
	if got, _, _ := Expand(cmd, "$2 beta", 0); got != "<$2 beta>" {
		t.Errorf("Expand = %q, want <$2 beta>", got)
	}
}

// A body with no placeholder at all comes back byte-identical.
func TestExpandLeavesAPlainBodyAlone(t *testing.T) {
	cmd := WorkspaceCommand{Body: "Reply with exactly this token: KUMQUAT-7731"}
	if got, _, _ := Expand(cmd, "ignored", 0); got != cmd.Body {
		t.Errorf("Expand = %q, want the body unchanged", got)
	}
}

// Mutation: return nil unconditionally and a caller can no longer tell an
// author that a body means something else on the other backend.
func TestDynamicBodyFormsNamesDivergentFeatures(t *testing.T) {
	got := DynamicBodyForms("run !`git status` then read @docs/x.md for $1", "alpha")
	want := []string{"!`\u2026`", "$N"}
	if !slices.Equal(got, want) {
		t.Errorf("DynamicBodyForms = %v, want %v (sorted, as the godoc says)", got, want)
	}
}

// The diagnostic and the substitution must never disagree about what a
// placeholder IS — they share one scanner. Mutation: go back to a
// `strings.Contains(body, "$")`-style probe and a price warns while a real
// placeholder in "$10" does not.
func TestDynamicBodyFormsAgreesWithExpand(t *testing.T) {
	quiet := []string{
		"Our budget is $500 per month.",      // a price, not a placeholder
		"npm i @anthropic-ai/sdk",            // an npm scope, not a file reference
		"shout at the end!`",                 // a bang mid-word, no closing tick
		"an empty pair !`` proves nothing",   // no command between the ticks
		"plain body, e-mail a@b.c stays put", // an address, not a reference
	}
	for _, body := range quiet {
		if got := DynamicBodyForms(body, "alpha beta"); len(got) != 0 {
			t.Errorf("DynamicBodyForms(%q) = %v, want silence", body, got)
		}
	}
	loud := map[string]string{
		"use $ARGUMENTS[0] here": "$ARGUMENTS[n]",
		// Beyond $9: Expand substitutes it, so the divergence must be named
		// or a body using it takes the CLI's off-by-one in silence.
		"the tenth is $10":      "$N",
		"first is $0":           "$0",
		"```!\ngit status\n```": "!`\u2026`",
		`a \$1 escape`:          `\$`,
	}
	for body, want := range loud {
		if got := DynamicBodyForms(body, "a b c d e f g h i j k l"); !slices.Contains(got, want) {
			t.Errorf("DynamicBodyForms(%q) = %v, want it to name %q", body, got, want)
		}
	}
}

// A repository under review ships whatever it likes under `.claude/commands/`,
// including a SYMLINK. The charset stops `..` from being spelled; it does
// nothing about a link, and os.ReadFile follows one.
//
// Mutation: swap the os.Root read back for os.ReadFile on the joined path —
// each of these three resolves and the private key lands in the prompt.
func TestLookupWorkspaceRefusesASymlinkOutOfTheCommandsDir(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(outside, []byte("PROBE-SECRET-8841"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	cmds := filepath.Join(ws, ".claude", "commands")
	if err := os.MkdirAll(filepath.Join(cmds, "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cmds, "leak.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(cmds, "esc")); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"leak", "esc:id_rsa"} {
		cmd, ok, err := LookupWorkspace(ws, name, 0)
		if ok || err == nil {
			t.Errorf("LookupWorkspace(%q, 0) = (ok=%v, err=%v) body=%q — the symlink escaped the commands dir",
				name, ok, err, cmd.Body)
		}
		if strings.Contains(cmd.Body, "PROBE-SECRET") {
			t.Errorf("LookupWorkspace(%q, 0) leaked the target file", name)
		}
	}
	// The control: a real file next to the symlinks still resolves, so the
	// containment is not just refusing everything.
	writeCommand(t, ws, filepath.Join("ns", "real.md"), "legitimate\n")
	if _, ok, err := LookupWorkspace(ws, "ns:real", 0); !ok || err != nil {
		t.Errorf("a real namespaced command stopped resolving: ok=%v err=%v", ok, err)
	}
}

// A `$N` placeholder is the WHOLE digit run, and never the prefix of a word.
// Mutation: read a single digit — "$100" becomes "<arg>00" and every price
// or identifier in a command body is silently corrupted.
func TestExpandLeavesMultiDigitAndWordPrefixedDollarsAlone(t *testing.T) {
	cases := map[string]string{
		"the price is $100 today": "the price is $100 today",
		"$1_suffix":               "$1_suffix",
		"$1x":                     "$1x",
		"$0 only":                 "$0 only",
		"[$1][$2]":                "[alpha][beta]",
		"$3 is out of range":      "$3 is out of range",
	}
	for body, want := range cases {
		if got, _, _ := Expand(WorkspaceCommand{Body: body}, "alpha beta", 0); got != want {
			t.Errorf("Expand(%q) = %q, want %q", body, got, want)
		}
	}
}

// A name the filesystem cannot even hold is "no such command", not a read
// failure — the caller must not fail a node over a prose prompt.
// Mutation: return the raw error and a 300-character slash-token kills a run.
func TestLookupWorkspaceTreatsAnImpossibleNameAsNotFound(t *testing.T) {
	ws := t.TempDir()
	writeCommand(t, ws, "present.md", "body\n")
	if err := os.Mkdir(filepath.Join(ws, ".claude", "commands", "trap.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A namespaced name whose intermediate segment is a regular FILE:
	// ENOTDIR, the arm no fixture reached.
	if err := os.WriteFile(filepath.Join(ws, ".claude", "commands", "seg"), []byte("a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{strings.Repeat("a", 300), "trap", "seg:nested"} {
		if _, ok, err := LookupWorkspace(ws, name, 0); ok || err != nil {
			t.Errorf("LookupWorkspace(%q, 0) = (ok=%v, err=%v), want a clean not-found", name[:min(len(name), 12)], ok, err)
		}
	}
}

// A leading `---` that is a horizontal rule is not frontmatter, and the text
// under it must survive — on this path the body is an instruction sent to a
// model, not help text.
//
// Mutation: drop the hasYAMLKey check and "A heading" disappears from the
// prompt; drop the whole-line close check and `----` truncates it to "-".
func TestFrontmatterIsNotAHorizontalRule(t *testing.T) {
	cases := map[string]string{
		"---\nA heading\n---\nrest of the body\n":     "---\nA heading\n---\nrest of the body\n",
		"---\ndescription: x\n----\nbody\n":           "---\ndescription: x\n----\nbody\n",
		"---\ndescription: ship it\n---\nreal body\n": "real body\n",
	}
	for content, wantBody := range cases {
		if got, _, _, _ := stripFrontmatter(content); got != wantBody {
			t.Errorf("stripFrontmatter(%q) body = %q, want %q", content, got, wantBody)
		}
	}
}

// Mutation: slice the description by byte and a multi-byte rune is cut in
// half, putting invalid UTF-8 on a public API field.
func TestDescriptionCapIsRuneSafe(t *testing.T) {
	_, desc, _, _ := stripFrontmatter(strings.Repeat("é", 200) + "\n")
	if !utf8.ValidString(desc) {
		t.Errorf("description is not valid UTF-8: %q", desc)
	}
	if n := utf8.RuneCountInString(desc); n > 120 {
		t.Errorf("description is %d runes, want it capped", n)
	}
}

// An empty or frontmatter-only file RESOLVES, with an empty body. The
// caller has to decide what that means; this pins the fact so it cannot
// change silently under it.
func TestLookupWorkspaceReportsAnEmptyBodyAsFound(t *testing.T) {
	ws := t.TempDir()
	writeCommand(t, ws, "empty.md", "")
	writeCommand(t, ws, "fmonly.md", "---\ndescription: nothing else\n---\n")
	for _, name := range []string{"empty", "fmonly"} {
		cmd, ok, err := LookupWorkspace(ws, name, 0)
		if !ok || err != nil {
			t.Fatalf("LookupWorkspace(%q, 0) = (%v, %v), want found", name, ok, err)
		}
		if cmd.Body != "" {
			t.Errorf("LookupWorkspace(%q, 0).Body = %q, want empty", name, cmd.Body)
		}
	}
}

// Mutation: return true unconditionally and every workspace pays the
// resolution path plus its diagnostics.
func TestHasWorkspaceCommands(t *testing.T) {
	empty := t.TempDir()
	if HasWorkspaceCommands(empty) {
		t.Error("HasWorkspaceCommands on a bare dir = true")
	}
	if HasWorkspaceCommands("") {
		t.Error("HasWorkspaceCommands(\"\") = true")
	}
	withCmd := t.TempDir()
	writeCommand(t, withCmd, "x.md", "body\n")
	if !HasWorkspaceCommands(withCmd) {
		t.Error("HasWorkspaceCommands with a command = false")
	}
}

// Frontmatter is stripped from the body and feeds the description — the
// same contract LoadDirCommands applies, through the same helper.
func TestLookupWorkspaceStripsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeCommand(t, dir, "ship.md", "---\ndescription: ship the change\n---\nRun tests then commit.\n")

	cmd, ok, err := LookupWorkspace(dir, "ship", 0)
	if err != nil || !ok {
		t.Fatalf("LookupWorkspace = (%v, %v)", ok, err)
	}
	if cmd.Description != "ship the change" {
		t.Errorf("Description = %q", cmd.Description)
	}
	if cmd.Body != "Run tests then commit." {
		t.Errorf("Body = %q, want the frontmatter stripped", cmd.Body)
	}
}

// A name that resolves to nothing is not an error: the caller decides what
// to do about it, and "no such command" must stay distinguishable from "the
// disk refused the read".
func TestLookupWorkspaceMissingIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	writeCommand(t, dir, "present.md", "body\n")
	cmd, ok, err := LookupWorkspace(dir, "absent", 0)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok || cmd.Path != "" {
		t.Errorf("= (%+v, %v), want the zero command and false", cmd, ok)
	}
}

// The containment boundary is the WORKSPACE, not the commands directory.
// Rooting at `.claude/commands` would resolve that path first, so a
// repository shipping it as a symlink moves the root itself and every read
// lands "contained" inside the wrong tree — the operator's own
// ~/.claude/commands, or the runner's scratch above the checkout.
//
// Mutation: root at CommandsDir(workDir) instead of workDir — each of these
// resolves and the out-of-tree body becomes the node's prompt.
func TestLookupWorkspaceRefusesASymlinkedCommandsDirectory(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "deploy.md"), []byte("HOST-ONLY PLAYBOOK"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("commands dir is a symlink out", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(ws, ".claude", "commands")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		cmd, ok, _ := LookupWorkspace(ws, "deploy", 0)
		if ok || strings.Contains(cmd.Body, "HOST-ONLY") {
			t.Errorf("a symlinked commands dir escaped: ok=%v body=%q", ok, cmd.Body)
		}
	})

	t.Run(".claude is a symlink out", func(t *testing.T) {
		ws := t.TempDir()
		shadow := t.TempDir()
		if err := os.MkdirAll(filepath.Join(shadow, "commands"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(shadow, "commands", "deploy.md"), []byte("HOST-ONLY PLAYBOOK"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(shadow, filepath.Join(ws, ".claude")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		cmd, ok, _ := LookupWorkspace(ws, "deploy", 0)
		if ok || strings.Contains(cmd.Body, "HOST-ONLY") {
			t.Errorf("a symlinked .claude escaped: ok=%v body=%q", ok, cmd.Body)
		}
	})

	t.Run("commands dir is a symlink above the checkout", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.WriteFile(filepath.Join(parent, "secrets.md"), []byte("RUNNER SCRATCH"), 0o644); err != nil {
			t.Fatal(err)
		}
		ws := filepath.Join(parent, "checkout")
		if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("..", filepath.Join(ws, ".claude", "commands")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		cmd, ok, _ := LookupWorkspace(ws, "secrets", 0)
		if ok || strings.Contains(cmd.Body, "RUNNER SCRATCH") {
			t.Errorf("a relative symlink escaped the checkout: ok=%v body=%q", ok, cmd.Body)
		}
	})

	// The control: containment must not disable the feature. A workspace
	// REACHED through a symlink (a symlinked home, /tmp on macOS, a bind
	// mount) still resolves its own commands.
	t.Run("a workspace reached through a symlink still resolves", func(t *testing.T) {
		real := t.TempDir()
		writeCommand(t, real, "ok.md", "legitimate\n")
		link := filepath.Join(t.TempDir(), "via-link")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, ok, err := LookupWorkspace(link, "ok", 0); !ok || err != nil {
			t.Errorf("a symlinked workspace stopped resolving: ok=%v err=%v", ok, err)
		}
	})
}

// A `.claude/commands` that is a regular FILE is "no such command", never a
// hard error — os.OpenRoot returns a plain sentinel there, not an errno, so
// classifying the error's spelling could not have caught it.
//
// Mutation: drop the HasWorkspaceCommands gate and every name returns an
// error, failing a node over a prompt that was prose.
func TestLookupWorkspaceTreatsAFileShapedCommandsDirAsNotFound(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".claude", "commands"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LookupWorkspace(ws, "anything", 0); ok || err != nil {
		t.Errorf("= (ok=%v, err=%v), want a clean not-found", ok, err)
	}
}

// A closing `---` with trailing whitespace is still a closing delimiter.
// Mutation: require the byte after `---` to be a newline and a whole YAML
// block lands in the model's prompt, with Description set to "---".
func TestFrontmatterClosesOnATrailingWhitespaceDelimiter(t *testing.T) {
	for _, content := range []string{
		"---\ndescription: ship it\n--- \nreal body\n",
		"---\ndescription: ship it\n---\t\nreal body\n",
	} {
		body, desc, _, _ := stripFrontmatter(content)
		if body != "real body\n" {
			t.Errorf("stripFrontmatter(%q) body = %q, want the frontmatter stripped", content, body)
		}
		if desc != "ship it" {
			t.Errorf("stripFrontmatter(%q) desc = %q, want %q", content, desc, "ship it")
		}
	}
}

// The reference trims before it parses (`let t=e.trim()`), so a prompt built
// from a template or a prior node's output — routinely arriving with a
// leading newline — is an invocation there.
//
// Mutation: drop the TrimLeft and every templated invocation silently stops
// resolving, with no diagnostic, because the parse simply says "prose".
func TestParseInvocationTrimsLeadingWhitespaceLikeTheReference(t *testing.T) {
	for _, prompt := range []string{" /review", "\n/review", "\t\n /review", "\r\n/review"} {
		name, _, ok := ParseInvocation(prompt)
		if !ok || name != "review" {
			t.Errorf("ParseInvocation(%q) = (%q, %v), want the invocation recognised", prompt, name, ok)
		}
	}
	// The control: whitespace does not turn prose into an invocation.
	if _, _, ok := ParseInvocation("  please run the thing"); ok {
		t.Error("prose was parsed as an invocation")
	}
}

// `\$` is honoured by the reference only before a digit or ARGUMENTS.
// Warning about `\$HOME` would be the always-wrong warning `@path` was
// dropped for; missing `\$1` would hide a real divergence.
//
// Mutation: report on a bare strings.Contains(body, `\$`) — the shell
// snippet warns and the author learns to ignore the warning.
func TestDynamicBodyFormsNarrowsTheEscapeToWhereItCounts(t *testing.T) {
	if got := DynamicBodyForms(`export HOME=\$HOME && echo ok`, "alpha"); len(got) != 0 {
		t.Errorf("DynamicBodyForms on a shell snippet = %v, want silence", got)
	}
	for _, body := range []string{`a \$1 escape`, `a \$ARGUMENTS escape`} {
		if got := DynamicBodyForms(body, "alpha"); !slices.Contains(got, `\$`) {
			t.Errorf("DynamicBodyForms(%q) = %v, want it to name the escape", body, got)
		}
	}
}

// The reference substitutes three ${CLAUDE_*} placeholders in a command
// body; Expand leaves them literal, so the body means two different things.
//
// Mutation: drop the branch and a command using ${CLAUDE_PROJECT_DIR} runs
// with the literal text on claw and a real path on claude_code, silently.
func TestDynamicBodyFormsNamesTheClaudeEnvPlaceholders(t *testing.T) {
	for _, body := range []string{
		"cd ${CLAUDE_PROJECT_DIR} first",
		"session ${CLAUDE_SESSION_ID}",
		"effort ${CLAUDE_EFFORT}",
	} {
		if got := DynamicBodyForms(body, "alpha"); !slices.Contains(got, "${CLAUDE_*}") {
			t.Errorf("DynamicBodyForms(%q) = %v, want it to name the placeholder", body, got)
		}
	}
}

// The bound has to be enforced AS THE OUTPUT IS PRODUCED, not measured on a
// finished string: both inputs are attacker-influenced when the body comes
// from a checkout under review, and a body that passes any sane file-size
// check still amplifies — 24 000 `$ARGUMENTS` times a pasted-diff argument
// reaches gigabytes. Measuring afterwards bounds the BILL and not the
// MEMORY, and the OOM lands on a multi-replica server's co-tenants.
//
// Mutation: expand unbounded and compare the length at the end — the
// allocation assertion goes red (measured ~98 MB against a 512 KiB budget).
func TestExpandRefusesWithoutAllocatingPastTheBound(t *testing.T) {
	const maxBytes = 1 << 16
	// Built before the measurement so the fixture is not counted.
	//
	// The body must sit UNDER the bound and still amplify past it, or the
	// cheap literal-size pre-check short-circuits and this measures that
	// guard instead of the produce-time one: 5 000 × 10 bytes = 50 000 < the
	// 65 536 bound, expanding to 5 000 × 4 096 ≈ 20 MB. (A 24 000-repeat
	// body was the first fixture and proved nothing — it is 240 KB, refused
	// before the loop ever ran.)
	body := strings.Repeat("$ARGUMENTS", 5000)
	args := strings.Repeat("x", 4096)
	if len(body) >= maxBytes {
		t.Fatalf("fixture is inert: the body is %d bytes, the pre-check refuses it before the loop", len(body))
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, _, err := Expand(WorkspaceCommand{Body: body}, args, maxBytes)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrExpansionTooLarge) {
		t.Fatalf("Expand = (%d bytes, %v), want ErrExpansionTooLarge", len(got), err)
	}
	if got != "" {
		t.Errorf("a refused expansion returned %d bytes, want none", len(got))
	}
	// Unbounded this body allocates 5 000 × 4 096 ≈ 20 MB (measured: 115 MB
	// of total churn through the builder's doubling). The budget is
	// generous on purpose — it separates "bounded" from "not bounded" by
	// two orders of magnitude, not by a tight constant that would flake.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8*maxBytes {
		t.Errorf("refusing allocated %d bytes, want at most %d — the expansion was materialised before being refused",
			grew, 8*maxBytes)
	}
}

// The bound refuses; it never truncates, and it never fires on a body that
// fits. Mutation: make the comparison `>=` and a body exactly at the bound
// is refused.
func TestExpandBoundAcceptsWhatFitsAndRefusesWhatDoesNot(t *testing.T) {
	cmd := WorkspaceCommand{Body: "[$ARGUMENTS]"}
	// "[" + args + "]" == 12 bytes with a 10-byte argument.
	got, consumed, err := Expand(cmd, strings.Repeat("y", 10), 12)
	if err != nil || !consumed || got != "["+strings.Repeat("y", 10)+"]" {
		t.Errorf("exactly at the bound = (%q, %v, %v), want the expansion", got, consumed, err)
	}
	if _, _, err := Expand(cmd, strings.Repeat("y", 11), 12); !errors.Is(err, ErrExpansionTooLarge) {
		t.Errorf("one byte over the bound = %v, want ErrExpansionTooLarge", err)
	}
	// A literal body larger than the bound is refused before any loop runs.
	if _, _, err := Expand(WorkspaceCommand{Body: strings.Repeat("z", 20)}, "", 12); !errors.Is(err, ErrExpansionTooLarge) {
		t.Errorf("an oversized literal body = %v, want ErrExpansionTooLarge", err)
	}
	// Unbounded stays unbounded.
	if _, _, err := Expand(cmd, strings.Repeat("y", 4096), 0); err != nil {
		t.Errorf("maxBytes=0 refused: %v", err)
	}
}

// The cheapest half of the class: a large command FILE needs no
// amplification at all. Reading it whole and measuring afterwards is the
// same defect the expander just stopped doing — and it is easier to
// trigger, since the attacker only has to commit a file.
//
// Mutation: read the whole file and check its length afterwards — the
// allocation assertion goes red (measured 2.00× the file, 537 MB for a
// 256 MiB command).
func TestLookupWorkspaceRefusesABigFileWithoutHoldingIt(t *testing.T) {
	const maxBytes = 1 << 16
	ws := t.TempDir()
	dir := filepath.Join(ws, ".claude", "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 16 MiB: far over the ceiling, small enough to keep the test quick. The
	// buffer lives in this block only, so the GC below can reclaim it.
	{
		big := make([]byte, 16<<20)
		for i := range big {
			big[i] = 'a'
		}
		if err := os.WriteFile(filepath.Join(dir, "huge.md"), big, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	cmd, ok, err := LookupWorkspace(ws, "huge", maxBytes)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("LookupWorkspace = (%v, %v), want ErrBodyTooLarge", ok, err)
	}
	if ok || cmd.Body != "" {
		t.Errorf("a refused file returned %d bytes of body", len(cmd.Body))
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16*maxBytes {
		t.Errorf("refusing allocated %d bytes for a 16 MiB file, want at most %d — the file was held before being refused",
			grew, 16*maxBytes)
	}

	// The control: a file under the ceiling still resolves.
	writeCommand(t, ws, "small.md", "fits\n")
	if _, ok, err := LookupWorkspace(ws, "small", maxBytes); !ok || err != nil {
		t.Errorf("a small command stopped resolving: ok=%v err=%v", ok, err)
	}
	// And unbounded stays unbounded.
	if _, ok, err := LookupWorkspace(ws, "huge", 0); !ok || err != nil {
		t.Errorf("maxBytes=0 refused a big file: ok=%v err=%v", ok, err)
	}
}

// The other quantity the ceiling never saw: splitting the arguments costs a
// 16-byte header per field — a fixed 8× amplification of the ARGUMENT
// bytes, paid even on the refusal path, and paid by every `$ARGUMENTS`-only
// body that never looks at a positional.
//
// Mutation: split eagerly (`fields := strings.Fields(args)` before the
// loop) — this goes red. It is also the fixture that exercises growHint's
// cap, which nothing else reaches: only a LARGE ARGUMENT makes the hint
// overshoot the bound.
func TestExpandDoesNotPayForArgumentsItNeverReads(t *testing.T) {
	const maxBytes = 12
	// A body with no positional at all: `fields` is never needed.
	cmd := WorkspaceCommand{Body: "$ARGUMENTS"}
	// MANY fields, not one big token: the cost is a 16-byte header PER
	// field, so a single 8 MiB word would allocate one header and prove
	// nothing. 500 000 two-byte words ≈ 8 MB of headers under an eager
	// split.
	args := strings.Repeat("y ", 500000)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, err := Expand(cmd, args, maxBytes)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrExpansionTooLarge) {
		t.Fatalf("Expand = %v, want ErrExpansionTooLarge", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Errorf("refusing allocated %d bytes for arguments never read, want under 1 MiB — the split was paid anyway",
			grew)
	}
}

// Claude Code applies NO charset to a command name — its name is the whole
// whitespace-delimited token — so a contributed file named `db.migrate.md`
// resolves there and was permanently unreachable here. A dot inside a path
// COMPONENT is an ordinary file name, not traversal.
//
// Mutation: drop '.' from the charset and the parity case stops resolving;
// drop the all-dots component check and the traversal spellings come back.
func TestNamesWithDotsResolveWhileDotComponentsStayRefused(t *testing.T) {
	ws := t.TempDir()
	writeCommand(t, ws, "db.migrate.md", "Run the migration.\n")
	writeCommand(t, ws, filepath.Join("ops", "deploy.v2.md"), "Deploy v2.\n")

	for _, tc := range []struct{ prompt, name, body string }{
		{"/db.migrate", "db.migrate", "Run the migration."},
		{"/ops:deploy.v2", "ops:deploy.v2", "Deploy v2."},
	} {
		name, _, ok := ParseInvocation(tc.prompt)
		if !ok || name != tc.name {
			t.Fatalf("ParseInvocation(%q) = (%q, %v), want %q", tc.prompt, name, ok, tc.name)
		}
		cmd, found, err := LookupWorkspace(ws, name, 0)
		if !found || err != nil {
			t.Fatalf("LookupWorkspace(%q) = (%v, %v), want it to resolve", name, found, err)
		}
		if cmd.Body != tc.body {
			t.Errorf("LookupWorkspace(%q).Body = %q, want %q", name, cmd.Body, tc.body)
		}
	}

	// The refusals a dot must NOT buy back.
	for _, name := range []string{"..", ".", "a:..:b", "..:x", "x:.."} {
		if _, _, ok := ParseInvocation("/" + name); ok {
			t.Errorf("ParseInvocation(%q) parsed, want prose", "/"+name)
		}
		if _, found, err := LookupWorkspace(ws, name, 0); found || err == nil {
			t.Errorf("LookupWorkspace(%q) = (%v, %v), want an error", name, found, err)
		}
	}
}

// `allowed-tools:` is HONORED — parsed into AllowedTools for the embedder to
// enforce — so it leaves the discarded list; what this parser still throws
// away is surfaced, so an embedder can say what it dropped instead of
// letting a command that configures itself apply in silence.
//
// Mutation: return nil from discardedExcept, or stop collecting the keys —
// the embedder's warning has nothing to name and this reddens. Drop
// "allowed-tools" from the honored set and a narrowed command expands
// unrestricted and this reddens.
func TestLookupWorkspaceSurfacesTheFrontmatterItDiscards(t *testing.T) {
	ws := t.TempDir()
	writeCommand(t, ws, "narrow.md",
		"---\ndescription: a narrowed command\nallowed-tools: Read, Grep\nmodel: claude-3-5-haiku\ndisable-model-invocation: true\n---\nDo the thing.\n")

	cmd, ok, err := LookupWorkspace(ws, "narrow", 0)
	if !ok || err != nil {
		t.Fatalf("LookupWorkspace = (%v, %v)", ok, err)
	}
	want := []string{"disable-model-invocation", "model"}
	if !slices.Equal(cmd.DiscardedFrontmatter, want) {
		t.Errorf("DiscardedFrontmatter = %v, want %v", cmd.DiscardedFrontmatter, want)
	}
	if !slices.Equal(cmd.AllowedTools, []string{"Read", "Grep"}) {
		t.Errorf("AllowedTools = %v, want [Read Grep]", cmd.AllowedTools)
	}
	// `description:` is consumed, not discarded, and must not be reported.
	if slices.Contains(cmd.DiscardedFrontmatter, "description") {
		t.Error("description is acted on, so it is not discarded")
	}
	if cmd.Description != "a narrowed command" {
		t.Errorf("Description = %q", cmd.Description)
	}
	if cmd.Body != "Do the thing." {
		t.Errorf("Body = %q, want the frontmatter stripped", cmd.Body)
	}

	// A command with only a description discards nothing: no warning is owed.
	writeCommand(t, ws, "plain.md", "---\ndescription: plain\n---\nbody\n")
	plain, _, _ := LookupWorkspace(ws, "plain", 0)
	if len(plain.DiscardedFrontmatter) != 0 {
		t.Errorf("DiscardedFrontmatter = %v for a description-only header, want none", plain.DiscardedFrontmatter)
	}
}

// A DECLARED `allowed-tools:` is non-nil even when its list parses empty:
// `allowed-tools: []` narrows to nothing, an absent key narrows nothing
// away — conflating the two lets a file that says "no tools" silently mean
// "all tools" (iterion#1717).
func TestLookupWorkspaceAllowedToolsDeclaredEmptyIsNotAbsent(t *testing.T) {
	ws := t.TempDir()
	writeCommand(t, ws, "sealed.md", "---\ndescription: sealed\nallowed-tools: []\n---\nDo nothing.\n")
	writeCommand(t, ws, "bare.md", "---\ndescription: bare\nallowed-tools:\n---\nDo nothing.\n")
	writeCommand(t, ws, "open.md", "---\ndescription: open\n---\nDo anything.\n")

	sealed, _, err := LookupWorkspace(ws, "sealed", 0)
	if err != nil {
		t.Fatalf("sealed: %v", err)
	}
	if sealed.AllowedTools == nil || len(sealed.AllowedTools) != 0 {
		t.Errorf("sealed AllowedTools = %v, want a declared EMPTY list (non-nil)", sealed.AllowedTools)
	}
	bare, _, err := LookupWorkspace(ws, "bare", 0)
	if err != nil {
		t.Fatalf("bare: %v", err)
	}
	if bare.AllowedTools == nil || len(bare.AllowedTools) != 0 {
		t.Errorf("bare AllowedTools = %v, want a declared EMPTY list (non-nil)", bare.AllowedTools)
	}
	open, _, err := LookupWorkspace(ws, "open", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if open.AllowedTools != nil {
		t.Errorf("open AllowedTools = %v, want nil — no restriction declared", open.AllowedTools)
	}
}

// The YAML block-list shape (`allowed-tools:` then indented `- entry`
// lines) is the third form the reference accepts: it parses to the declared
// list, not to a declared-empty one.
func TestLookupWorkspaceAllowedToolsBlockList(t *testing.T) {
	ws := t.TempDir()
	writeCommand(t, ws, "block.md", "---\ndescription: blocked\nallowed-tools:\n  - Read\n  - Bash(git:*)\nmodel: haiku\n---\nDo the thing.\n")

	cmd, ok, err := LookupWorkspace(ws, "block", 0)
	if !ok || err != nil {
		t.Fatalf("LookupWorkspace = (%v, %v)", ok, err)
	}
	if !slices.Equal(cmd.AllowedTools, []string{"Read", "Bash(git:*)"}) {
		t.Errorf("AllowedTools = %v, want [Read Bash(git:*)]", cmd.AllowedTools)
	}
	// The following key is not swallowed by the lookahead.
	if !slices.Contains(cmd.DiscardedFrontmatter, "model") {
		t.Errorf("DiscardedFrontmatter = %v, want model named", cmd.DiscardedFrontmatter)
	}
	if cmd.Body != "Do the thing." {
		t.Errorf("Body = %q, want the frontmatter stripped", cmd.Body)
	}
}
