package tool

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// CommandScope is the per-generation narrowing a workspace slash command's
// `allowed-tools:` frontmatter applies (#1717). claude_code bounds a command
// that declares one; claw's own tool loop is the place that has to hold the
// same line, dispatch-side, as claw-code-go's SkillStateLock does for its
// skills (internal/runtime/skill_state.go).
//
// It lives on the CONTEXT, not on the Registry: a run's registry is built
// once (runview.BuildExecutor) and shared by every node the run executes, so
// state hung on it would leak one node's narrowing into the next. The
// backend pins a fresh scope per node execution (model.ClawBackend.Execute)
// and the `command` tool's Execute — which receives that same context — is
// what narrows it. The in-container runner of a sandboxed node re-executes
// the same code, so the boundary needs no IPC: a narrowed tool is refused
// before its proxy would forward the call.
//
// The semantics are INTERSECT-ONLY, a deliberate divergence from the
// vendor's applySkillSideEffects (where the OPERATOR's next turn lifts the
// narrowing): on this surface every invocation is model-side, so a command
// without `allowed-tools:` changes nothing and a command with one can only
// shrink the scope further — a self-liftable restriction is none at all
// (review HIGH). The vendor's parser keeps a DECLARED empty list distinct
// from an absent key: `allowed-tools: []` narrows to the always-allowed
// trio. Narrowing only ever restricts: the dispatch gate intersects with
// the node's declared tools for free, since a tool the node does not hold
// is refused before the gate is even consulted.
type CommandScope struct {
	mu      sync.Mutex
	command string // the latest command that narrowed, for the refusal message
	set     narrowedSet
	active  bool
}

// narrowedSet is the folded answer to one or more `allowed-tools:` lists:
// exact tool names, MCP server FAMILIES (`mcp__srv__*` → the `mcp_srv_`
// prefix), and the bare `*` wildcard — the shapes a Claude Code command
// frontmatter publishes (review LOW-5).
type narrowedSet struct {
	names    map[string]bool
	families []string
	wildcard bool
}

// allows reports whether name passes the set. The always-allowed trio is
// NOT answered here — CheckAllowed exempts it ahead of the set, so the
// set itself can be shared with the subagent filter (where the trio's
// members are not all present) without that filter granting them.
func (s narrowedSet) allows(name string) bool {
	if s.wildcard {
		return true
	}
	if s.names[name] {
		return true
	}
	for _, prefix := range s.families {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// intersect keeps only what BOTH sets allow — the narrowing a second
// command adds to the first.
func (s narrowedSet) intersect(other narrowedSet) narrowedSet {
	out := narrowedSet{names: map[string]bool{}}
	// A wildcard set accepts what the other accepts: X ∩ * = X.
	if s.wildcard {
		return other
	}
	if other.wildcard {
		return s
	}
	for name := range s.names {
		if other.names[name] {
			out.names[name] = true
		}
	}
	for _, prefix := range s.families {
		if slices.Contains(other.families, prefix) {
			out.families = append(out.families, prefix)
		}
	}
	return out
}

// buildNarrowedSet folds an `allowed-tools:` list into a narrowedSet. The
// always-allowed trio is NOT added here: the dispatch gate exempts it on
// its own, and the surfaces that filter a tool SET add it through
// withAlwaysAllowed — a narrowed node that loses `command` can never invoke
// the command that narrows further, and one that loses `todo_write` breaks
// the loop's own bookkeeping.
func buildNarrowedSet(allowed []string) narrowedSet {
	set := narrowedSet{names: map[string]bool{}}
	for _, spelling := range allowed {
		s := strings.TrimSpace(spelling)
		if s == "*" {
			set.wildcard = true
			continue
		}
		if rest, ok := strings.CutSuffix(s, "__*"); ok && strings.HasPrefix(s, "mcp__") {
			// `mcp__srv__*` names the server's whole family, advertised here
			// under the sanitized `mcp_srv_` prefix.
			if server, _, ok := SplitMCPFQN(rest + "__x"); ok {
				set.families = append(set.families, "mcp_"+server+"_")
			}
			continue
		}
		if name := FoldCLISpelling(s); name != "" {
			set.names[name] = true
		}
	}
	return set
}

// withAlwaysAllowed adds the trio to the set, for the surfaces that filter
// a node's tool SET (the prompt-path build-time intersection): the gate
// exempts the trio on its own, but a set used to shrink an advertised list
// must carry them or the narrowed node loses the meta-operations it needs
// to recover.
func (s narrowedSet) withAlwaysAllowed() narrowedSet {
	for name := range alwaysAllowedCommandTools {
		s.names[name] = true
	}
	return s
}

// alwaysAllowedCommandTools dispatch regardless of the active narrowing —
// the meta-operations the model needs to change or recover its context.
// `command` heads the list: without it a narrowed node could never invoke
// the command that narrows further.
var alwaysAllowedCommandTools = map[string]struct{}{
	"command":     {},
	"tool_search": {},
	"todo_write":  {},
}

// ctxKeyCommandScope is the context key WithCommandScope pins the scope
// under, beside ctxKeyNodeExecutableTools — the other per-node fact a
// tool's Execute reads.
type ctxKeyCommandScope struct{}

// WithCommandScope pins the node execution's CommandScope on the context
// every tool Execute of that execution receives.
func WithCommandScope(ctx context.Context, scope *CommandScope) context.Context {
	return context.WithValue(ctx, ctxKeyCommandScope{}, scope)
}

// CommandScopeFrom reads the pinned scope, or nil when the caller is not an
// execution that pinned one (a direct Execute in a test, a tool run outside
// any loop). A nil scope narrows nothing: the gate and the `command` tool
// both treat it as "no narrowing possible here".
func CommandScopeFrom(ctx context.Context) *CommandScope {
	scope, _ := ctx.Value(ctxKeyCommandScope{}).(*CommandScope)
	return scope
}

// Apply narrows the scope by the given command's `allowed-tools:` list.
// INTERSECT-ONLY: the first invocation narrows, the following ones can only
// shrink the scope further — a command without the key never reaches this
// (the caller skips Apply), because a restriction the model could lift by
// invoking a plain command would be none at all on a surface where every
// invocation is model-side (#1717, review HIGH).
func (s *CommandScope) Apply(command string, allowed []string) {
	fresh := buildNarrowedSet(allowed)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		s.set, s.active = fresh, true
	} else {
		s.set = s.set.intersect(fresh)
	}
	s.command = command
}

// CheckAllowed reports whether name may dispatch under the active narrowing.
// Nil means yes — no narrowing, an always-allowed tool, or a listed one. The
// refusal names the command that narrowed, so the model can adapt (invoke a
// command within the bound, or report it) rather than retry blind.
func (s *CommandScope) CheckAllowed(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return nil
	}
	if _, ok := alwaysAllowedCommandTools[name]; ok {
		return nil
	}
	if s.set.allows(name) {
		return nil
	}
	return fmt.Errorf("tool %q is not in allowed-tools for command %q; allowed: [%s]",
		name, s.command, strings.Join(s.allowedLocked(), ", "))
}

// allowedLocked renders the current allowance for the refusal message and
// the notice: the names, the families in their declared spelling, and the
// trio — sorted, so the sentence is stable. Caller holds the lock.
func (s *CommandScope) allowedLocked() []string {
	if s.set.wildcard {
		return []string{"*"}
	}
	out := make([]string, 0, len(s.set.names)+len(s.set.families))
	for name := range s.set.names {
		out = append(out, name)
	}
	for _, prefix := range s.set.families {
		out = append(out, prefix+"*")
	}
	for name := range alwaysAllowedCommandTools {
		out = append(out, name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Active reports whether a narrowing binds. A false scope narrows nothing —
// Allows then answers true for everything.
func (s *CommandScope) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Allows reports whether name passes the active narrowing, WITHOUT the
// always-allowed exemption: the consumer is a tool-SET filter (the subagent
// spawn, review MED-3), not the dispatch gate. Inactive → true.
func (s *CommandScope) Allows(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return true
	}
	return s.set.allows(name)
}

// NarrowedBy reports the latest command that narrowed the scope and the
// current allowance, for the notice the `command` tool prefixes to its
// answer. The second return is false when no narrowing is active.
func (s *CommandScope) NarrowedBy() (string, []string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return "", nil, false
	}
	return s.command, s.allowedLocked(), true
}

// Snapshot captures the active narrowing for the pause checkpoint (#1717,
// review MED-1): the run the operator resumes must keep the bound the
// interrupted generation held — the approval answered ONE call, not the
// restriction's lifting. Nil when no narrowing is active.
func (s *CommandScope) Snapshot() *delegate.CommandNarrowingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return nil
	}
	st := &delegate.CommandNarrowingState{
		Command:  s.command,
		Wildcard: s.set.wildcard,
		Families: append([]string(nil), s.set.families...),
	}
	for name := range s.set.names {
		st.Names = append(st.Names, name)
	}
	slices.Sort(st.Names)
	return st
}

// Restore re-establishes a checkpointed narrowing on a fresh scope (the
// resume path). A nil state restores nothing.
func (s *CommandScope) Restore(st *delegate.CommandNarrowingState) {
	if st == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set := narrowedSet{wildcard: st.Wildcard, families: append([]string(nil), st.Families...), names: map[string]bool{}}
	for _, name := range st.Names {
		set.names[name] = true
	}
	s.set, s.command, s.active = set, st.Command, true
}

// CommandNarrowedSet is the build-time answer to an `allowed-tools:` list
// (#1717, prompt-invoked surface): the folded allowance PLUS the
// always-allowed trio, as the predicate model.buildTask intersects the
// node's resolved ToolDefs with. The one derivation, shared with the
// mid-loop gate (CommandScope), so the two surfaces of the capability never
// disagree about what a narrowing permits.
func CommandNarrowedSet(allowed []string) func(string) bool {
	set := buildNarrowedSet(allowed).withAlwaysAllowed()
	return set.allows
}
