package supervise

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ClaudeSession identifies a raw Claude Code CLI/VSCode session to
// supervise: its working directory (which yields the project key Claude
// Code uses for ~/.claude/projects/<key>/) and its session id (the
// transcript file stem). Either is enough to resolve the other.
type ClaudeSession struct {
	Cwd            string // absolute working directory of the session
	ProjectKey     string // store.EncodeWorkDirKey(Cwd) — Claude Code's project dir name
	SessionID      string // transcript stem (<sessionId>.jsonl); "" until resolved
	TranscriptPath string // resolved <key>/<sessionId>.jsonl
}

// ClaudeProjectKey encodes an absolute working directory the way Claude
// Code names its ~/.claude/projects/<key>/ transcript directory: every
// character that is not [A-Za-z0-9] becomes '-' (so '/', '.', '_', '+',
// ':' all map to '-'; case is preserved; no run-collapsing). This is
// NOT the same as store.EncodeWorkDirKey, which only maps '/', ':', '\'
// and preserves '.' — a mismatch that left the supervisor looking in the
// wrong project dir for any path containing a dot (e.g. a tmp dir, or any
// `/.claude/` segment). Verified against real project dirs, e.g.
// "/tmp/x.Y" -> "-tmp-x-Y" and "/home/jo/.iterion" -> "-home-jo--iterion".
func ClaudeProjectKey(absPath string) string {
	var b strings.Builder
	b.Grow(len(absPath))
	for _, r := range absPath {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// claudeProjectsDir returns ~/.claude/projects (honouring $CLAUDE_HOME /
// $HOME), the root Claude Code writes session transcripts under.
func claudeProjectsDir() string {
	if dir := strings.TrimRight(os.Getenv("CLAUDE_CONFIG_DIR"), string(filepath.Separator)); dir != "" {
		return filepath.Join(dir, "projects")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".claude", "projects")
	}
	return filepath.Join(os.TempDir(), ".claude", "projects")
}

// claudeSessionInboxRoot is the iterion-owned inbox tree for raw Claude
// Code sessions: ~/.iterion/claude-sessions/<project-key>. The
// supervisor writes here; the `iterion __claude-hook-drain` hook reads
// here. Keyed by project so the supervisor and the in-repo hook agree on
// the path from cwd alone.
func claudeSessionInboxRoot(projectKey string) string {
	return filepath.Join(store.GlobalIterionDataDir(), "claude-sessions", projectKey)
}

// ResolveClaudeSession turns a --claude-session argument into a fully
// resolved ClaudeSession. The arg may be:
//   - a directory path (the session cwd) → newest active transcript in it
//   - a session id (UUID-ish) → scanned for across project dirs
//   - empty → use cwd
func ResolveClaudeSession(arg, cwd string) (*ClaudeSession, error) {
	// Session-id form: a value that isn't an existing path and looks like
	// a transcript stem. Locate its project dir by scanning.
	if arg != "" && !isExistingDir(arg) {
		if sess, ok := findSessionByID(arg); ok {
			return sess, nil
		}
		// Not found as an id; fall through treating it as a (maybe
		// not-yet-existing) cwd.
	}

	dir := cwd
	if isExistingDir(arg) {
		dir = arg
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("supervise: resolve cwd %q: %w", dir, err)
	}
	key := ClaudeProjectKey(abs)
	sess := &ClaudeSession{Cwd: abs, ProjectKey: key}
	if id, path, ok := newestTranscript(filepath.Join(claudeProjectsDir(), key)); ok {
		sess.SessionID = id
		sess.TranscriptPath = path
	}
	return sess, nil
}

func isExistingDir(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// newestTranscript returns the most-recently-modified <sessionId>.jsonl
// directly under projectDir (the active session), ignoring the subagents
// subdir.
func newestTranscript(projectDir string) (id, path string, ok bool) {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return "", "", false
	}
	type cand struct {
		id   string
		path string
		mod  int64
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, cand{
			id:   strings.TrimSuffix(e.Name(), ".jsonl"),
			path: filepath.Join(projectDir, e.Name()),
			mod:  info.ModTime().UnixNano(),
		})
	}
	if len(cands) == 0 {
		return "", "", false
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod > cands[j].mod })
	return cands[0].id, cands[0].path, true
}

// findSessionByID scans every project dir for <id>.jsonl, recovering the
// project key (and thus cwd-key) for a session given only its id.
func findSessionByID(id string) (*ClaudeSession, bool) {
	root := claudeProjectsDir()
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, false
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		path := filepath.Join(root, p.Name(), id+".jsonl")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return &ClaudeSession{
				ProjectKey:     p.Name(),
				SessionID:      id,
				TranscriptPath: path,
			}, true
		}
	}
	return nil, false
}
