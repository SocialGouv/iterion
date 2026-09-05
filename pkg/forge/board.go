package forge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file defines the OPTIONAL project-board capability a provider's admin
// client may expose, on top of the mandatory Admin interface — the same
// type-asserted shape as IssueClient / PermissionClient / RepoCreator, so a
// provider without a project board degrades gracefully instead of stubbing it.
//
// A "project board" here is the forge's own cross-repo planning surface:
// GitHub Projects v2 today. It carries ITEMS (each backed by an issue or PR)
// and FIELDS (single-select columns like Status / Area / Priority). Iterion
// reads it to hydrate native cards and writes exactly one field back — the
// status — per ADR-097.

// ProjectOwnerKind distinguishes the two namespaces a project can live under.
// It is explicit rather than probed: GitHub's GraphQL entry point differs
// (`organization(login:)` vs `user(login:)`), and guessing would turn a typo
// into a silent lookup of the wrong owner.
type ProjectOwnerKind string

const (
	ProjectOwnerOrg  ProjectOwnerKind = "org"
	ProjectOwnerUser ProjectOwnerKind = "user"
)

// Valid reports whether k is one of the two known owner kinds. The zero value
// is NOT valid — callers normalise with OrDefault first.
func (k ProjectOwnerKind) Valid() bool {
	return k == ProjectOwnerOrg || k == ProjectOwnerUser
}

// OrDefault maps the zero value to ProjectOwnerOrg, leaving any other value
// (valid or not) untouched so validation still sees a typo.
func (k ProjectOwnerKind) OrDefault() ProjectOwnerKind {
	if k == "" {
		return ProjectOwnerOrg
	}
	return k
}

// ProjectRef identifies one project board: the owner login plus the project
// NUMBER shown in its URL. Number, not id: it is what a human reads off
// `/orgs/<owner>/projects/<n>` and it survives a project rename.
type ProjectRef struct {
	Owner     string           `json:"owner"`
	OwnerKind ProjectOwnerKind `json:"owner_kind,omitempty"`
	Number    int              `json:"number"`
}

// String renders the ref the way an operator types it: "owner/number".
func (r ProjectRef) String() string {
	if r.Owner == "" && r.Number == 0 {
		return ""
	}
	return r.Owner + "/" + strconv.Itoa(r.Number)
}

// Validate reports why a ref cannot be resolved, or nil.
func (r ProjectRef) Validate() error {
	if strings.TrimSpace(r.Owner) == "" {
		return errors.New("forge: project owner is required")
	}
	if r.Number <= 0 {
		return errors.New("forge: project number must be positive")
	}
	if k := r.OwnerKind.OrDefault(); !k.Valid() {
		return errors.New("forge: project owner kind must be org or user, got " + string(r.OwnerKind))
	}
	return nil
}

// ParseProjectRef parses the "owner/number" form an operator types
// (`--project SocialGouv/203`). The owner kind is not encoded in that form and
// stays at its zero value for the caller to set.
func ParseProjectRef(s string) (ProjectRef, error) {
	owner, num, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return ProjectRef{}, errors.New("forge: project must be <owner>/<number>, got " + s)
	}
	n, err := strconv.Atoi(strings.TrimSpace(num))
	if err != nil || n <= 0 {
		return ProjectRef{}, errors.New("forge: project number must be a positive integer, got " + num)
	}
	ref := ProjectRef{Owner: strings.TrimSpace(owner), Number: n}
	if err := ref.Validate(); err != nil {
		return ProjectRef{}, err
	}
	return ref, nil
}

// ProjectFieldOption is one choice of a single-select field. The ID is the
// provider's opaque handle — never guessed, always discovered by Name.
type ProjectFieldOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectField is one column definition on the board. DataType is the
// provider's own vocabulary ("SINGLE_SELECT", "TEXT", …), kept verbatim so a
// caller can tell "this field exists but is not selectable" from "absent".
type ProjectField struct {
	ID       string               `json:"id"`
	Name     string               `json:"name"`
	DataType string               `json:"data_type,omitempty"`
	Options  []ProjectFieldOption `json:"options,omitempty"`
}

// SingleSelect reports whether the field carries a fixed option set iterion
// can write.
func (f ProjectField) SingleSelect() bool { return len(f.Options) > 0 }

// Option finds an option by name, case-insensitively on the trimmed name, so a
// board writing "In Progress" binds the same as one writing "In progress".
func (f ProjectField) Option(name string) (ProjectFieldOption, bool) {
	want := foldName(name)
	for _, o := range f.Options {
		if foldName(o.Name) == want {
			return o, true
		}
	}
	return ProjectFieldOption{}, false
}

// OptionByID finds an option by its provider id.
func (f ProjectField) OptionByID(id string) (ProjectFieldOption, bool) {
	for _, o := range f.Options {
		if o.ID == id {
			return o, true
		}
	}
	return ProjectFieldOption{}, false
}

// Project is one board's definition: identity plus its field schema. Items are
// fetched separately (ListProjectItems) because a board can hold thousands.
type Project struct {
	ID     string         `json:"id"`
	Number int            `json:"number"`
	Title  string         `json:"title"`
	URL    string         `json:"url,omitempty"`
	Fields []ProjectField `json:"fields,omitempty"`
}

// Field finds a field by name, case-insensitively on the trimmed name.
func (p Project) Field(name string) (ProjectField, bool) {
	want := foldName(name)
	for _, f := range p.Fields {
		if foldName(f.Name) == want {
			return f, true
		}
	}
	return ProjectField{}, false
}

// ProjectItemContent is the forge object an item is backed by. Kind is
// "issue", "pull_request" or "draft"; a draft item has no repo or number,
// which is exactly why iterion skips it (it has no card to join on).
type ProjectItemContent struct {
	Kind   string `json:"kind,omitempty"`
	Repo   string `json:"repo,omitempty"` // "owner/repo"
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"` // "open" | "closed"
}

// Content kinds. They are iterion's normalized vocabulary, not the provider's.
const (
	ProjectContentIssue = "issue"
	ProjectContentPull  = "pull_request"
	ProjectContentDraft = "draft"
)

// ProjectItemField is one field VALUE on one item. UpdatedAt is the provider's
// own timestamp for that value — the load-bearing half of ADR-097's conflict
// rule, which needs to know when the status last changed, not when the row was
// last touched.
type ProjectItemField struct {
	FieldID   string    `json:"field_id"`
	FieldName string    `json:"field_name"`
	OptionID  string    `json:"option_id,omitempty"`
	Value     string    `json:"value,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// ProjectItem is one card on the board.
type ProjectItem struct {
	ID        string             `json:"id"`
	Content   ProjectItemContent `json:"content"`
	Fields    []ProjectItemField `json:"fields,omitempty"`
	Archived  bool               `json:"archived,omitempty"`
	UpdatedAt time.Time          `json:"updated_at,omitempty"`
}

// Field finds a field value by field name, case-insensitively.
func (it ProjectItem) Field(name string) (ProjectItemField, bool) {
	want := foldName(name)
	for _, f := range it.Fields {
		if foldName(f.FieldName) == want {
			return f, true
		}
	}
	return ProjectItemField{}, false
}

// ProjectItemListOptions pages ListProjectItems. Cursor is opaque and comes
// from a previous page's NextCursor.
type ProjectItemListOptions struct {
	PerPage int
	Cursor  string
}

// ProjectItemPage is one page of items plus the cursor to continue from.
type ProjectItemPage struct {
	Items      []ProjectItem `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
	HasNext    bool          `json:"has_next,omitempty"`
}

// BoardClient is the optional project-board capability, type-asserted off an
// Admin like IssueClient (`if bc, ok := forge.AsBoardClient(admin); ok`).
//
// It reads a board's schema and items and writes exactly one thing: a
// single-select field value. That asymmetry is deliberate (ADR-097) — iterion
// projects its own state onto the board's status column and never authors the
// board's content.
type BoardClient interface {
	// GetProject resolves a board by owner+number and returns its field
	// schema, including every single-select's option set.
	GetProject(ctx context.Context, ref ProjectRef) (Project, error)

	// ListProjectItems returns one page of the board's items with their field
	// values. Archived items are included and flagged, not filtered — the
	// caller decides, because "archived" means different things per workflow.
	ListProjectItems(ctx context.Context, ref ProjectRef, opts ProjectItemListOptions) (ProjectItemPage, error)

	// IssueContentID resolves a forge issue ("owner/repo", number) to the
	// opaque handle AddItem takes. Separate from AddItem because the provider
	// may need a different call (or none) to obtain it.
	IssueContentID(ctx context.Context, repo string, number int) (string, error)

	// AddItem puts a piece of content on the board. Adding content already on
	// the board is not an error: it returns the existing item.
	AddItem(ctx context.Context, projectID, contentID string) (ProjectItem, error)

	// SetSingleSelect writes one single-select field value on one item.
	SetSingleSelect(ctx context.Context, projectID, itemID, fieldID, optionID string) error
}

// AsBoardClient type-asserts the project-board capability off an Admin. It
// exists so call sites read as a question about capability rather than about
// Go typing, and so the assertion has one greppable name.
func AsBoardClient(a Admin) (BoardClient, bool) {
	bc, ok := a.(BoardClient)
	return bc, ok
}

// ErrProjectNotFound reports a board that the credential cannot resolve —
// wrong owner, wrong number, or no visibility.
var ErrProjectNotFound = errors.New("forge: project not found")

// ---- the status vocabulary joining a board to the native kanban ----

// ProjectStatusFieldName is the field iterion reflects its own state onto.
// A board that does not carry it can still be bound (labels only); it just
// gets no status projection.
const ProjectStatusFieldName = "Status"

// StatusMapping is one (board status ⇄ native state) pair. The mapping is
// INJECTIVE in both directions by construction (ADR-097): a native state
// outside it is inert — the reflect writes nothing rather than collapsing
// `review` onto "In progress" and having the next import drag the card back
// out of it.
type StatusMapping struct {
	Status string `json:"status"` // the board's single-select option name
	State  string `json:"state"`  // the native board's column name
}

// DefaultStatusMapping is the vocabulary the shipped native board
// (pkg/dispatcher/native) and the AGENTS.md project board agree on. It is the
// ONE place the five pairs are written down; the binding stores the option ids
// discovered for them, never the names again.
func DefaultStatusMapping() []StatusMapping {
	return []StatusMapping{
		{Status: "Inbox", State: "inbox"},
		{Status: "Planned", State: "ready"},
		{Status: "In progress", State: "in_progress"},
		{Status: "Blocked", State: "blocked"},
		{Status: "Done", State: "done"},
	}
}

// StatusMappingFromMap builds a mapping from an operator's `column → state`
// map — the escape hatch that keeps the five shipped names a DEFAULT and not a
// fence: a board whose columns read "Todo"/"Doing"/"Shipped" binds by naming
// them, with no code change.
//
// It REFUSES a non-injective map, naming the collision. Two columns pointing
// at one state would make the reverse direction ambiguous: the reflect would
// have to pick one, and the next import would read the other back and undo the
// transition — the oscillation the injectivity rule exists to prevent.
//
// The result is sorted by state so a binding's stored map, and anything
// rendered from it, is stable across runs.
func StatusMappingFromMap(m map[string]string) ([]StatusMapping, error) {
	if len(m) == 0 {
		return nil, errors.New("forge: status map is empty")
	}
	byState := map[string]string{}
	bySameStatus := map[string]bool{}
	out := make([]StatusMapping, 0, len(m))
	for status, state := range m {
		s := strings.TrimSpace(status)
		st := strings.TrimSpace(state)
		if s == "" || st == "" {
			return nil, fmt.Errorf("forge: status map has an empty entry (%q → %q)", status, state)
		}
		if bySameStatus[foldName(s)] {
			return nil, fmt.Errorf("forge: status map names column %q twice", s)
		}
		bySameStatus[foldName(s)] = true
		if prev, dup := byState[foldName(st)]; dup {
			return nil, fmt.Errorf("forge: status map is not injective: columns %q and %q both map to state %q", prev, s, st)
		}
		byState[foldName(st)] = s
		out = append(out, StatusMapping{Status: s, State: st})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].State < out[j].State })
	return out, nil
}

// StatusForState returns the board status name mapped to a native state, and
// false when the state is unmapped (inert).
func StatusForState(m []StatusMapping, state string) (string, bool) {
	want := foldName(state)
	for _, p := range m {
		if foldName(p.State) == want {
			return p.Status, true
		}
	}
	return "", false
}

// StateForStatus returns the native state mapped to a board status name, and
// false when the status is unmapped.
func StateForStatus(m []StatusMapping, status string) (string, bool) {
	want := foldName(status)
	for _, p := range m {
		if foldName(p.Status) == want {
			return p.State, true
		}
	}
	return "", false
}

// ---- the label vocabulary for the board's other single-select fields ----

// DefaultLabelFields are the single-select fields imported onto cards as
// namespaced labels, with the prefix each uses. Read-only by decision
// (ADR-097 §3): the import writes them, nothing writes them back.
//
// The prefixes are declared board-local in pkg/server so a plain issue import
// — which mirrors the REPO's labels verbatim — cannot strip them off.
func DefaultLabelFields() []LabelField {
	return []LabelField{
		{Field: "Area", Prefix: "area:"},
		{Field: "Mode", Prefix: "mode:"},
		{Field: "Priority", Prefix: "prio:"},
	}
}

// LabelField binds one board field to the label namespace its values land in.
type LabelField struct {
	Field  string `json:"field"`
	Prefix string `json:"prefix"`
}

// FieldLabel renders one field value as its card label: the prefix plus the
// slugified value ("cloud/ops" → "area:cloud-ops"). An empty value yields "",
// meaning "no label", never a dangling "area:".
func FieldLabel(prefix, value string) string {
	slug := SlugifyLabelValue(value)
	if slug == "" {
		return ""
	}
	return prefix + slug
}

// SlugifyLabelValue lowercases a field value and collapses everything that is
// not a letter, digit, '.', '_' or '-' into single dashes, so a board's free
// prose ("In progress", "cloud/ops") becomes a label a matcher can carry.
func SlugifyLabelValue(v string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(v)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		case r == '-':
			dash = b.Len() > 0
		default:
			dash = b.Len() > 0
		}
	}
	return b.String()
}

// foldName is the name-matching rule used everywhere in this file: trimmed and
// case-insensitive, so a board renamed "STATUS" or " Status " still binds.
func foldName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
