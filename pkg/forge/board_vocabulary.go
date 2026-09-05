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
}

// Vocabulary reads the binding's cached status vocabulary.
func (b BoardBinding) Vocabulary() StatusVocabulary {
	return StatusVocabulary{
		Mapping: b.StatusMapping, Options: b.StatusOptions,
		StatusFieldID: b.StatusFieldID, MissingStatuses: b.MissingStatuses,
	}
}

// SetVocabulary writes it back.
func (b *BoardBinding) SetVocabulary(v StatusVocabulary) {
	b.StatusMapping, b.StatusOptions = v.Mapping, v.Options
	b.StatusFieldID, b.MissingStatuses = v.StatusFieldID, v.MissingStatuses
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
	// Lost are the columns that answer to NEITHER their cached id nor their
	// name. Nothing here can repair that: their option is dropped so the
	// reflect stops retrying them, and the binding is marked degraded.
	Lost []LostColumn
}

// Changed reports whether the repair altered the vocabulary at all — i.e.
// whether the caller owes the store a write.
func (r StatusVocabularyRepair) Changed() bool {
	return len(r.Renamed)+len(r.Rebound)+len(r.Adopted)+len(r.Lost) > 0
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
//  3. else ⇒ LOST. Drop the dead id so nothing retries it, and report it.
//
// The Status FIELD's own id is refreshed the same way — it is resolved by
// name, so a field deleted and re-created is repaired rather than fatal.
//
// A labels-only binding (no Status field at bind time) has no vocabulary and
// is left alone. A binding whose Status field has vanished loses every state:
// there is nothing left to write into.
func (b *BoardBinding) ReconcileStatusOptions(project Project) StatusVocabularyRepair {
	var rep StatusVocabularyRepair
	if b == nil || b.StatusFieldID == "" {
		return rep
	}
	field, ok := project.Field(ProjectStatusFieldName)
	if !ok || !field.SingleSelect() {
		// The field itself is gone. Every cached id is unwritable; say so once
		// rather than failing one write per card per pass.
		for _, m := range b.Mapping() {
			if b.StatusOptions[m.State] != "" {
				rep.Lost = append(rep.Lost, LostColumn{State: m.State, Status: m.Status})
			}
		}
		if len(rep.Lost) > 0 {
			b.StatusOptions = map[string]string{}
		}
		return rep
	}

	mapping := append([]StatusMapping(nil), b.Mapping()...)
	options := make(map[string]string, len(b.StatusOptions))
	for state, id := range b.StatusOptions {
		options[state] = id
	}
	var missing []string
	for i, m := range mapping {
		id := options[m.State]
		if opt, ok := field.OptionByID(id); id != "" && ok {
			if !strings.EqualFold(strings.TrimSpace(opt.Name), strings.TrimSpace(m.Status)) {
				rep.Renamed = append(rep.Renamed, StatusRename{State: m.State, From: m.Status, To: opt.Name})
				mapping[i].Status = opt.Name
			}
			continue
		}
		opt, ok := field.Option(m.Status)
		switch {
		case !ok:
			missing = append(missing, m.Status)
			if id != "" {
				rep.Lost = append(rep.Lost, LostColumn{State: m.State, Status: m.Status})
				delete(options, m.State)
			}
		case id == "":
			options[m.State] = opt.ID
			rep.Adopted = append(rep.Adopted, m.State)
		default:
			options[m.State] = opt.ID
			rep.Rebound = append(rep.Rebound, m.State)
		}
	}
	sort.Strings(missing) // stable report, like the bind's own
	if field.ID != "" && field.ID != b.StatusFieldID {
		// The field was re-created. Its NAME is what resolves it, exactly as
		// at bind time, so this is repairable rather than fatal.
		rep.Rebound = append(rep.Rebound, ProjectStatusFieldName)
		b.StatusFieldID = field.ID
	}
	if !rep.Changed() {
		return rep
	}
	b.StatusMapping, b.StatusOptions, b.MissingStatuses = mapping, options, missing
	return rep
}
