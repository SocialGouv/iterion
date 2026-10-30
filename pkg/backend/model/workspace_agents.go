package model

// File-backed named subagents (#1714): the `agent` tool's subagent_type may
// name `<workspace>/.claude/agents/<name>.md` — the files a plugin's
// `contributes: agents` mirrors into the workspace (engine_run.go's
// mirrorPluginContributions) and claude_code discovers natively through
// `--setting-sources project`. The resolver lives iterion-side by
// arbitration (ARB-83): NewSubagentRunner executes every spawn a workflow
// drives, for BOTH backends, while claw's own ConversationLoop — where a
// claw-side resolver would serve — is not the loop iterion pilots. A
// claw-code-go re-export for third-party embedders is the bump lane's call.
//
// The frontmatter is honoured where it NARROWS and refused where it spends:
//
//   - `description:` is read for the resolved agent's record;
//   - `tools:` INTERSECTS the node's effective tool set — an agent file
//     narrows its child, it never widens one past what the node holds;
//   - `model:` is REFUSED (ErrWorkspaceAgentModel): a contributed file does
//     not re-route a node's spend, and the value cannot pass claw-code-go
//     today (#1717). The model the child bills stays the node's, or the one
//     the delegating model passes as the agent tool's own argument.
//
// Containment is os.Root at the WORKSPACE root — rooting at the agents
// directory would let a repository move the root itself by shipping
// `.claude` or `.claude/agents` as a symlink. The name charset keeps a name
// from spelling traversal; the root is what makes escape a refusal rather
// than a convention.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// ErrWorkspaceAgentModel refuses a delegation whose resolved agent file
// declares `model:` in its frontmatter. Refused, not ignored: silently
// billing the node's model where the file named another is one file meaning
// two things on two backends, and honouring it would let a checkout under
// review pick the model a node spends on (#1717).
var ErrWorkspaceAgentModel = errors.New("subagent: a workspace agent's model: frontmatter is refused")

// ErrWorkspaceAgentTooLarge refuses an agent file past the byte ceiling. The
// read stops one byte past the bound, so the refusal never holds what it
// refuses — the file is untrusted input (a checkout under review) that turns
// straight into a billed system prompt.
var ErrWorkspaceAgentTooLarge = errors.New("subagent: workspace agent file exceeds the byte ceiling")

// WorkspaceAgentMaxBytesEnv overrides the ceiling on one agent file, in
// bytes. "0" removes the ceiling. A value a project `.env` planted carries
// no authority — workspaceAgentMaxBytes resolves the default instead — and
// does not cross into the sandbox: forwardableProviderEnv sends the
// operator's value or an explicit empty, never an absent name the runner's
// own `.env` auto-load would fill.
const WorkspaceAgentMaxBytesEnv = "ITERION_CLAW_WORKSPACE_AGENT_MAX_BYTES"

// defaultWorkspaceAgentMaxBytes matches the workspace-command ceiling: two
// orders of magnitude above any real agent file, so it bounds the abuse
// without bounding the use — and it is an abstention, never a truncation,
// because half a system prompt is an instruction nobody wrote.
const defaultWorkspaceAgentMaxBytes = 256 << 10

// WorkspaceAgent is one named subagent read from a workspace's
// `.claude/agents/` directory.
type WorkspaceAgent struct {
	// Name is the subagent_type that resolves to this agent: the file's
	// basename without `.md`.
	Name string
	// Description is the frontmatter `description:` when present.
	Description string
	// Body is the agent's prompt — the file's text past the frontmatter,
	// which serves as the child conversation's system prompt.
	Body string
	// Tools is the frontmatter `tools:`, folded onto iterion's tool names,
	// sorted and deduplicated. Meaningful only when HasTools holds.
	Tools []string
	// HasTools reports whether the frontmatter declared `tools:` at all —
	// an explicit `tools: []` narrows the child to nothing, an absent key
	// narrows nothing.
	HasTools bool
	// Model is the frontmatter `model:` when present. Never honoured: its
	// presence refuses the delegation (ErrWorkspaceAgentModel, #1717).
	Model string
	// Path is the file the agent was read from.
	Path string
}

// ToolSet is the frontmatter's `tools:` as the allowlist filterSubagentTools
// takes — nil when the file declared none (no narrowing).
func (a WorkspaceAgent) ToolSet() map[string]bool {
	if !a.HasTools {
		return nil
	}
	set := make(map[string]bool, len(a.Tools))
	for _, n := range a.Tools {
		set[n] = true
	}
	return set
}

// workspaceAgentToolNames folds Claude Code's agent-file spellings onto the
// tool names a claw node's effective set carries. Lower-cased first, so
// `Glob`/`Grep`/`Bash` need no rows; the rows are the spellings that differ
// past case. toolcatalog.CanonicalToolName is NOT this table: it folds onto
// canonical CONCEPTS (`read`, `webfetch`), which are not the registry's tool
// names (`read_file`, `web_fetch`), and toolcatalog.BuiltinAlias's exact-case
// tier covers three of these. A frontmatter name no row and no effective
// tool claims simply intersects to nothing — narrowing can only ever remove.
var workspaceAgentToolNames = map[string]string{
	"read":         "read_file",
	"write":        "write_file",
	"edit":         "file_edit",
	"multiedit":    "file_edit",
	"notebookedit": "notebook_edit",
	"webfetch":     "web_fetch",
	"websearch":    "web_search",
	"todowrite":    "todo_write",
	"task":         "agent",
}

// workspaceAgentToolName folds one frontmatter spelling onto an iterion tool
// name.
func workspaceAgentToolName(spelling string) string {
	n := strings.ToLower(strings.TrimSpace(spelling))
	if folded, ok := workspaceAgentToolNames[n]; ok {
		return folded
	}
	return n
}

// validWorkspaceAgentName reports whether name is one path segment of
// letters, digits, `_`, `-` and interior dots — never a separator, never a
// dot-run. It is the shape rule, not the containment guarantee (that is
// os.Root's): a name has no business spelling what the root would refuse.
func validWorkspaceAgentName(name string) bool {
	if name == "" || strings.Trim(name, ".") == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// notAnAgentFile reports whether err says the name designates no agent file
// at all — it does not exist, is too long, is a directory, or a segment is
// not one. Those are "no such agent", never "the disk refused": a workspace
// contributes agents optionally, so an absent one must not fail a delegation.
func notAnAgentFile(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ENAMETOOLONG) ||
		errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.ENOTDIR)
}

// LookupWorkspaceAgent resolves a subagent_type against workDir's
// `.claude/agents/`, reading `<name>.md` through os.Root rooted at the
// WORKSPACE. Scope is the workspace alone — no ancestor walk, no user-level
// `~/.claude/agents/`: a workspace is a checkout the run does not control,
// and host state does not belong in a sandboxed or multi-tenant run.
//
// found=false with a nil error means "no such agent" — the caller falls back
// to the builtin subagent-type behaviour. A name that spells traversal, a
// symlink escaping the workspace, an unreadable or oversized file are
// ERRORS: the delegation is refused rather than silently downgraded.
//
// maxBytes bounds the FILE (the read stops one byte past the ceiling and
// returns ErrWorkspaceAgentTooLarge); non-positive means unbounded.
func LookupWorkspaceAgent(workDir, name string, maxBytes int) (WorkspaceAgent, bool, error) {
	if workDir == "" {
		return WorkspaceAgent{}, false, nil
	}
	if !validWorkspaceAgentName(name) {
		return WorkspaceAgent{}, false, fmt.Errorf("subagent: %q is not a valid workspace agent name (one segment, no separators, no traversal)", name)
	}
	// A fast path, not the guarantee: most workspaces contribute no agent at
	// all and should not pay a root open per delegation.
	info, err := os.Stat(filepath.Join(workDir, ".claude", "agents"))
	if err != nil || !info.IsDir() {
		return WorkspaceAgent{}, false, nil
	}
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return WorkspaceAgent{}, false, fmt.Errorf("subagent: open workspace %s: %w", workDir, err)
	}
	defer root.Close()
	rel := filepath.Join(".claude", "agents", name+".md")
	f, err := root.Open(rel)
	if err != nil {
		if notAnAgentFile(err) {
			return WorkspaceAgent{}, false, nil
		}
		return WorkspaceAgent{}, false, fmt.Errorf("subagent: read %s: %w", filepath.Join(workDir, rel), err)
	}
	defer f.Close()
	var data []byte
	if maxBytes > 0 {
		data, err = io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	} else {
		data, err = io.ReadAll(f)
	}
	if err != nil {
		if notAnAgentFile(err) {
			return WorkspaceAgent{}, false, nil
		}
		return WorkspaceAgent{}, false, fmt.Errorf("subagent: read %s: %w", filepath.Join(workDir, rel), err)
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return WorkspaceAgent{}, false, ErrWorkspaceAgentTooLarge
	}
	agent, err := parseWorkspaceAgent(name, string(data))
	if err != nil {
		return WorkspaceAgent{}, false, fmt.Errorf("subagent: parse %s: %w", filepath.Join(workDir, rel), err)
	}
	agent.Path = filepath.Join(workDir, rel)
	return agent, true, nil
}

// workspaceAgentFrontmatter is the header shape this resolver acts on. Keys
// it does not declare (`name:`, `color:`, …) are read and thrown away — the
// two with a consequence here, `tools:` and `model:`, are both handled.
type workspaceAgentFrontmatter struct {
	Description string `yaml:"description"`
	Model       string `yaml:"model"`
	Tools       any    `yaml:"tools"`
}

// parseWorkspaceAgent splits the frontmatter off an agent file and reads the
// three keys. The accepted block shape is an opening `---` line and a closing
// `---` line, past a BOM and CRLF endings (splitAgentFrontmatter folds both,
// as claude_code's native parse does); anything else — an unterminated block
// included — is body, not frontmatter, so a file whose first line merely
// starts with dashes is never misparsed.
func parseWorkspaceAgent(name, data string) (WorkspaceAgent, error) {
	agent := WorkspaceAgent{Name: name, Body: strings.TrimSpace(data)}
	header, body, ok := splitAgentFrontmatter(data)
	if !ok {
		return agent, nil
	}
	var fm workspaceAgentFrontmatter
	if err := yaml.Unmarshal([]byte(header), &fm); err != nil {
		return WorkspaceAgent{}, err
	}
	agent.Description = strings.TrimSpace(fm.Description)
	agent.Model = strings.TrimSpace(fm.Model)
	agent.Body = strings.TrimSpace(body)
	tools, declared, err := workspaceAgentTools(fm.Tools)
	if err != nil {
		return WorkspaceAgent{}, err
	}
	agent.Tools, agent.HasTools = tools, declared
	return agent, nil
}

// splitAgentFrontmatter returns the header and body of a leading frontmatter
// block, or ok=false when the file opens with none. claude_code parses these
// files natively whatever their bytes — a UTF-8 BOM, CRLF endings — so the
// split folds both before matching: a file one backend reads as frontmatter
// must never be body-with-`model:`-ignored on the other (the refusal of
// ErrWorkspaceAgentModel is opposable only when the parse sees the key).
func splitAgentFrontmatter(data string) (header, body string, ok bool) {
	data = strings.ReplaceAll(strings.TrimPrefix(data, "\xef\xbb\xbf"), "\r\n", "\n")
	rest, found := strings.CutPrefix(data, "---\n")
	if !found {
		return "", "", false
	}
	if i := strings.Index(rest, "\n---\n"); i >= 0 {
		return rest[:i], rest[i+len("\n---\n"):], true
	}
	if strings.HasSuffix(rest, "\n---") {
		return rest[:len(rest)-len("\n---")], "", true
	}
	return "", "", false
}

// workspaceAgentTools reads the frontmatter `tools:` value, in both of its
// accepted shapes — a comma-separated string (`tools: Read, Grep`) and a YAML
// list — folding each spelling onto an iterion tool name. The second return
// distinguishes an absent key (no narrowing) from an explicit empty one (the
// child gets nothing); a `tools:` left null reads as absent.
func workspaceAgentTools(v any) ([]string, bool, error) {
	var raw []string
	switch t := v.(type) {
	case nil:
		return nil, false, nil
	case string:
		for _, part := range strings.Split(t, ",") {
			if p := strings.TrimSpace(part); p != "" {
				raw = append(raw, p)
			}
		}
	case []any:
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, false, fmt.Errorf("tools: entries must be strings, got %v", item)
			}
			if s = strings.TrimSpace(s); s != "" {
				raw = append(raw, s)
			}
		}
	default:
		return nil, false, fmt.Errorf("tools: must be a comma-separated string or a list, got %T", v)
	}
	out := make([]string, 0, len(raw))
	for _, spelling := range raw {
		out = append(out, workspaceAgentToolName(spelling))
	}
	slices.Sort(out)
	return slices.Compact(out), true, nil
}

// workspaceAgentMaxBytes resolves the per-file ceiling: the operator's
// override when it parses, the built-in default otherwise. A non-numeric
// value is ignored rather than obeyed — a typo must not silently remove a
// bound.
//
// Neither is a value a project `.env` planted: the CLI auto-loads that file
// (cmd/iterion applyDotEnv), including out of a repository under review,
// whose answer here would be "0" — the ceiling gone, an unbounded read of
// what turns into a billed system prompt — or "1", every agent file refused.
// A planted value still reads as a value; it does not speak for the
// operator, the same authority rule every security switch follows
// (claw_host_forfait.go, plugin/config.go, runview/executor.go).
func workspaceAgentMaxBytes() int {
	if envtrust.Planted(WorkspaceAgentMaxBytesEnv) {
		return defaultWorkspaceAgentMaxBytes
	}
	raw := strings.TrimSpace(os.Getenv(WorkspaceAgentMaxBytesEnv))
	if raw == "" {
		return defaultWorkspaceAgentMaxBytes
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultWorkspaceAgentMaxBytes
	}
	return n
}

// intersectToolSets narrows one allowlist by another, where a nil set means
// "everything" (claw's general-purpose shape): intersecting with it yields
// the other set. Both narrowings apply; neither widens.
func intersectToolSets(a, b map[string]bool) map[string]bool {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := make(map[string]bool, min(len(a), len(b)))
	for name := range a {
		if b[name] {
			out[name] = true
		}
	}
	return out
}
