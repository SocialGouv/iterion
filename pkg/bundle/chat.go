package bundle

import (
	"fmt"
	"sort"
	"strings"
)

// ChatSurface is a bot's declaration that it is a CONVERSATIONAL bot — one
// the studio hosts in its assistant dock rather than launching through the
// generic run form.
//
// It exists so that adding a second conversational bot costs a bundle and
// not a studio release. Before it, `studio/src/lib/whats-next/firstClassBots.ts`
// hard-coded the single whats-next entry, with its own TODO saying so; a bot
// id baked into the product is also the thing CLAUDE.md's "the ENGINE stays
// bot-agnostic" rule forbids, one layer up.
//
// What it can express is bounded on purpose: everything the studio needs to
// RENDER a turn (which node speaks, which node collects the answer, which
// field carries the text) and nothing about what the bot means. The studio
// reads shape; the bot keeps its semantics.
type ChatSurface struct {
	// Label and Description override the bundle's display_name and
	// description for the chat picker. Both optional — empty falls back to
	// the manifest's own, which is the common case.
	Label       string `json:"label,omitempty" yaml:"label,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`

	// SeedVar is the launch var carrying the operator's FIRST message. The
	// composer writes into it when it starts a session, so a bot without one
	// can only be started empty. whats-next and copilot both use
	// "initial_message".
	SeedVar string `json:"seed_var,omitempty" yaml:"seed_var,omitempty"`

	// Nodes maps a workflow node id to how the chat renders it. A node the
	// map does not mention renders as an ordinary run event, which is the
	// safe default: a bot that adds a node without updating its manifest
	// degrades to noisier, never to broken.
	Nodes map[string]ChatNode `json:"nodes,omitempty" yaml:"nodes,omitempty"`

	// LauncherVars are the vars the session launcher asks for before the
	// first message. Empty is the common case: a studio launch already
	// scopes to the server's work_dir.
	LauncherVars []ChatLauncherVar `json:"launcher_vars,omitempty" yaml:"launcher_vars,omitempty"`

	// Launcher is the optional canned-opener form shown instead of a bare
	// Start button. Its answer is written into SeedVar verbatim.
	Launcher *ChatLauncher `json:"launcher,omitempty" yaml:"launcher,omitempty"`
}

// ChatNodeKind is how one workflow node shows up in a transcript.
type ChatNodeKind string

const (
	// ChatNodeBanner is a working node: a collapsible progress banner.
	ChatNodeBanner ChatNodeKind = "banner"
	// ChatNodeHuman is the pause where the operator answers — the composer.
	ChatNodeHuman ChatNodeKind = "human"
	// ChatNodeSilent renders nothing (plumbing: compute/seed/gate nodes).
	ChatNodeSilent ChatNodeKind = "silent"
)

// ChatNode is the per-node presentation hint.
type ChatNode struct {
	Kind ChatNodeKind `json:"kind" yaml:"kind"`

	// Label is the banner caption for a "banner" node ("Nexie is working").
	Label string `json:"label,omitempty" yaml:"label,omitempty"`

	// SummaryField plucks a field off the node's output as the banner's
	// collapsed one-liner. Empty closes the banner with no summary.
	SummaryField string `json:"summary_field,omitempty" yaml:"summary_field,omitempty"`

	// Prompt is the assistant-side text shown above a "human" node's input.
	// Deliberately usually EMPTY: the runtime-resolved `instructions:` of the
	// node is the bot's actual reply, and a manifest string here would
	// overwrite it with a constant.
	Prompt string `json:"prompt,omitempty" yaml:"prompt,omitempty"`

	// TextField is the answer-schema field the operator's typed text lands
	// in ("message" for both shipped chat bots).
	TextField string `json:"text_field,omitempty" yaml:"text_field,omitempty"`

	// ApprovedField is the boolean field for a "human" node rendered with
	// approve/reject buttons instead of a free-text composer.
	ApprovedField string `json:"approved_field,omitempty" yaml:"approved_field,omitempty"`
}

// ChatLauncherVar is one var the session launcher collects up front.
type ChatLauncherVar struct {
	Name  string `json:"name" yaml:"name"`
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
	// DefaultFrom names a studio-side source to pre-fill from. The only
	// value the studio understands today is "work_dir"; an unknown one is
	// ignored rather than rejected, so a newer bundle degrades to an empty
	// field instead of failing to load.
	DefaultFrom string `json:"default_from,omitempty" yaml:"default_from,omitempty"`
}

// ChatLauncher is the canned-opener form.
type ChatLauncher struct {
	Prompt      string `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	SubmitLabel string `json:"submit_label,omitempty" yaml:"submit_label,omitempty"`
	// AllowOther keeps the free-text escape hatch on the preset list. It
	// defaults to TRUE at normalization: a canned list that cannot be
	// escaped turns a conversation into a menu.
	AllowOther *bool            `json:"allow_other,omitempty" yaml:"allow_other,omitempty"`
	Presets    []ChatSeedPreset `json:"presets,omitempty" yaml:"presets,omitempty"`
}

// ChatSeedPreset is one canned first message.
type ChatSeedPreset struct {
	// Value is what the bot receives VERBATIM as the first message.
	Value       string `json:"value" yaml:"value"`
	Label       string `json:"label,omitempty" yaml:"label,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// chatNodeKinds is the closed set a manifest may name. Closed because the
// studio switches on it to pick a renderer: an unknown kind has no rendering
// at all, and silently dropping the node from the transcript is exactly the
// failure a bot author would not think to look for.
var chatNodeKinds = map[ChatNodeKind]bool{
	ChatNodeBanner: true,
	ChatNodeHuman:  true,
	ChatNodeSilent: true,
}

// normalized trims the block and collapses an effectively-empty one to nil,
// so a bot that wrote `chat:` and nothing under it does not advertise a chat
// surface it cannot serve.
func (c *ChatSurface) normalized() *ChatSurface {
	if c == nil {
		return nil
	}
	out := ChatSurface{
		Label:       strings.TrimSpace(c.Label),
		Description: strings.TrimSpace(c.Description),
		SeedVar:     strings.TrimSpace(c.SeedVar),
	}
	for id, n := range c.Nodes {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if out.Nodes == nil {
			out.Nodes = map[string]ChatNode{}
		}
		out.Nodes[id] = ChatNode{
			Kind:          ChatNodeKind(strings.TrimSpace(string(n.Kind))),
			Label:         strings.TrimSpace(n.Label),
			SummaryField:  strings.TrimSpace(n.SummaryField),
			Prompt:        strings.TrimSpace(n.Prompt),
			TextField:     strings.TrimSpace(n.TextField),
			ApprovedField: strings.TrimSpace(n.ApprovedField),
		}
	}
	for _, v := range c.LauncherVars {
		name := strings.TrimSpace(v.Name)
		if name == "" {
			continue
		}
		out.LauncherVars = append(out.LauncherVars, ChatLauncherVar{
			Name:        name,
			Label:       strings.TrimSpace(v.Label),
			DefaultFrom: strings.TrimSpace(v.DefaultFrom),
		})
	}
	if c.Launcher != nil {
		l := ChatLauncher{
			Prompt:      strings.TrimSpace(c.Launcher.Prompt),
			Description: strings.TrimSpace(c.Launcher.Description),
			SubmitLabel: strings.TrimSpace(c.Launcher.SubmitLabel),
			AllowOther:  c.Launcher.AllowOther,
		}
		for _, p := range c.Launcher.Presets {
			value := strings.TrimSpace(p.Value)
			if value == "" {
				continue
			}
			l.Presets = append(l.Presets, ChatSeedPreset{
				Value:       value,
				Label:       strings.TrimSpace(p.Label),
				Description: strings.TrimSpace(p.Description),
			})
		}
		if l.AllowOther == nil {
			// A canned list with no way out is a menu, not a conversation.
			allow := true
			l.AllowOther = &allow
		}
		if l.Prompt != "" || l.Description != "" || l.SubmitLabel != "" || len(l.Presets) > 0 {
			out.Launcher = &l
		}
	}
	if out.Label == "" && out.Description == "" && out.SeedVar == "" &&
		len(out.Nodes) == 0 && len(out.LauncherVars) == 0 && out.Launcher == nil {
		return nil
	}
	return &out
}

// validateChatSurface rejects a chat block the studio could not render.
//
// It fails the BUNDLE rather than warning, for one reason: this block's whole
// job is to be read by a surface that is not here to complain. A typo'd kind
// or a "human" node with no answer field produces a chat window that looks
// alive and swallows every message, and the author's only signal would be an
// operator saying "it does nothing".
func validateChatSurface(c *ChatSurface) error {
	if c == nil {
		return nil
	}
	humans := 0
	for _, id := range sortedNodeIDs(c.Nodes) {
		n := c.Nodes[id]
		if !chatNodeKinds[n.Kind] {
			return fmt.Errorf("chat: node %q has kind %q — expected banner, human or silent", id, n.Kind)
		}
		if n.Kind != ChatNodeHuman && (n.TextField != "" || n.ApprovedField != "") {
			return fmt.Errorf("chat: node %q is %q but declares an answer field — only a human node collects one", id, n.Kind)
		}
		if n.Kind == ChatNodeHuman {
			humans++
			// Both assistant chat surfaces route a pending turn through the
			// unified text composer. They do not render the transcript's
			// approve/reject buttons for that turn, so admitting an
			// approved_field would make the bundle look interactive while no
			// boolean can ever be submitted.
			if n.ApprovedField != "" {
				return fmt.Errorf("chat: node %q declares approved_field %q, but assistant chat surfaces currently submit text answers only", id, n.ApprovedField)
			}
			if n.TextField == "" {
				return fmt.Errorf("chat: node %q is the operator's turn but names no text_field — its answers would have nowhere to land", id)
			}
		}
	}
	if len(c.Nodes) > 0 && humans == 0 {
		return fmt.Errorf("chat: no node is the operator's turn (kind: human) — the session could never be answered")
	}
	if c.Launcher != nil && c.SeedVar == "" {
		return fmt.Errorf("chat: a launcher form was declared but seed_var is empty — the operator's first message would be discarded")
	}
	return nil
}

// sortedNodeIDs keeps validation errors deterministic across runs; Go map
// order would otherwise report a different one of several offending nodes
// each time, which reads as flakiness in CI.
func sortedNodeIDs(m map[string]ChatNode) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
