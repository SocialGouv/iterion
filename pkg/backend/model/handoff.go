package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The routing handoff (ADR-121 § Delivery 2): what a cross-harness switch
// hands the incoming harness. A HandoffRecorder buffers the four neutral
// event types per node, seals them to a markdown file when the run's
// model_fallback event carries an active cross_harness posture, and the
// dispatch's clearing branch attaches the sealed path to the incoming
// element's task. Under "off" (and on every local run) no recorder exists:
// nothing is recorded, no directory is created, and no event field changes.
//
// The recorder is fed by handoffEmitter — an EventEmitter wrapper the
// runview layer installs AROUND the store emitter, so NewStoreEventHooks
// builds redacting(recorder(store)): the recorder observes exactly what is
// persisted, post-redaction, because the handoff file is read by another
// harness with a different credential scope.

// The neutral event vocabulary the handoff records. Everything else —
// llm_prompt, usage, degradation, fallback itself — is route bookkeeping,
// not work the incoming harness should read.
func handoffEventType(t store.EventType) bool {
	switch t {
	case store.EventAssistantText, store.EventToolCalled, store.EventToolError, store.EventLLMStepFinished:
		return true
	}
	return false
}

const (
	// handoffHeadBytes/handoffTailBytes bound one node's buffer: the head
	// (where the work started) plus the tail (where it failed) survive,
	// the middle collapses behind an omission marker. Applied AT APPEND
	// time so a seal renders truncation-free.
	handoffHeadBytes = 32 * 1024
	handoffTailBytes = 160 * 1024
)

// handoffEntry is one buffered event, pre-rendered to its markdown
// section at append time (the size accounting is then exact).
type handoffEntry struct {
	section string
	size    int
}

// handoffNode is one node's buffer plus its seal state. All access is
// guarded by the recorder's single mutex: events arrive from concurrent
// branch goroutines and the seal rides the dispatch goroutine.
type handoffNode struct {
	entries    []handoffEntry
	bytes      int
	dropped    int
	headCount  int    // entries inside the head budget
	lastStep   string // the response_text of the last buffered llm_step_finished
	sealedPath string
}

// HandoffRecorder records the per-node transcripts a cross-harness
// crossing hands over. Construct it only for a run whose resolved policy
// carries an ACTIVE cross-harness posture; nil elsewhere is the design —
// off records nothing.
type HandoffRecorder struct {
	mu  sync.Mutex
	dir string // <state dir>/routing-handoff; empty until a state dir arrives
	// fallbackDir seeds dir when the run has no shared state dir
	// (copy-based drivers): the artifact files scratch area, same
	// absolute-path guarantees.
	fallbackDir string
	nodes       map[string]*handoffNode
	created     bool             // this recorder made the directory (and only it may remove it)
	closed      bool             // Cleanup ran: no further writes
	now         func() time.Time // test seam
}

// NewHandoffRecorder builds a recorder rooted at fallbackRoot (the run's
// artifact files dir, possibly empty). SetRoot upgrades it to the shared
// state dir when the sandbox driver provides one — always before the
// first Execute, hence before any seal.
func NewHandoffRecorder(fallbackRoot string) *HandoffRecorder {
	rec := &HandoffRecorder{
		fallbackDir: fallbackRoot,
		nodes:       map[string]*handoffNode{},
		now:         time.Now,
	}
	if fallbackRoot != "" {
		rec.dir = filepath.Join(fallbackRoot, "routing-handoff")
	}
	return rec
}

// SetRoot re-homes the recorder under the shared state dir (the same
// absolute path host and sandbox see, outside the checkout). Called by the
// executor's SetSharedStateDir — the sandbox driver sets it before the
// first Execute, so no seal can have run yet.
func (r *HandoffRecorder) SetRoot(dir string) {
	if dir == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealedAny() {
		// Unreachable in production (SetRoot precedes Execute); refuse to
		// re-home under sealed files rather than orphan them.
		return
	}
	r.dir = filepath.Join(dir, "routing-handoff")
}

// sealedAny reports whether any node has been sealed. Caller holds the lock.
func (r *HandoffRecorder) sealedAny() bool {
	for _, n := range r.nodes {
		if n.sealedPath != "" {
			return true
		}
	}
	return false
}

// Observe buffers one event's rendered section for its node. Only the
// neutral vocabulary lands; claw's double capture (the step text rides
// both llm_step_finished's response_text and a derived assistant_text) is
// folded here — a derived assistant_text equal to the step's own
// response_text adds nothing the step section lacks.
func (r *HandoffRecorder) Observe(evType store.EventType, nodeID string, data map[string]any) {
	if !handoffEventType(evType) || nodeID == "" {
		return
	}
	section, size := renderHandoffSection(evType, data)
	if size == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	n := r.nodes[nodeID]
	if n == nil {
		n = &handoffNode{}
		r.nodes[nodeID] = n
	}
	if evType == store.EventLLMStepFinished {
		n.lastStep, _ = data["response_text"].(string)
	}
	if evType == store.EventAssistantText {
		if text, _ := data["text"].(string); text != "" && text == n.lastStep {
			return
		}
	}
	section, size = truncateHandoffSection(section, size)
	n.entries = append(n.entries, handoffEntry{section: section, size: size})
	n.bytes += size
	r.capNode(n)
}

// capNode applies the head+tail budget: once the buffer passes it, middle
// entries (everything after the head region) drop until it fits again,
// counted in dropped. The head region extends only while the NEXT entry
// still fits inside the head budget — a single oversized first event
// would otherwise become the whole head and starve the tail to what the
// budget has left (revi R49ea1b); entries are clamped at append to the
// tail budget, so nothing a run produced can ever evaporate behind the
// omission marker. Caller holds the lock.
func (r *HandoffRecorder) capNode(n *handoffNode) {
	budget := handoffHeadBytes + handoffTailBytes
	for n.headCount < len(n.entries) && n.headBytes()+n.entries[n.headCount].size <= handoffHeadBytes {
		n.headCount++
	}
	if n.bytes <= budget {
		return
	}
	drop := n.headCount
	for drop < len(n.entries) && n.bytes > budget {
		n.bytes -= n.entries[drop].size
		drop++
	}
	n.dropped += drop - n.headCount
	n.entries = append(n.entries[:n.headCount], n.entries[drop:]...)
}

// truncateHandoffSection clamps one event's rendered section to the tail
// budget — a single llm_step_finished can carry up to maxFieldSize of
// inline text, more than the node's whole transcript budget, and an
// unclamped entry that big would either starve every newer event or be
// dropped whole (its work unrepresented). The cut is rune-safe and says
// so.
func truncateHandoffSection(section string, size int) (string, int) {
	if size <= handoffTailBytes {
		return section, size
	}
	cut := handoffTailBytes
	for cut > 0 && !utf8.RuneStart(section[cut]) {
		cut--
	}
	clamped := section[:cut] + "\n[… event truncated …]\n"
	return clamped, len(clamped)
}

// headBytes sums the head region. Caller holds the lock.
func (n *handoffNode) headBytes() int {
	sum := 0
	for i := 0; i < n.headCount && i < len(n.entries); i++ {
		sum += n.entries[i].size
	}
	return sum
}

// Seal renders the node's buffer to its markdown file and marks it
// read-only. Re-seals re-render from the fuller buffer (a second crossing
// hands over the first rung's own work too) over the same path via
// temp+rename — a rename needs only the directory, not write permission
// on the sealed target. An empty buffer seals nothing: the caller stamps
// the event "unavailable" instead of inventing an empty artifact.
func (r *HandoffRecorder) Seal(nodeID string, fallbackData map[string]any) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || nodeID == "" || r.dir == "" {
		return ""
	}
	n := r.nodes[nodeID]
	if n == nil || len(n.entries) == 0 {
		return ""
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return ""
	}
	r.created = true
	path := filepath.Join(r.dir, sanitizeNodeFile(nodeID)+".md")
	body := renderHandoffFile(nodeID, n, fallbackData, r.now().UTC())
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return ""
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		_ = os.Remove(tmp)
		return ""
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return ""
	}
	n.sealedPath = path
	return path
}

// SealedPath returns the node's sealed handoff file, or "" when nothing
// was sealed (no active posture crossing, empty buffer, or the recorder
// never received the state dir).
func (r *HandoffRecorder) SealedPath(nodeID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := r.nodes[nodeID]; n != nil {
		return n.sealedPath
	}
	return ""
}

// Cleanup removes the routing-handoff directory this recorder created —
// and nothing else: a recorder that never created one (no active posture,
// no seal, or no state dir) removes nothing. Called from the executor's
// Close.
func (r *HandoffRecorder) Cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.created || r.dir == "" {
		return
	}
	_ = os.RemoveAll(r.dir)
	r.created = false
	r.closed = true
}

// sanitizeNodeFile maps a node id to one file name: ids are author
// vocabulary, but a handoff file lives on disk — keep it to a safe
// alphabet, escaping everything else.
func sanitizeNodeFile(nodeID string) string {
	var b strings.Builder
	for _, c := range nodeID {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteRune(c)
		default:
			fmt.Fprintf(&b, "_%04x_", c)
		}
	}
	return b.String()
}

// renderHandoffFile composes the artifact: a header naming the node, the
// failed route and the classified refusal, then the buffered sections in
// arrival order with the omission marker where the middle dropped. The
// vocabulary is neutral — event type names, tool names, prose — never a
// harness's own transcript format.
func renderHandoffFile(nodeID string, n *handoffNode, fb map[string]any, sealed time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Routing handoff — node %s\n\n", nodeID)
	fmt.Fprintf(&b, "The harness serving this node failed and an active cross_harness posture hands its work over. "+
		"Everything below is what that work produced, up to the failed attempt.\n\n")
	mode, _ := fb["cross_harness"].(string)
	fmt.Fprintf(&b, "- posture: `%s`\n", mode)
	fmt.Fprintf(&b, "- failed route: %s", handoffRouteLabel(fb["from_backend"], fb["from_provider"], fb["from_model"]))
	fmt.Fprintf(&b, " → %s\n", handoffRouteLabel(fb["to_backend"], fb["to_provider"], fb["to_model"]))
	if reason, _ := fb["reason"].(string); reason != "" {
		fmt.Fprintf(&b, "- reason: %s\n", reason)
	}
	fmt.Fprintf(&b, "- sealed: %s\n\n", sealed.Format(time.RFC3339))
	if n.dropped > 0 {
		fmt.Fprintf(&b, "[%d events omitted]\n\n", n.dropped)
	}
	for _, e := range n.entries {
		b.WriteString(e.section)
	}
	return b.String()
}

// handoffRouteLabel renders one end of the failed route.
func handoffRouteLabel(backend, provider, model any) string {
	bs, _ := backend.(string)
	ps, _ := provider.(string)
	ms, _ := model.(string)
	parts := make([]string, 0, 3)
	if bs != "" {
		parts = append(parts, bs)
	}
	if ps != "" {
		parts = append(parts, "("+ps+")")
	}
	label := strings.Join(parts, " ")
	if ms != "" {
		if label != "" {
			label += " "
		}
		label += ms
	}
	if label == "" {
		return "?"
	}
	return label
}

// renderHandoffSection renders one event as a markdown section. A text
// field renders as a fenced block; scalar fields as a list. Empty when
// the event carries nothing renderable.
func renderHandoffSection(evType store.EventType, data map[string]any) (string, int) {
	if data == nil {
		return "", 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", evType)
	wrote := false
	// Stable order: the sorted keys, with the prose fields folded into
	// fenced blocks at their sorted position.
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := data[k]
		switch t := v.(type) {
		case string:
			if t == "" {
				continue
			}
			if k == "text" || k == "response_text" {
				fmt.Fprintf(&b, "%s:\n\n```\n%s\n```\n\n", k, t)
			} else {
				fmt.Fprintf(&b, "- %s: %s\n", k, t)
			}
			wrote = true
		case float64:
			fmt.Fprintf(&b, "- %s: %v\n", k, int64(t))
			wrote = true
		case int:
			fmt.Fprintf(&b, "- %s: %d\n", k, t)
			wrote = true
		case bool:
			fmt.Fprintf(&b, "- %s: %v\n", k, t)
			wrote = true
		}
	}
	if !wrote {
		return "", 0
	}
	b.WriteString("\n")
	s := b.String()
	return s, len(s)
}

// handoffEmitter is the EventEmitter wrapper the runview layer installs
// around the store emitter: NewStoreEventHooks then builds
// redacting(recorder(store)), so Observe and the seal see exactly what is
// persisted — post-redaction — and the model_fallback event that crosses
// carries its handoff field onto the wire.
type handoffEmitter struct {
	inner EventEmitter
	rec   *HandoffRecorder
}

// HandoffEmitter wraps inner with the recorder, or returns inner unchanged
// when either side is missing (the nil-by-default direction: no posture,
// no wrapper, byte-identical events).
func HandoffEmitter(inner EventEmitter, rec *HandoffRecorder) EventEmitter {
	if inner == nil || rec == nil {
		return inner
	}
	return handoffEmitter{inner: inner, rec: rec}
}

func (h handoffEmitter) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	h.rec.Observe(evt.Type, evt.NodeID, evt.Data)
	if evt.Type == store.EventModelFallback && evt.Data != nil {
		if mode, _ := evt.Data["cross_harness"].(string); llmroute.CrossHarnessActive(mode) {
			if p := h.rec.Seal(evt.NodeID, evt.Data); p != "" {
				evt.Data["handoff"] = p
			} else {
				evt.Data["handoff"] = "unavailable"
			}
		}
	}
	return h.inner.AppendEvent(ctx, runID, evt)
}
