package forge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// BindBoard turns a human-readable board address into the opaque ids every
// later write uses (ADR-097 §5).
//
// It is the ONE place a name becomes an id. Everything it can get wrong is
// silent afterwards — a mistyped column resolves to no option and the status
// projection quietly does nothing, a credential without the grant fails on the
// first write hours later — so each of those fails HERE, naming what could not
// be resolved.
//
// It reads the board and returns a binding; it does not store one. Persistence
// (and the tenant-authorization that must precede it) belongs to the caller.

// BindRequest describes the board to bind and the policy to bind it with.
type BindRequest struct {
	TenantID     string
	Provider     Provider
	Ref          ProjectRef
	ConnectionID string

	// StatusMap is the operator's `column → native state` map. Empty uses the
	// shipped five-column vocabulary. Must be injective (§2).
	StatusMap map[string]string

	// LabelFields overrides which single-select fields land as labels. Empty
	// uses the default (Area / Mode / Priority); only the fields the board
	// actually carries are bound.
	LabelFields []LabelField

	// SyncEvery is the reconciliation interval. nil = the default; an explicit
	// 0 = OFF. A pointer, because "unset" and "the operator turned it off" are
	// different answers and a bare 0 cannot tell them apart.
	SyncEvery *time.Duration
}

func (r BindRequest) validate() error {
	if strings.TrimSpace(r.TenantID) == "" {
		return errors.New("forge: bind board: tenant is required")
	}
	if !r.Provider.Valid() {
		return fmt.Errorf("forge: bind board: provider %q is not supported", r.Provider)
	}
	if err := r.Ref.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.ConnectionID) == "" {
		return errors.New("forge: bind board: a forge connection is required")
	}
	if r.SyncEvery != nil && *r.SyncEvery < 0 {
		return fmt.Errorf("forge: bind board: sync interval must be >= 0 (0 = off), got %v", *r.SyncEvery)
	}
	return nil
}

// BindBoard reads the board and resolves the binding.
func BindBoard(ctx context.Context, bc BoardClient, req BindRequest) (BoardBinding, error) {
	if bc == nil {
		return BoardBinding{}, errors.New("forge: bind board: no board client")
	}
	if err := req.validate(); err != nil {
		return BoardBinding{}, err
	}
	mapping, err := bindMapping(req)
	if err != nil {
		return BoardBinding{}, err
	}
	project, err := bc.GetProject(ctx, req.Ref)
	if err != nil {
		return BoardBinding{}, fmt.Errorf("forge: bind board %s: %w", req.Ref, err)
	}

	b := BoardBinding{
		TenantID: req.TenantID, Provider: req.Provider,
		Owner: req.Ref.Owner, OwnerKind: req.Ref.OwnerKind.OrDefault(), Number: req.Ref.Number,
		ConnectionID:  req.ConnectionID,
		ProjectID:     project.ID,
		ProjectTitle:  project.Title,
		ProjectURL:    project.URL,
		StatusMapping: mapping,
		SyncEvery:     DefaultBoardSyncEvery,
	}
	if req.SyncEvery != nil {
		b.SyncEvery = *req.SyncEvery
	}

	if err := bindStatus(&b, project, mapping); err != nil {
		return BoardBinding{}, err
	}
	b.LabelFields = bindLabelFields(project, req.LabelFields)

	if err := b.Validate(); err != nil {
		return BoardBinding{}, err
	}
	return b, nil
}

// bindMapping resolves the effective status map, refusing a non-injective one
// through the SAME builder every other entry point uses.
func bindMapping(req BindRequest) ([]StatusMapping, error) {
	if len(req.StatusMap) == 0 {
		return DefaultStatusMapping(), nil
	}
	m, err := StatusMappingFromMap(req.StatusMap)
	if err != nil {
		return nil, fmt.Errorf("forge: bind board: %w", err)
	}
	return m, nil
}

// bindStatus resolves the status field and each mapped column's option id.
//
// Three outcomes, deliberately distinct:
//   - no Status field at all → bind for LABELS only, with no status projection
//     (a legitimate board shape; the caller sees an empty StatusFieldID);
//   - some columns missing → bind, and REPORT them (the covered half works);
//   - NONE of the mapped columns present → refuse. That is not a partial
//     board, it is a wrong map, and binding it would ship a projection that is
//     inert from its first minute.
func bindStatus(b *BoardBinding, project Project, mapping []StatusMapping) error {
	field, ok := project.Field(ProjectStatusFieldName)
	if !ok {
		return nil
	}
	b.StatusFieldID = field.ID
	b.StatusOptions = map[string]string{}
	var missing []string
	for _, m := range mapping {
		opt, ok := field.Option(m.Status)
		if !ok {
			missing = append(missing, m.Status)
			continue
		}
		b.StatusOptions[m.State] = opt.ID
	}
	if len(b.StatusOptions) == 0 {
		have := make([]string, 0, len(field.Options))
		for _, o := range field.Options {
			have = append(have, o.Name)
		}
		return fmt.Errorf("forge: bind board %s: none of the mapped columns (%s) exist in the %s field, which has [%s]",
			b.Ref(), strings.Join(missing, ", "), ProjectStatusFieldName, strings.Join(have, ", "))
	}
	sort.Strings(missing) // stable report
	b.MissingStatuses = missing
	return nil
}

// bindLabelFields binds only the label fields the board actually carries: a
// bound field the board lacks would promise a label that never arrives.
func bindLabelFields(project Project, want []LabelField) []BoundLabelField {
	if len(want) == 0 {
		want = DefaultLabelFields()
	}
	var out []BoundLabelField
	for _, lf := range want {
		f, ok := project.Field(lf.Field)
		if !ok || !f.SingleSelect() {
			continue
		}
		out = append(out, BoundLabelField{FieldID: f.ID, Name: f.Name, Prefix: lf.Prefix})
	}
	return out
}
