package delegate

import (
	"slices"
	"strconv"
	"strings"
	"sync"
)

// SessionLedger remembers, per CLI session id, the background tasks that had
// not reported back when a process of that session ended — still running, or
// finished without their result reaching the agent: they are lost with it, and
// the transcript shows them launched and never shows them end. A process that
// resumes the session — or forks it — is told they are gone; once it ran,
// what it was told is settled and what it left running is recorded.
//
// The entry is keyed by the session id, the transcript itself, never by node,
// edge or attempt: every path that resumes a transcript — a retry, a schema
// re-ask, a loop re-entry, a pause, a fork, a formatting pass — reads the same
// entry, so none of them can carry a stale, a lost or a foreign one.
type SessionLedger interface {
	// Terminated returns the tasks recorded for sessionID, nil when none.
	Terminated(sessionID string) []string
	// Settle records the end of a process that ran sessionID: the tasks it
	// was told about leave the entry, those it left running join it. Only
	// what THIS process was told is removed, so two processes of one session
	// settle to the same entry in either order.
	Settle(sessionID string, told, terminated []string)
}

// MemorySessionLedger is the SessionLedger of one run. The runtime persists
// its Entries in the run's checkpoint and restores them on resume. The zero
// value is not usable; a nil *MemorySessionLedger records nothing.
type MemorySessionLedger struct {
	mu      sync.Mutex
	entries map[string][]string
}

// NewSessionLedger returns a ledger holding a copy of entries.
func NewSessionLedger(entries map[string][]string) *MemorySessionLedger {
	l := &MemorySessionLedger{entries: make(map[string][]string, len(entries))}
	for id, tasks := range entries {
		if id != "" && len(tasks) > 0 {
			l.entries[id] = slices.Clone(tasks)
		}
	}
	return l
}

func (l *MemorySessionLedger) Terminated(sessionID string) []string {
	if l == nil || sessionID == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries[sessionID])
}

func (l *MemorySessionLedger) Settle(sessionID string, told, terminated []string) {
	if l == nil || sessionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var next []string
	for _, t := range l.entries[sessionID] {
		if !slices.Contains(told, t) {
			next = append(next, t)
		}
	}
	next = appendLabels(next, terminated...)
	if len(next) == 0 {
		delete(l.entries, sessionID)
		return
	}
	l.entries[sessionID] = next
}

// appendLabels appends the labels dst does not hold yet, in order.
func appendLabels(dst []string, labels ...string) []string {
	for _, t := range labels {
		if t != "" && !slices.Contains(dst, t) {
			dst = append(dst, t)
		}
	}
	return dst
}

// Entries returns a copy of every entry, for the checkpoint.
func (l *MemorySessionLedger) Entries() map[string][]string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) == 0 {
		return nil
	}
	out := make(map[string][]string, len(l.entries))
	for id, tasks := range l.entries {
		out[id] = slices.Clone(tasks)
	}
	return out
}

// terminatedBackgroundNote prefixes the prompt of a process that resumes a
// transcript whose earlier process lost background work: that transcript may
// show the work launched and never show its result, and the new process has
// no such tasks. Without the note the agent waits on work that no longer
// exists, or reports as if its results had come in. The record errs toward
// telling: a result the transcript does show is named too, and the agent sees
// it above — never the reverse.
func terminatedBackgroundNote(told []string, prompt string) string {
	return backgroundNote(told, prompt, "Re-launch those you still need, or do that work directly.")
}

// formattingPassNote is terminatedBackgroundNote for the one-shot formatting
// pass: it formats what the agent already has. A task it re-launched would
// hold that pass's own process — up to the CLI's wind-down ceiling — for a
// result the pass does not need.
func formattingPassNote(told []string, prompt string) string {
	return backgroundNote(told, prompt, "Do not re-launch them: report with what you have.")
}

func backgroundNote(told []string, prompt, then string) string {
	if len(told) == 0 {
		return prompt
	}
	// Quoted: a label is the task's description, text from the agent's side
	// — data in this note, never a line of it.
	quoted := make([]string, 0, len(told))
	for _, t := range told {
		quoted = append(quoted, strconv.Quote(t))
	}
	return "[iterion] This session resumes in a new process. When an earlier one ended, these background " +
		"tasks launched in this session may not have reported back: " + strings.Join(quoted, "; ") +
		". Any whose result you do not see above never will now — do not wait for it. " + then + "\n\n" + prompt
}
