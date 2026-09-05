package forge

import (
	"fmt"
	"sort"
	"strings"
)

// The binding's cached STATUS VOCABULARY and its repair (ADR-097 §5).
//
// BindBoard turns names into ids once. What it stores is two halves that go
// stale independently against a board the operator keeps editing:
//
//   - the OPTION IDS (state → id) are what every write uses. They survive a
//     rename and die with a delete;
//   - the COLUMN NAMES (the StatusMapping) are what both directions COMPARE:
//     the import maps a board status onto a native state, the reflect checks
//     the state's status against the board's. They survive a delete-and-re-add
//     and die with a rename.
//
// So a rename leaves a valid id under a name nothing matches — the import goes
// inert and the reflect rewrites the same option every pass, forever — and a
// re-add leaves a matching name over a dead id. Neither is an exotic case: a
// board column is an operator's to edit.
//
// The repair below re-resolves both halves against the board's live field, by
// id first and by name second. It is pure; persistence and the degraded
// readout belong to the caller.

// StatusVocabulary is the cached half of a binding: the (column ⇄ state) map,
// the option id per state, and the mapped columns the board does not carry.
//
// It is what a repair rewrites, and the ONLY thing it rewrites — a binding's
// address, credential and policy are the operator's, never a sync pass's.
type StatusVocabulary struct {
	Mapping         []StatusMapping   `bson:"status_mapping,omitempty" json:"status_mapping,omitempty"`
	Options         map[string]string `bson:"status_options,omitempty" json:"status_options,omitempty"`
	StatusFieldID   string            `bson:"status_field_id,omitempty" json:"status_field_id,omitempty"`
	MissingStatuses []string          `bson:"missing_statuses,omitempty" json:"missing_statuses,omitempty"`
	// UnresolvedAtBind travels with the rest so a reconciliation can persist
	// the set it RECONSTRUCTED for a binding stored before the field existed.
	// A reconciliation never otherwise writes it — see BoardBinding's own doc.
	UnresolvedAtBind []string `bson:"unresolved_at_bind" json:"unresolved_at_bind,omitempty"`
}

// Vocabulary reads the binding's cached status vocabulary.
func (b BoardBinding) Vocabulary() StatusVocabulary {
	return StatusVocabulary{
		Mapping: b.StatusMapping, Options: b.StatusOptions,
		StatusFieldID: b.StatusFieldID, MissingStatuses: b.MissingStatuses,
		UnresolvedAtBind: b.UnresolvedAtBind,
	}
}

// SetVocabulary writes it back.
func (b *BoardBinding) SetVocabulary(v StatusVocabulary) {
	b.StatusMapping, b.StatusOptions = v.Mapping, v.Options
	b.StatusFieldID, b.MissingStatuses = v.StatusFieldID, v.MissingStatuses
	b.UnresolvedAtBind = v.UnresolvedAtBind
}

// StatusRename is one column the board renamed under a stable option id.
type StatusRename struct {
	State string
	From  string
	To    string
}

// LostColumn is one mapped column the board answers to under NEITHER its
// cached option id nor its name. Both halves are reported: the operator
// re-creates a COLUMN, the sync serves a STATE.
type LostColumn struct {
	State  string
	Status string
}

// StatusVocabularyRepair reports what one reconciliation changed, and what it
// could not. Each list holds NATIVE STATE names, so a caller can act per card.
type StatusVocabularyRepair struct {
	// Renamed are columns still identified by their cached id whose NAME the
	// board changed. Repaired by adopting the board's name.
	Renamed []StatusRename
	// Rebound are columns whose cached id is dead but whose name still
	// resolves — deleted and re-added. Repaired by adopting the new id.
	Rebound []string
	// Adopted are states the board had no column for when the binding was made
	// and now does.
	Adopted []string
	// FieldRebound reports that the Status FIELD itself was re-created and its
	// id re-resolved by name.
	FieldRebound bool
	// Lost are the mapped columns the board answers to under neither their
	// cached option id nor their name, and that never resolved for this
	// binding. Nothing here can repair that, so it is what marks the binding
	// degraded — and it is re-derived on EVERY pass, for as long as it stays
	// true.
	//
	// A column the bind accepted as absent (`UnresolvedAtBind`, the ordinary
	// partial-coverage bind) is NOT lost WHILE it has never resolved: nothing
	// broke, the map simply names more than the board carries. Once a
	// reconciliation adopts it, it has resolved, and its later disappearance is
	// a break like any other — the bind-time exemption discharges.
	Lost []LostColumn

	// changed records whether the reconciliation rewrote the vocabulary, which
	// is a different question from whether anything is wrong: a standing loss
	// re-reports identically on every pass and must not re-write the store.
	changed bool
}

// Changed reports whether the repair altered the vocabulary — i.e. whether the
// caller owes the store a write. A LOST column does not: it is re-derived from
// the same inputs on every pass, and the binding keeps its cached id as the
// evidence that makes that possible.
func (r StatusVocabularyRepair) Changed() bool { return r.changed }

// LostStates is the set of native states no card can be reflected onto,
// keyed for a per-card test. It is what stops the reflect from writing an
// option id the board no longer knows, now that the id is kept.
func (r StatusVocabularyRepair) LostStates() map[string]bool {
	if len(r.Lost) == 0 {
		return nil
	}
	out := make(map[string]bool, len(r.Lost))
	for _, c := range r.Lost {
		out[c.State] = true
	}
	return out
}

// Renames maps each renamed column's OLD name (folded) to its new one.
//
// A card's recorded sync status is a column NAME, so after a rename every card
// carries a name the vocabulary no longer knows. Reading those records through
// this map is what keeps a rename from reading as "the board moved to an
// unknown column" on every card at once.
func (r StatusVocabularyRepair) Renames() map[string]string {
	if len(r.Renamed) == 0 {
		return nil
	}
	out := make(map[string]string, len(r.Renamed))
	for _, m := range r.Renamed {
		out[foldName(m.From)] = m.To
	}
	return out
}

// Reason renders the degradation message for the lost columns, or "" when
// nothing was lost. It NAMES them, because the remedy is to re-create one of
// those columns or re-bind the board with a map that matches what it carries.
func (r StatusVocabularyRepair) Reason() string {
	if len(r.Lost) == 0 {
		return ""
	}
	lost := make([]string, 0, len(r.Lost))
	for _, c := range r.Lost {
		lost = append(lost, fmt.Sprintf("%q (%s)", c.Status, c.State))
	}
	sort.Strings(lost)
	return fmt.Sprintf("the %s field no longer carries %s; re-create the column, or re-bind the board with a --status-map that matches it",
		ProjectStatusFieldName, strings.Join(lost, ", "))
}

// ReconcileStatusOptions re-resolves the binding's cached status vocabulary
// against the board's LIVE schema, and reports what it repaired.
//
// Resolution order per mapped state, and it is the whole point:
//
//  1. the cached OPTION ID still on the field ⇒ the column lives; adopt the
//     board's current NAME for it (this is the rename repair);
//  2. else the mapped NAME still on the field ⇒ the column was re-created, or
//     added since the bind; adopt its id;
//  3. else, and only if the column never resolved for this binding ⇒ LOST.
//
// Rule 3 is a property of the binding's CURRENT shape, not an event: it is
// re-derived identically on every pass, which is what lets a caller treat
// "degraded" as a level rather than an edge. "Never resolved" is a CONJUNCTION
// of two oracles, and neither one alone is sound:
//
//   - `UnresolvedAtBind` — what the operator accepted when they bound the
//     board. Alone it never discharges: a column absent at bind and ADOPTED
//     later earns a real id, so its next disappearance is a genuine break the
//     bind-time name would go on exempting forever;
//   - an EMPTY cached option id. Alone it misreads the upgrade: an earlier
//     release deleted that id on the pass that observed the loss, so a rule
//     reading "the binding HAS an id" finds nothing lost on a binding that
//     release degraded — and a readout that clears when it finds nothing lost
//     would clear a still-true degradation permanently. The same two shapes
//     meet during a rolling deploy, in either order.
//
// Together they say the thing that is actually meant: this column has never
// worked, so it cannot have broken. Only something that worked can break.
//
// The cached id is nevertheless KEPT on a loss: it is what `LostStates` gates
// the reflect with, and the half of the conjunction that keeps the loss
// re-derivable once the bind-time exemption has discharged.
//
// The Status FIELD's own id is refreshed the same way — it is resolved by
// name, so a field deleted and re-created is repaired rather than fatal.
//
// A labels-only binding (no Status field at bind time) has no vocabulary and
// is left alone. A binding whose Status field has vanished loses every state
// it had resolved: there is nothing left to write into.
func (b *BoardBinding) ReconcileStatusOptions(project Project) StatusVocabularyRepair {
	var rep StatusVocabularyRepair
	if b == nil || b.StatusFieldID == "" {
		return rep
	}
	field, hasField := project.Field(ProjectStatusFieldName)
	if hasField && !field.SingleSelect() {
		hasField = false // a Status field with no options can serve no column
	}

	mapping := append([]StatusMapping(nil), b.Mapping()...)
	options := make(map[string]string, len(b.StatusOptions))
	for state, id := range b.StatusOptions {
		options[state] = id
	}

	// 1. Resolve every mapped column against the live field — by cached id
	// first (a rename keeps it), then by name (a re-created column keeps that).
	resolved := make(map[string]bool, len(mapping))
	var missing []string
	for i, m := range mapping {
		if !hasField {
			missing = append(missing, m.Status)
			continue
		}
		id := options[m.State]
		if opt, ok := field.OptionByID(id); id != "" && ok {
			resolved[m.State] = true
			if !strings.EqualFold(strings.TrimSpace(opt.Name), strings.TrimSpace(m.Status)) {
				rep.Renamed = append(rep.Renamed, StatusRename{State: m.State, From: m.Status, To: opt.Name})
				mapping[i].Status = opt.Name
				rep.changed = true
			}
			continue
		}
		opt, ok := field.Option(m.Status)
		if !ok {
			missing = append(missing, m.Status)
			continue
		}
		resolved[m.State] = true
		options[m.State] = opt.ID
		if id == "" {
			rep.Adopted = append(rep.Adopted, m.State)
		} else {
			rep.Rebound = append(rep.Rebound, m.State)
		}
		rep.changed = true
	}
	sort.Strings(missing) // stable report, like the bind's own

	// 2. Recover the bind-time accepted set for a binding written before it
	// was recorded, so step 3 can tell "never had one" from "lost one".
	if b.UnresolvedAtBind == nil {
		b.UnresolvedAtBind = reconstructUnresolvedAtBind(b)
		rep.changed = true
	}
	accepted := make(map[string]bool, len(b.UnresolvedAtBind))
	for _, s := range b.UnresolvedAtBind {
		accepted[foldName(s)] = true
	}

	// 3. LOST: a mapped column the board serves under NEITHER its cached id
	// nor its name, that the bind did not accept as absent AND that has never
	// resolved since. Both halves of that second clause are load-bearing — see
	// the doc comment.
	//
	// The id is read off `b.StatusOptions`, the binding as it stands, not off
	// the `options` working copy step 1 mutates: the question is what this
	// state had resolved to BEFORE this pass. Identical today (step 1 only
	// writes ids for states that then short-circuit as resolved), but it says
	// what it means and cannot be broken by a later edit to the resolve loop.
	for _, m := range mapping {
		if resolved[m.State] || (accepted[foldName(m.Status)] && b.StatusOptions[m.State] == "") {
			continue
		}
		rep.Lost = append(rep.Lost, LostColumn{State: m.State, Status: m.Status})
	}

	if !sameStringSet(missing, b.MissingStatuses) {
		rep.changed = true
	}
	if hasField && field.ID != "" && field.ID != b.StatusFieldID {
		// The field was re-created. Its NAME is what resolves it, exactly as
		// at bind time, so this is repairable rather than fatal.
		rep.FieldRebound = true
		rep.changed = true
		b.StatusFieldID = field.ID
	}
	if !rep.changed {
		return rep
	}
	b.StatusMapping, b.StatusOptions, b.MissingStatuses = mapping, options, missing
	return rep
}

// reconstructUnresolvedAtBind recovers the bind-time accepted set for a binding
// stored before it was recorded.
//
// It reads the BINDING, never the live board: a mapped column with no cached
// option id is one the bind could not resolve and did not refuse over, which is
// the definition of the set. The board's current shape is the wrong oracle —
// "what the board lacks NOW" and "what the bind accepted" diverge exactly when
// something broke in the upgrade window, and diverge catastrophically when the
// Status field has vanished (every mapped column lacks a home, and recording
// all of them as accepted persists a lie the operator reads back on the binding
// API). Independent of the board, this cannot be poisoned by one bad pass.
//
// It is deliberately ASYMMETRIC on the binding's own health:
//
//   - a HEALTHY legacy binding: a column it never resolved is one it has always
//     run without, so it was accepted. A three-column board bound with the
//     five-column default map must not start reading as broken on an upgrade;
//   - a DEGRADED one: NOTHING was accepted. Its record already says a column
//     broke, and the release that flagged it also deleted that column's cached
//     id — so from the outside "accepted at bind" and "lost last week" are the
//     same shape, and the only safe reading of "I cannot tell" is to keep the
//     degradation the binding itself declared.
//
// That is the rule in one line: a health flag is never cleared by the absence
// of the evidence that would have kept it.
func reconstructUnresolvedAtBind(b *BoardBinding) []string {
	out := []string{} // non-nil: reconstructed-as-empty, not un-recorded
	if b.Degraded() {
		return out
	}
	for _, m := range b.Mapping() {
		if b.StatusOptions[m.State] == "" {
			out = append(out, m.Status)
		}
	}
	sort.Strings(out) // stable report, like the bind's own
	return out
}

// sameStringSet compares two already-sorted name lists.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
