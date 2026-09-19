// Package discovery measures what a run spent on ORIENTATION — the
// reads, searches and listings an agent performs to find its way around
// a repository — as opposed to what it spent on the change itself.
//
// The unit of attribution is the NODE, not the turn. A node's
// authoritative token spend is recorded once, at node end, on its turn
// checkpoints (pkg/store.TurnCheckpoint.Usage); the intra-node split
// between "still looking" and "now writing" is NOT reported by either
// backend path, so this package never invents it. What it does instead:
//
//   - classify every tool call from the event stream, so the shape of a
//     node's activity is measurable on any run that has one;
//   - name the nodes that never mutated anything — for those, and only
//     those, the whole token spend IS discovery cost, with no imputation;
//   - report coverage explicitly, so a thin corpus reads as thin rather
//     than as a confident number.
package discovery

import (
	"encoding/json"
	"path"
	"strings"
)

// Class is what a single tool call was doing. Unknown is a first-class
// outcome, never folded into one of the others: the classifier's reach
// is part of what the report has to disclose.
type Class string

const (
	ClassDiscovery Class = "discovery" // read, search, list, fetch — pulls context in
	ClassMutation  Class = "mutation"  // writes a file, a commit, a remote
	ClassOther     Class = "other"     // builds, tests, bookkeeping — neither
	ClassUnknown   Class = "unknown"   // the table has no entry, or the input was unreadable
)

// toolClass maps a NORMALISED tool name (see normaliseTool) to its class.
// Tool names differ per backend — claude_code capitalises (`Read`), claw
// does not (`read`), MCP prefixes with `mcp__<server>__` — so the table is
// keyed on the normalised form and one entry covers every backend.
//
// Shell-shaped tools are deliberately absent: they resolve through the
// command they carry, because `bash` is whatever its command says.
//
// The table is a declared heuristic, not a guarantee. Its blind spots are
// meant to surface as ClassUnknown in the report, never to be absorbed
// into a neighbouring class: a name nobody listed is a name nobody
// measured.
var toolClass = map[string]Class{
	// Pulling context in.
	"read":             ClassDiscovery,
	"read_file":        ClassDiscovery,
	"glob":             ClassDiscovery,
	"grep":             ClassDiscovery,
	"ls":               ClassDiscovery,
	"list_files":       ClassDiscovery,
	"workspace_grep":   ClassDiscovery,
	"notebookread":     ClassDiscovery,
	"websearch":        ClassDiscovery,
	"web_search":       ClassDiscovery,
	"webfetch":         ClassDiscovery,
	"web_fetch":        ClassDiscovery,
	"toolsearch":       ClassDiscovery,
	"skill":            ClassDiscovery,
	"memory_read":      ClassDiscovery,
	"memory_list":      ClassDiscovery,
	"browser_snapshot": ClassDiscovery,
	"browser_find":     ClassDiscovery,

	// Changing something.
	"write":          ClassMutation,
	"edit":           ClassMutation,
	"multiedit":      ClassMutation,
	"notebookedit":   ClassMutation,
	"write_file":     ClassMutation,
	"apply_patch":    ClassMutation,
	"memory_write":   ClassMutation,
	"create_issue":   ClassMutation,
	"update_issue":   ClassMutation,
	"comment_issue":  ClassMutation,
	"browser_click":  ClassMutation,
	"browser_type":   ClassMutation,
	"browser_fill":   ClassMutation,
	"browser_upload": ClassMutation,

	// Neither: bookkeeping, structure, delegation.
	"todowrite":         ClassOther,
	"todo_write":        ClassOther,
	"structuredoutput":  ClassOther,
	"structured_output": ClassOther,
	"agent":             ClassOther,
	"task":              ClassOther,
	"askuserquestion":   ClassOther,
	"ask_user":          ClassOther,
	"exitplanmode":      ClassOther,
	"browser_navigate":  ClassOther,
}

// shellVerbClass maps a shell command's VERB — one word, or two when the
// first is a multiplexer whose subcommand decides (`git log` vs
// `git commit`) — to its class.
//
// Only forms that are unambiguous in their bare shape are listed. A verb
// whose class depends on a flag nobody read (`git branch` lists or
// deletes; `gh api` reads or posts) is deliberately absent: it lands in
// ClassUnknown, which the report shows, rather than in a class it only
// sometimes deserves.
var shellVerbClass = map[string]Class{
	// Reading the tree.
	"ls": ClassDiscovery, "cat": ClassDiscovery, "head": ClassDiscovery,
	"tail": ClassDiscovery, "grep": ClassDiscovery, "rg": ClassDiscovery,
	"find": ClassDiscovery, "fd": ClassDiscovery, "tree": ClassDiscovery,
	"wc": ClassDiscovery, "file": ClassDiscovery, "stat": ClassDiscovery,
	"du": ClassDiscovery, "which": ClassDiscovery, "awk": ClassDiscovery,
	"sed": ClassDiscovery, "jq": ClassDiscovery, "yq": ClassDiscovery,
	"diff": ClassDiscovery, "basename": ClassDiscovery, "dirname": ClassDiscovery,
	"realpath": ClassDiscovery,
	"git log":  ClassDiscovery, "git status": ClassDiscovery, "git diff": ClassDiscovery,
	"git show": ClassDiscovery, "git blame": ClassDiscovery, "git ls-files": ClassDiscovery,
	"git grep": ClassDiscovery, "git ls-tree": ClassDiscovery, "git cat-file": ClassDiscovery,
	"git describe": ClassDiscovery, "git rev-parse": ClassDiscovery, "git rev-list": ClassDiscovery,
	"go doc": ClassDiscovery, "go list": ClassDiscovery, "go env": ClassDiscovery,

	// Changing the tree, the history, or a remote.
	"mv": ClassMutation, "rm": ClassMutation, "cp": ClassMutation,
	"mkdir": ClassMutation, "touch": ClassMutation, "tee": ClassMutation,
	"chmod": ClassMutation, "ln": ClassMutation, "patch": ClassMutation,
	"git add": ClassMutation, "git commit": ClassMutation, "git push": ClassMutation,
	"git checkout": ClassMutation, "git merge": ClassMutation, "git rebase": ClassMutation,
	"git reset": ClassMutation, "git apply": ClassMutation, "git restore": ClassMutation,
	"git stash": ClassMutation, "git switch": ClassMutation,

	// Neither.
	"go build": ClassOther, "go test": ClassOther, "go vet": ClassOther,
	"go run": ClassOther, "go mod": ClassOther, "gofmt": ClassOther,
	"task": ClassOther, "make": ClassOther, "devbox": ClassOther,
	"npm": ClassOther, "pnpm": ClassOther, "yarn": ClassOther,
	"docker": ClassOther, "kubectl": ClassOther, "helm": ClassOther,
	"echo": ClassOther, "printf": ClassOther, "true": ClassOther, "sleep": ClassOther,
	"export": ClassOther, "cd": ClassOther, "set": ClassOther,
}

// shellTools are the tools whose class is decided by their command, not
// their name. iterion's own tool nodes are named `shell:<node>` and
// `script:<lang>:<node>`, so they are matched by prefix in normaliseTool.
var shellTools = map[string]bool{
	"bash": true, "shell": true, "sh": true, "zsh": true,
	"diagnostic_shell": true, "run_command": true, "execute_command": true,
	"terminal": true,
}

// multiplexers are commands whose subcommand decides the class.
var multiplexers = map[string]bool{"git": true, "go": true}

// wrappers run another command and take its class. Classifying the
// wrapper instead of what it wraps is how `rtk git commit` — this repo's
// own token-efficient proxy — reads as a search: measured on 106 calls
// in the operator's store before this rule existed.
var wrappers = map[string]bool{
	"rtk": true, "sudo": true, "env": true, "nice": true, "time": true,
	"nohup": true, "stdbuf": true, "command": true, "timeout": true,
	"xargs": true, "ionice": true,
}

// wrapperSubcommand names the word some wrappers put between themselves
// and the command they run (`devbox run -- go test`, `rtk proxy git log`).
// Required means the bare command is NOT a wrapper: `devbox install` is
// devbox's own, while `rtk git log` wraps with no subcommand at all.
var wrapperSubcommand = map[string]struct {
	word     string
	required bool
}{
	"devbox": {"run", true},
	"rtk":    {"proxy", false},
}

// streamEditors rewrite their input in place when handed -i. Bare, they
// read; with the flag, they write — so the flag decides, not the name.
var streamEditors = map[string]bool{"sed": true, "perl": true, "ruby": true}

// Classify returns the class of one tool call. rawInput is the tool's
// JSON input as the event stream recorded it (`data.input`). An empty or
// unparseable input on a shell-shaped tool yields ClassUnknown: a shell
// call whose command we cannot read is not evidence of anything.
// UnnamedVerb returns the verb of the FIRST segment a chain could not
// name — the entry the table is actually missing.
//
// ShellVerb answers with the chain's head, which for `grep -q x && <a
// verb nobody knows>` is `grep`: a verb the table already names, listed
// as its own to-do. The chain's textual order is the command's own, so
// "first" here is a fact about the line and not about an arrival order.
// Empty for a tool that is not shell-shaped, or when no segment is the
// unnamed one.
func UnnamedVerb(toolName string, rawInput []byte) string {
	if !shellTools[normaliseTool(toolName)] {
		return ""
	}
	for _, seg := range splitSegments(stripHeredocBodies(shellCommand(rawInput))) {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		if classifySegment(seg) == ClassUnknown {
			return verbOf(seg)
		}
	}
	return ""
}

func Classify(toolName string, rawInput []byte) Class {
	name := normaliseTool(toolName)
	if shellTools[name] {
		return classifyCommand(shellCommand(rawInput))
	}
	if c, ok := toolClass[name]; ok {
		return c
	}
	return ClassUnknown
}

// classifyCommand resolves a full command line.
//
// A command line is a CHAIN, not a verb: `grep -q TODO f && git add f`
// reads as a search if only its head is looked at, and 229 such lines in
// the operator's store were counted as orientation while they committed.
// So the line is split into segments and EVERY segment is classified.
//
// Precedence between the segments' verdicts is mutation > unknown >
// discovery > other. Unknown outranks discovery on purpose: a chain with
// one segment nobody can name is a chain nobody can name, and the report
// publishes that share rather than absorbing it.
func classifyCommand(cmd string) Class {
	if cmd == "" {
		return ClassUnknown
	}
	// A heredoc body is DATA, not command text. Scanning it finds `>` in
	// a python comparison and calls the whole call a file write.
	cmd = stripHeredocBodies(cmd)
	best := ClassOther
	sawSegment := false
	for _, seg := range splitSegments(cmd) {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		sawSegment = true
		switch c := classifySegment(seg); c {
		case ClassMutation:
			return ClassMutation
		case ClassUnknown:
			best = ClassUnknown
		case ClassDiscovery:
			if best != ClassUnknown {
				best = ClassDiscovery
			}
		}
	}
	if !sawSegment {
		return ClassUnknown
	}
	return best
}

// classifySegment resolves ONE command of a chain. Two command-level
// rules run before the verb table, because each makes a command write
// whatever its verb is:
//
//  1. a redirection into a file (`> out`, `>> out`) is a write;
//  2. a stream editor handed -i rewrites its input.
//
// These are rules, not spellings: they hold for any verb, listed or not.
func classifySegment(seg string) Class {
	if writesViaRedirect(seg) {
		return ClassMutation
	}
	verb := verbOf(seg)
	if verb == "" {
		return ClassUnknown
	}
	if head, _, _ := strings.Cut(verb, " "); streamEditors[head] && hasInPlaceFlag(seg) {
		return ClassMutation
	}
	if c, ok := shellVerbClass[verb]; ok {
		return c
	}
	// An unlisted subcommand of a known multiplexer stays unknown rather
	// than inheriting the multiplexer's class: `git log` and `git push`
	// are not the same measurement.
	return ClassUnknown
}

// splitSegments cuts a command line on the operators that separate
// commands — `;`, `&&`, `||`, `|`, newline — while respecting quotes, so
// `echo "a; rm -rf x"` stays one segment and does not read as a delete.
//
// Known limit, disclosed rather than papered over: a command substitution
// (`$(…)`) is left inside its segment, so a mutation hidden there is only
// seen if it is also the segment's verb.
func splitSegments(cmd string) []string {
	var segments []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' && i+1 < len(cmd) {
				cur.WriteByte(c)
				i++
				cur.WriteByte(cmd[i])
				continue
			}
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == ';' || c == '\n' || c == '|':
			segments = append(segments, cur.String())
			cur.Reset()
			if c == '|' && i+1 < len(cmd) && cmd[i+1] == '|' {
				i++
			}
		case c == '&' && i+1 < len(cmd) && cmd[i+1] == '&':
			// Only `&&` separates commands. A lone `&` is a descriptor
			// dup (`2>&1`) or a background marker, and cutting there
			// turned `go test ./... 2>&1` into a segment called "1".
			segments = append(segments, cur.String())
			cur.Reset()
			i++
		default:
			cur.WriteByte(c)
		}
	}
	return append(segments, cur.String())
}

// unquoted returns the segment with every quoted span blanked out, so a
// scanner looking for shell operators cannot read one out of a pattern:
// `grep -rn 'a -> b'` and `awk '$3 > 5 {print}'` carry no redirection.
func unquoted(seg string) string {
	out := []byte(seg)
	var quote byte
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			out[i] = ' '
		case c == '\'' || c == '"':
			quote = c
			out[i] = ' '
		}
	}
	return string(out)
}

// normaliseTool folds a backend's spelling into the table's key space:
// lowercase, MCP server prefix removed (`mcp__playwright__browser_click`
// → `browser_click`), and iterion's own `shell:<node>` / `script:py:<node>`
// tool-node names reduced to their family.
func normaliseTool(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(n, "mcp__") {
		// mcp__<server>__<tool> — keep the tool, drop the server, since
		// the same tool is reachable through several server aliases.
		if i := strings.LastIndex(n, "__"); i >= 0 && i+2 <= len(n) {
			n = n[i+2:]
		}
	}
	if strings.HasPrefix(n, "shell:") || strings.HasPrefix(n, "script:") {
		return "shell"
	}
	return n
}

// shellCommand pulls the command line out of a shell tool's JSON input.
// Backends name the field differently; the first one present wins.
func shellCommand(rawInput []byte) string {
	if len(rawInput) == 0 {
		return ""
	}
	var in map[string]any
	if err := json.Unmarshal(rawInput, &in); err != nil {
		return ""
	}
	for _, key := range []string{"command", "script", "cmd", "code"} {
		if s, ok := in[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// ShellVerb extracts the classifying verb from a shell tool's JSON input:
// one word, or two when the first is a multiplexer. An empty string means
// "no command could be read" — never a default verb.
//
// Only the verb ever leaves this function: arguments carry paths, URLs
// and sometimes secrets, and none of them are returned or persisted.
func ShellVerb(rawInput []byte) string { return verbOf(shellCommand(rawInput)) }

// verbOf reduces a command line to its classifying verb. It walks past
// leading environment assignments (`FOO=bar cmd`), strips a directory
// (`/usr/bin/git` → `git`), and appends the subcommand for the handful of
// multiplexers whose class depends on it.
func verbOf(cmd string) string {
	fields := strings.Fields(cmd)
	// Peel transparent wrappers until a real command surfaces. The bound
	// is a guard against a pathological line, not a semantic limit.
	for depth := 0; depth < 4; depth++ {
		for len(fields) > 0 && isEnvAssignment(fields[0]) {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			return ""
		}
		head := strings.ToLower(path.Base(cleanWord(fields[0])))
		if head == "" {
			return ""
		}
		if rest, wrapped := unwrap(head, fields); wrapped {
			fields = rest
			continue
		}
		if !multiplexers[head] {
			return head
		}
		for _, f := range fields[1:] {
			if word := cleanWord(f); isSubcommandWord(word) {
				return head + " " + strings.ToLower(word)
			}
		}
		return head
	}
	return ""
}

// unwrap strips a transparent wrapper and returns the command it runs.
// The second result is false when head is not a wrapper, so the caller
// classifies head itself.
func unwrap(head string, fields []string) ([]string, bool) {
	sub, hasSub := wrapperSubcommand[head]
	next := ""
	if len(fields) > 1 {
		next = strings.ToLower(cleanWord(fields[1]))
	}
	switch {
	case hasSub && next == sub.word:
		return dropLeadingSeparators(fields[2:]), true
	case hasSub && sub.required:
		return nil, false // the bare command is not a wrapper
	case !wrappers[head]:
		return nil, false
	}
	rest := fields[1:]
	// A wrapper's own flags and numeric arguments (`timeout 30`,
	// `nice -n 5`) precede the command it runs.
	for len(rest) > 0 {
		f := strings.Trim(rest[0], `"'`)
		if strings.HasPrefix(f, "-") || isNumeric(f) || isEnvAssignment(f) {
			rest = rest[1:]
			continue
		}
		break
	}
	return dropLeadingSeparators(rest), true
}

func dropLeadingSeparators(fields []string) []string {
	for len(fields) > 0 && strings.Trim(fields[0], `"'`) == "--" {
		fields = fields[1:]
	}
	return fields
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' && r != 's' && r != 'm' {
			return false
		}
	}
	return true
}

// cleanWord strips the quoting and the shell punctuation a word carries
// when it sits next to a separator: `pwd;` and `ls &&` are `pwd` and `ls`,
// not verbs of their own.
func cleanWord(w string) string {
	return strings.Trim(w, "\"'`;&|()")
}

// isSubcommandWord reports whether a word can be a multiplexer's
// subcommand. A subcommand is a plain identifier: it rules out flags
// (`-C`), the VALUES those flags take (`/tmp/wt`, `user.name=x`) and
// path arguments, without enumerating which flags take a value.
func isSubcommandWord(w string) bool {
	if w == "" || strings.HasPrefix(w, "-") {
		return false
	}
	return !strings.ContainsAny(w, "/=.")
}

// isEnvAssignment reports whether a leading field is `NAME=value` rather
// than the command itself.
func isEnvAssignment(field string) bool {
	name, _, found := strings.Cut(field, "=")
	if !found || name == "" || strings.HasPrefix(field, "-") {
		return false
	}
	for _, r := range name {
		if !isIdentifierRune(r) {
			return false
		}
	}
	return true
}

func isIdentifierRune(r rune) bool {
	return r == '_' ||
		(r >= 'A' && r <= 'Z') ||
		(r >= 'a' && r <= 'z') ||
		(r >= '0' && r <= '9')
}

// writesViaRedirect reports whether a command redirects into a file.
// `2>&1` (a descriptor dup) and `>/dev/null` (the bit bucket) are not
// writes anyone measures; everything else that follows `>` or `>>` is a
// path the command creates or truncates.
//
// The scan walks the segment itself, tracking quotes as it goes, rather
// than scanning its `unquoted()` form: that form blanks the quoted span,
// so a quoted TARGET (`cat > "/home/jo/notes.md"`) left nothing to read
// and a file write classified as a read. Reading the target here also
// keeps `cmd > "path" 2>&1` honest — scanning the blanked form skipped
// past the target and took `2>&1` for it, reaching the right verdict by
// the wrong route.
func writesViaRedirect(seg string) bool {
	var quote byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c != '>' {
			continue
		}
		// `->`, `>=`, `<…>`: an operator or a template, not a redirection.
		if i > 0 && (seg[i-1] == '-' || seg[i-1] == '=' || seg[i-1] == '<') {
			continue
		}
		j := i + 1
		for j < len(seg) && seg[j] == '>' {
			j++
		}
		if j < len(seg) && seg[j] == '=' {
			continue // `>=`
		}
		for j < len(seg) && (seg[j] == ' ' || seg[j] == '\t') {
			j++
		}
		if j < len(seg) && seg[j] == '&' {
			continue // a descriptor dup: `>&1`, `2>&1`
		}
		target := readWord(seg[j:])
		if target == "" {
			continue // a trailing `>` with nothing after it
		}
		// cleanWord, not a quote strip: `2>/dev/null; echo x` yields the
		// target `/dev/null;`, which compared unequal to /dev/null and
		// turned 219 reads in the operator's store into writes.
		if cleanWord(target) != "/dev/null" {
			return true
		}
	}
	return false
}

// stripHeredocBodies removes every heredoc's DATA while keeping the
// command text around it. Cutting the line at the first `<<` instead
// threw away the terminator and everything past it, so 23 chained writes
// in the operator's store — `python3 - <<'PY' … PY` scripts followed by
// the command that did the writing — read as unnamed.
//
// The opener token goes with its body: left in place it would be found
// again on the next pass, and `<<TERM` is not command text either.
func stripHeredocBodies(cmd string) string {
	var b strings.Builder
	i := 0
	for i < len(cmd) {
		j := nextHeredocOpener(cmd, i)
		if j < 0 {
			b.WriteString(cmd[i:])
			break
		}
		b.WriteString(cmd[i:j])
		delim, afterOpener := readHeredocDelim(cmd, j)
		if delim == "" {
			b.WriteString(cmd[j : j+2]) // a bare `<<`: nothing to follow
			i = j + 2
			continue
		}
		nl := strings.IndexByte(cmd[afterOpener:], '\n')
		if nl < 0 {
			b.WriteString(cmd[afterOpener:]) // the opener ends the text
			break
		}
		// The rest of the opener's own line is command text: `cat <<EOF > f`
		// redirects, and the redirect sits after the delimiter.
		b.WriteString(cmd[afterOpener : afterOpener+nl+1])
		i = skipHeredocBody(cmd, afterOpener+nl+1, delim)
	}
	return b.String()
}

// nextHeredocOpener returns the index of the next `<<` that opens a
// heredoc, stepping over `<<<` — a here-string, whose operand is a word
// on the same line and not a body.
func nextHeredocOpener(s string, from int) int {
	for i := from; i+1 < len(s); i++ {
		if s[i] != '<' || s[i+1] != '<' {
			continue
		}
		if i+2 < len(s) && s[i+2] == '<' {
			i += 2
			continue
		}
		return i
	}
	return -1
}

// readHeredocDelim reads the delimiter of the opener at i and returns it
// with the index just past it. `<<-`, `<<'EOF'` and `<<"EOF"` name the
// same delimiter; the quoting decides whether the body is expanded, which
// is no business of a classifier.
func readHeredocDelim(s string, i int) (string, int) {
	j := i + 2
	if j < len(s) && s[j] == '-' {
		j++
	}
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	start := j
	var quote byte
	var b strings.Builder
	for ; j < len(s); j++ {
		c := s[j]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			b.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case ' ', '\t', '\n', ';', '|', '&', '>', '<':
			if b.Len() == 0 {
				return "", start
			}
			return b.String(), j
		default:
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return "", start
	}
	return b.String(), j
}

// skipHeredocBody returns the index just past the line that terminates
// the body, or the end of the text when the terminator never comes.
func skipHeredocBody(s string, start int, delim string) int {
	for i := start; i < len(s); {
		var line string
		var next int
		if end := strings.IndexByte(s[i:], '\n'); end < 0 {
			line, next = s[i:], len(s)
		} else {
			line, next = s[i:i+end], i+end+1
		}
		if strings.TrimSpace(line) == delim {
			return next
		}
		i = next
	}
	return len(s)
}

// readWord reads one shell word, honouring quotes and returning it with
// the quotes removed. A quoted word may contain spaces, which is why the
// redirect scan cannot reach for `strings.Fields`.
func readWord(s string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
				continue
			}
			b.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t' || c == '\n':
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// hasInPlaceFlag reports whether a stream editor was handed -i, in any of
// its spellings (`-i`, `-i.bak`, `-i”`, or inside a bundle like `-ne -i`).
func hasInPlaceFlag(seg string) bool {
	for _, f := range strings.Fields(unquoted(seg)) {
		if f == "--in-place" || strings.HasPrefix(f, "--in-place=") {
			return true
		}
		if len(f) < 2 || f[0] != '-' || f[1] == '-' {
			continue
		}
		// A short-option bundle: -i, -i.bak, -pi, -ne. The text after the
		// i is a backup extension, not another flag, so the scan stops at
		// the first non-letter.
		for j := 1; j < len(f); j++ {
			if f[j] == 'i' {
				return true
			}
			if !isLetter(f[j]) {
				break
			}
		}
	}
	return false
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }
