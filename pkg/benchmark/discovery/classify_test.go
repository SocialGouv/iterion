package discovery

import (
	"strings"
	"testing"
)

// TestQuotingAnArgumentNeverChangesItsClass is the guarantee behind the
// redirect scan rather than one more spelling in a list. A path can be
// written bare, in single quotes or in double quotes; the shell runs the
// same command either way, so the classifier owes the same verdict.
//
// Enumerating spellings is what let the defect through twice: round 1
// taught the scan to ignore quotes, which blanked the target, so
// `cat > "/home/jo/notes.md"` classified as orientation. A property
// refuses the next spelling without anyone having to think of it.
func TestQuotingAnArgumentNeverChangesItsClass(t *testing.T) {
	templates := []string{
		"cat > PATH",
		"cat >> PATH",
		"cat > PATH 2>&1",
		"tee PATH",
		"grep -r TODO PATH",
		"rm -f PATH",
		"cp a.txt PATH",
		"go test ./... > PATH 2>&1",
		"sed -i 's/a/b/' PATH",
		"ls PATH",
		"git add PATH",
		"cat PATH | grep x",
		"echo hi > PATH && git add PATH",
		"grep -q TODO PATH && rm PATH",
	}
	paths := []string{"/home/jo/notes.md", "notes.md", "/dev/null", "a b.md"}
	styles := []struct {
		name string
		wrap func(string) string
	}{
		{"bare", func(s string) string { return s }},
		{"single", func(s string) string { return "'" + s + "'" }},
		{"double", func(s string) string { return `"` + s + `"` }},
	}

	for _, tpl := range templates {
		for _, p := range paths {
			var want Class
			var wantCmd, wantStyle string
			for _, st := range styles {
				// Bare, a path containing a space is two words — a
				// different command, not a different spelling.
				if st.name == "bare" && strings.ContainsAny(p, " \t") {
					continue
				}
				cmd := strings.ReplaceAll(tpl, "PATH", st.wrap(p))
				got := classifyCommand(cmd)
				if wantStyle == "" {
					want, wantCmd, wantStyle = got, cmd, st.name
					continue
				}
				if got != want {
					t.Errorf("quoting changed the class:\n  %-6s %q -> %s\n  %-6s %q -> %s",
						wantStyle, wantCmd, want, st.name, cmd, got)
				}
			}
		}
	}
}

// TestAHeredocKeepsTheCommandsAfterItsTerminator pins both halves of the
// heredoc rule: the body is data and must not be read as command text,
// and everything past the terminator is command text and must not be
// thrown away with it.
func TestAHeredocKeepsTheCommandsAfterItsTerminator(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want Class
	}{
		{
			"a write chained after the terminator still counts",
			"python3 - <<'PY'\nprint(1)\nPY\ngit add f",
			ClassMutation,
		},
		{
			"a redirect on the opener's own line still counts",
			"cat <<'EOF' > /tmp/out.txt\nhello\nEOF",
			ClassMutation,
		},
		{
			"a tab-stripping opener names the same delimiter",
			"cat <<-EOF\nbody\n\tEOF\ngit commit -m x",
			ClassMutation,
		},
		{
			"a here-string is not a heredoc and keeps its chain",
			"grep -q x <<< \"$v\" && git add f",
			ClassMutation,
		},
	}
	for _, c := range cases {
		if got := classifyCommand(c.cmd); got != c.want {
			t.Errorf("%s:\n  %q\n  got %s, want %s", c.name, c.cmd, got, c.want)
		}
	}
}

// TestAHeredocBodyIsDataNotCommandText is separate because it is the
// half that must hold when the other half is wrong: a `>` inside a
// python body is a comparison, and reading it as a redirect is what the
// blunt cut was there to prevent in the first place.
func TestAHeredocBodyIsDataNotCommandText(t *testing.T) {
	const cmd = "python3 - <<'PY'\nif a > b:\n    pass\nPY"
	if got := classifyCommand(cmd); got == ClassMutation {
		t.Errorf("a `>` inside a heredoc body read as a file write:\n  %q\n  got %s", cmd, got)
	}
}

// Each case below is chosen so exactly one table entry or one rule can
// make it fail: deleting `read` from toolClass reddens the first row and
// nothing else, and removing the redirect rule reddens only the redirect
// rows.
func TestClassifyToolNames(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  Class
	}{
		{"claude_code capitalises", "Read", `{"file_path":"a.go"}`, ClassDiscovery},
		{"claw lowercases the same tool", "read", `{"file_path":"a.go"}`, ClassDiscovery},
		{"an edit is a mutation either way", "Edit", `{}`, ClassMutation},
		{"claw edit", "edit", `{}`, ClassMutation},
		{"mcp prefix is stripped to the tool", "mcp__playwright__browser_click", `{}`, ClassMutation},
		{"mcp read-only tool", "mcp__playwright__browser_snapshot", `{}`, ClassDiscovery},
		{"bookkeeping is neither", "TodoWrite", `{}`, ClassOther},
		{"a tool nobody listed stays unknown", "SomeFutureTool", `{}`, ClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.tool, []byte(tc.input)); got != tc.want {
				t.Fatalf("Classify(%q) = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}

func TestClassifyShellCommands(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want Class
	}{
		{"a listing reads", "ls -la pkg/", ClassDiscovery},
		{"an absolute path is stripped", "/usr/bin/git status", ClassDiscovery},
		{"leading env assignments are walked past", "FOO=1 BAR=2 grep -rn x .", ClassDiscovery},
		{"a multiplexer takes its subcommand", "git commit -m 'x'", ClassMutation},
		{"a flag between the two words is skipped", "git -C /tmp/x log", ClassDiscovery},
		{"a redirect into a file writes, whatever the verb", "ls -la > /tmp/out.txt", ClassMutation},
		{"an append redirect writes", "echo hi >> notes.md", ClassMutation},
		{"a descriptor dup is not a write", "go test ./... 2>&1", ClassOther},
		{"the bit bucket is not a write", "grep -r x . >/dev/null", ClassDiscovery},
		{"a stream editor in place writes", "sed -i s/a/b/ f.go", ClassMutation},
		{"the same stream editor without the flag reads", "sed s/a/b/ f.go", ClassDiscovery},
		{"an unlisted subcommand of a known multiplexer stays unknown", "git branch -d old", ClassUnknown},
		{"an unlisted verb stays unknown", "frobnicate --all", ClassUnknown},
		{"a wrapper takes the class of what it wraps", "rtk git commit -m x", ClassMutation},
		{"the same wrapper over a search still reads", "rtk grep -rn needle .", ClassDiscovery},
		{"devbox run wraps", "devbox run -- git push origin main", ClassMutation},
		{"devbox on its own is not a wrapper", "devbox install", ClassOther},
		{"a wrapper's numeric argument is not the verb", "timeout 30 ls -la", ClassDiscovery},
		{"wrappers nest", "sudo env FOO=1 rm -rf /tmp/x", ClassMutation},
		{"a wrapper's own subcommand is not the verb", "rtk proxy git log --oneline", ClassDiscovery},
		// A chain is classified on EVERY segment, not on its head: these
		// six shapes all read as orientation while they wrote, 229 times
		// in the operator's own store.
		{"a write after && is still a write", "grep -q TODO main.go && git add main.go", ClassMutation},
		{"a write after || is still a write", "ls /tmp/x || mkdir -p /tmp/x", ClassMutation},
		{"a write after ; is still a write", "cat f.txt; rm f.txt", ClassMutation},
		{"a write after a pipe is still a write", "cat banner.txt | tee /etc/motd", ClassMutation},
		{"a write after cd is still a write", "cd /repo && git checkout -- .", ClassMutation},
		{"a quoted separator does not split a segment", `echo "a; rm -rf x"`, ClassOther},
		// The discarded-output idiom, with and without the punctuation
		// that made 219 reads in the store look like writes.
		{"the bit bucket followed by a separator is not a write", "ls x 2>/dev/null; echo done", ClassDiscovery},
		{"an unnamed segment outranks a named one", "ls && python3 script.py", ClassUnknown},
		// A `>` inside a quoted pattern is not a redirection.
		{"an arrow in a pattern is not a redirection", "grep -rn 'a -> b' pkg/", ClassDiscovery},
		{"a comparison in an awk program is not a redirection", "awk '$3 > 5 {print}' /tmp/x", ClassDiscovery},
		{"a heredoc body is data, not command text", "python3 - <<'EOF'\nif a > b: pass\nEOF", ClassUnknown},
		{"a bundled in-place flag writes", "perl -pi -e 's/a/b/' f.go", ClassMutation},
		{"trailing punctuation is not part of the verb", "ls; echo done", ClassDiscovery},
		{"nor part of a subcommand", "git status; echo done", ClassDiscovery},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(`{"command":` + quote(tc.cmd) + `}`)
			if got := Classify("Bash", in); got != tc.want {
				t.Fatalf("Classify(Bash, %q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

// A shell call whose command cannot be read is not evidence of anything.
// If this ever returns a class, every "unknown" percentage in the report
// becomes a lie by omission.
func TestUnreadableShellInputIsUnknown(t *testing.T) {
	for _, in := range []string{``, `not json`, `{}`, `{"file_path":"a.go"}`} {
		if got := Classify("bash", []byte(in)); got != ClassUnknown {
			t.Fatalf("Classify(bash, %q) = %q, want %q", in, got, ClassUnknown)
		}
	}
}

// iterion's own tool nodes are named `shell:<node>` / `script:<lang>:<node>`;
// they classify by their command like any other shell.
func TestIterionToolNodesClassifyByCommand(t *testing.T) {
	if got := Classify("shell:verify_run", []byte(`{"command":"go test ./..."}`)); got != ClassOther {
		t.Fatalf("shell: node = %q, want %q", got, ClassOther)
	}
	if got := Classify("script:py:load_pending", []byte(`{"script":"cat state.json"}`)); got != ClassDiscovery {
		t.Fatalf("script: node = %q, want %q", got, ClassDiscovery)
	}
}

func TestShellVerbReturnsOnlyTheVerb(t *testing.T) {
	in := []byte(`{"command":"curl -H 'Authorization: Bearer sekret' https://example.test/x"}`)
	if got := ShellVerb(in); got != "curl" {
		t.Fatalf("ShellVerb = %q, want %q (arguments must never escape)", got, "curl")
	}
}

// quote renders a Go string as a JSON string literal without pulling in
// encoding/json in the test's assertion path.
func quote(s string) string {
	out := []rune{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		default:
			out = append(out, r)
		}
	}
	return string(append(out, '"'))
}
