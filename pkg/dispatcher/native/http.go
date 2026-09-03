package native

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// BoardAPI serves the kanban REST surface against a BoardStore resolved
// PER REQUEST. Local/self-hosted mode resolves a single constant store
// (the filesystem *Store); cloud mode resolves the caller's tenant board
// (boardmongo.Store keyed by team) so one server serves every team's board
// from the same routes. The handlers only touch the BoardStore interface
// (+ the optional BoardAdmin / commentDispatcherSource capabilities), so the
// same code drives both backends.
type BoardAPI struct {
	// Resolve returns the board store for this request, or an error. A nil
	// store (no error) is treated as "board not available" (404). In cloud
	// mode it extracts the team from the path/identity and returns
	// CloudBoardFor(team); the membership gate lives in the mount wrapper.
	Resolve func(r *http.Request) (BoardStore, error)
}

// BoardAdmin is the optional board-CONFIG-mutation capability: editing
// columns, custom fields, saved views and the label vocabulary, including
// the cascades to issues (a column rename follows its cards). The
// filesystem *Store implements it; the cloud Mongo store does not yet, so
// cloud board-config editing returns 501 (the board itself can still be
// replaced wholesale via PUT /board, which is a plain BoardStore.SetBoard).
type BoardAdmin interface {
	AddState(st State) error
	RenameState(from, to string) (int, error)
	DeleteState(name, migrateTo string) (int, error)
	UpdateState(name string, p StatePatch) error
	ReorderStates(order []string) error
	AddField(f Field) error
	UpdateField(name string, p FieldPatch) error
	RenameField(from, to string) (int, error)
	DeleteField(name string) (int, error)
	ReorderFields(order []string) error
	SaveView(v View) error
	DeleteView(name string) error
	RenameLabel(from, to string) (int, error)
	MergeLabels(from, to string) (int, error)
	DeleteLabel(label string) (int, error)
}

// commentDispatcherSource is implemented by *Store: it exposes the installed
// slash-command resolver so handleAddComment can auto-dispatch a leading
// "/command". A store that doesn't implement it (boardmongo) simply records
// the comment — cloud command-dispatch flows through invocation_dispatch.go.
type commentDispatcherSource interface {
	getCommentDispatcher() CommentDispatcher
}

var errBoardAdminUnavailable = errors.New("board configuration editing is not available on this server yet")

// RegisterRoutes mounts the native tracker's REST surface on mux under
// prefix against a single constant store. Pass "" to mount at the mux root.
func (s *Store) RegisterRoutes(mux *http.ServeMux, prefix string) {
	s.RegisterRoutesWithMiddleware(mux, prefix, nil)
}

// RegisterRoutesWithMiddleware mounts the routes for this constant store
// through a caller-supplied wrapper (typically the studio server's
// requireAuth). It delegates to a BoardAPI whose resolver always returns s,
// so the self-hosted single-board behaviour is unchanged.
func (s *Store) RegisterRoutesWithMiddleware(mux *http.ServeMux, prefix string, wrap func(http.Handler) http.Handler) {
	(&BoardAPI{Resolve: func(*http.Request) (BoardStore, error) { return s, nil }}).
		RegisterRoutesWithMiddleware(mux, prefix, wrap)
}

// RegisterRoutesWithMiddleware mounts the kanban REST routes under prefix,
// each wrapped by wrap (nil = identity). One pattern per (method, path) so
// Go 1.22's ServeMux doesn't flag ambiguities against other catch-all
// method routes. The prefix may itself contain path wildcards (e.g.
// "/api/teams/{tid}/board") that the resolver reads back via PathValue.
func (h *BoardAPI) RegisterRoutesWithMiddleware(mux *http.ServeMux, prefix string, wrap func(http.Handler) http.Handler) {
	p := strings.TrimSuffix(prefix, "/")
	if wrap == nil {
		wrap = func(hh http.Handler) http.Handler { return hh }
	}
	mux.Handle("GET "+p+"/issues", wrap(http.HandlerFunc(h.handleListIssues)))
	mux.Handle("POST "+p+"/issues", wrap(http.HandlerFunc(h.handleCreateIssue)))
	mux.Handle("GET "+p+"/issues/{id}", wrap(http.HandlerFunc(h.handleGetIssue)))
	mux.Handle("PATCH "+p+"/issues/{id}", wrap(http.HandlerFunc(h.handlePatchIssue)))
	mux.Handle("DELETE "+p+"/issues/{id}", wrap(http.HandlerFunc(h.handleDeleteIssue)))
	mux.Handle("POST "+p+"/issues/{id}/transition", wrap(http.HandlerFunc(h.handleTransitionIssue)))
	mux.Handle("POST "+p+"/issues/{id}/comments", wrap(http.HandlerFunc(h.handleAddComment)))
	mux.Handle("GET "+p+"/labels", wrap(http.HandlerFunc(h.handleListLabels)))
	mux.Handle("POST "+p+"/labels/rename", wrap(http.HandlerFunc(h.handleRenameLabel)))
	mux.Handle("POST "+p+"/labels/merge", wrap(http.HandlerFunc(h.handleMergeLabels)))
	mux.Handle("DELETE "+p+"/labels/{label}", wrap(http.HandlerFunc(h.handleDeleteLabel)))
	mux.Handle("GET "+p+"/board", wrap(http.HandlerFunc(h.handleGetBoard)))
	mux.Handle("PUT "+p+"/board", wrap(http.HandlerFunc(h.handlePutBoard)))
	mux.Handle("POST "+p+"/board/states", wrap(http.HandlerFunc(h.handleAddState)))
	mux.Handle("POST "+p+"/board/states/reorder", wrap(http.HandlerFunc(h.handleReorderStates)))
	mux.Handle("PATCH "+p+"/board/states/{name}", wrap(http.HandlerFunc(h.handleUpdateState)))
	mux.Handle("DELETE "+p+"/board/states/{name}", wrap(http.HandlerFunc(h.handleDeleteState)))
	mux.Handle("POST "+p+"/board/fields", wrap(http.HandlerFunc(h.handleAddField)))
	mux.Handle("POST "+p+"/board/fields/reorder", wrap(http.HandlerFunc(h.handleReorderFields)))
	mux.Handle("PATCH "+p+"/board/fields/{name}", wrap(http.HandlerFunc(h.handleUpdateField)))
	mux.Handle("DELETE "+p+"/board/fields/{name}", wrap(http.HandlerFunc(h.handleDeleteField)))
	mux.Handle("POST "+p+"/board/views", wrap(http.HandlerFunc(h.handleSaveView)))
	mux.Handle("DELETE "+p+"/board/views/{name}", wrap(http.HandlerFunc(h.handleDeleteView)))
}

// store resolves the per-request board store, writing the appropriate error
// and returning ok=false when unavailable.
func (h *BoardAPI) store(w http.ResponseWriter, r *http.Request) (BoardStore, bool) {
	s, err := h.Resolve(r)
	if err != nil {
		writeErr(w, statusForErr(err), err)
		return nil, false
	}
	if s == nil {
		writeErr(w, http.StatusNotFound, errors.New("board not available"))
		return nil, false
	}
	return s, true
}

// admin type-asserts the BoardAdmin capability, writing 501 when the
// resolved store can't edit board config (cloud boardmongo today).
func (h *BoardAPI) admin(w http.ResponseWriter, s BoardStore) (BoardAdmin, bool) {
	a, ok := s.(BoardAdmin)
	if !ok {
		writeErr(w, http.StatusNotImplemented, errBoardAdminUnavailable)
		return nil, false
	}
	return a, true
}

// ---------------------------------------------------------------------------
// /issues
// ---------------------------------------------------------------------------

type issueCreateReq struct {
	Title    string         `json:"title"`
	Body     string         `json:"body,omitempty"`
	State    string         `json:"state,omitempty"`
	Labels   []string       `json:"labels,omitempty"`
	Priority int            `json:"priority,omitempty"`
	Assignee string         `json:"assignee,omitempty"`
	Blockers []string       `json:"blockers,omitempty"`
	Fields   map[string]any `json:"fields,omitempty"`
	// External links the card to a forge repo at creation (repo-first
	// board scoping); number/url/state stay empty until push-to-forge
	// or the sync worker populate them.
	External *ExternalRef      `json:"external,omitempty"`
	Bot      string            `json:"bot,omitempty"`
	BotArgs  map[string]string `json:"bot_args,omitempty"`
}

func (h *BoardAPI) handleListIssues(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	filter := ListFilter{
		States: r.URL.Query()["state"],
		Labels: r.URL.Query()["label"],
	}
	if a := r.URL.Query().Get("assignee"); a != "" {
		filter.Assignee = a
	}
	issues, err := s.List(filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, issues)
}

func (h *BoardAPI) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	var in issueCreateReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.Create(Issue{
		Title:    in.Title,
		Body:     in.Body,
		State:    in.State,
		Labels:   in.Labels,
		Priority: in.Priority,
		Assignee: in.Assignee,
		Blockers: in.Blockers,
		Fields:   in.Fields,
		External: in.External,
		Bot:      in.Bot,
		BotArgs:  in.BotArgs,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ---------------------------------------------------------------------------
// /issues/{id} and /issues/{id}/transition
// ---------------------------------------------------------------------------

type issuePatchReq struct {
	Title    *string        `json:"title,omitempty"`
	Body     *string        `json:"body,omitempty"`
	Labels   *[]string      `json:"labels,omitempty"`
	Priority *int           `json:"priority,omitempty"`
	Assignee *string        `json:"assignee,omitempty"`
	Blockers *[]string      `json:"blockers,omitempty"`
	Fields   map[string]any `json:"fields,omitempty"`
	// External, when present, re-links the card's forge repo (absent =
	// unchanged; the store keeps sync-owned number/url/state semantics).
	External *ExternalRef       `json:"external,omitempty"`
	Bot      *string            `json:"bot,omitempty"`
	BotArgs  *map[string]string `json:"bot_args,omitempty"`
}

type transitionReq struct {
	To string `json:"to"`
}

// resolvePathID extracts the {id} segment, runs prefix resolution against
// the resolved store, and returns the full ID. On miss/ambiguity writes the
// appropriate HTTP error and returns "" + false.
func resolvePathID(s BoardStore, w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := r.PathValue("id")
	if raw == "" {
		http.NotFound(w, r)
		return "", false
	}
	id, err := s.Resolve(raw)
	if err != nil {
		if errors.Is(err, tracker.ErrNotFound) {
			http.Error(w, "issue not found", http.StatusNotFound)
			return "", false
		}
		writeErr(w, http.StatusBadRequest, err)
		return "", false
	}
	return id, true
}

func (h *BoardAPI) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	id, ok := resolvePathID(s, w, r)
	if !ok {
		return
	}
	iss, err := s.Get(id)
	if err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, iss)
}

func (h *BoardAPI) handlePatchIssue(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	id, ok := resolvePathID(s, w, r)
	if !ok {
		return
	}
	var in issuePatchReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.Update(id, Patch{
		Title:    in.Title,
		Body:     in.Body,
		Labels:   in.Labels,
		Priority: in.Priority,
		Assignee: in.Assignee,
		Blockers: in.Blockers,
		Fields:   in.Fields,
		External: in.External,
		Bot:      in.Bot,
		BotArgs:  in.BotArgs,
	})
	if err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *BoardAPI) handleDeleteIssue(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	id, ok := resolvePathID(s, w, r)
	if !ok {
		return
	}
	if err := s.Delete(id); err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *BoardAPI) handleTransitionIssue(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	id, ok := resolvePathID(s, w, r)
	if !ok {
		return
	}
	var in transitionReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.To == "" {
		writeErr(w, http.StatusBadRequest, errors.New("transition: to is required"))
		return
	}
	// The board REST API is an operator surface (the /board drag):
	// leaving a terminal state is the sanctioned reopen.
	iss, err := SetStateOrReopen(s, id, in.To)
	if err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, iss)
}

// commentReq is the body of POST /issues/{id}/comments. Body is the comment
// text. The optional Bot / BotArgs / TransitionTo fields let a caller record
// the comment AND dispatch a run in one request. The native store stays
// decoupled from the bot registry — command→bot resolution happens in the
// caller (or the installed CommentDispatcher), not here.
type commentReq struct {
	Author       string            `json:"author,omitempty"`
	Body         string            `json:"body"`
	Bot          *string           `json:"bot,omitempty"`
	BotArgs      map[string]string `json:"bot_args,omitempty"`
	TransitionTo string            `json:"transition_to,omitempty"`
}

func (h *BoardAPI) handleAddComment(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	id, ok := resolvePathID(s, w, r)
	if !ok {
		return
	}
	var in commentReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	author := in.Author
	if author == "" {
		author = "operator"
	}
	updated, _, err := s.AddComment(id, author, in.Body)
	if err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	bot := in.Bot
	botArgs := in.BotArgs
	transitionTo := in.TransitionTo
	// Auto-resolve a leading "/command" when the caller didn't pre-resolve a
	// bot AND the store exposes a comment dispatcher (the filesystem *Store
	// does; boardmongo doesn't, so cloud just records the comment and lets
	// invocation_dispatch.go handle commands).
	if bot == nil && botArgs == nil && updated != nil {
		if src, sok := s.(commentDispatcherSource); sok {
			if d := src.getCommentDispatcher(); d != nil {
				if rbot, rargs, rto, rok := d(*updated, in.Body); rok {
					bot = &rbot
					botArgs = rargs
					if transitionTo == "" {
						transitionTo = rto
					}
				}
			}
		}
	}
	// Optional one-shot dispatch: stamp bot + args, then move to the
	// requested state so the dispatcher picks the issue up. The stamp must
	// stay FIRST — the move is what makes the card dispatchable, and a
	// dispatcher claiming it between the two would launch the previous bot.
	if bot != nil || botArgs != nil {
		patch := Patch{}
		if bot != nil {
			patch.Bot = bot
		}
		if botArgs != nil {
			patch.BotArgs = &botArgs
		}
		if _, err := s.Update(id, patch); err != nil {
			writeErr(w, statusForErr(err), err)
			return
		}
	}
	// The move goes through the OPERATOR helper, like every other operator
	// surface the terminal-sink sweep converted (transition, CLI move,
	// pipeline actions, deps). This one was missed, and it is the surface
	// where it bites hardest: resolveBoardComment returns StateReady for
	// EVERY "/command", while the default give-up column (blocked) is a
	// sink. A bare SetState therefore answered 409 — after the comment and
	// the bot stamp above had already persisted, leaving the card mutated
	// but un-dispatched — and killed "retry a given-up card by commenting"
	// outright. Reachable on both twins via an explicit transition_to
	// (iterion remote board, the remote_issue_comment MCP tool). Reopen is
	// the sanctioned exit, and an operator's comment is entitled to it.
	if transitionTo != "" {
		if _, err := SetStateOrReopen(s, id, transitionTo); err != nil {
			writeErr(w, statusForErr(err), err)
			return
		}
	}
	iss, err := s.Get(id)
	if err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, iss)
}

// ---------------------------------------------------------------------------
// /labels
// ---------------------------------------------------------------------------

// handleListLabels returns every label currently in use on the board with
// usage count + last-used timestamp. Read-only; auth is the mount wrapper.
func (h *BoardAPI) handleListLabels(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.AggregateLabels())
}

type labelRenameReq struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type labelOpResp struct {
	Touched int `json:"touched"`
}

func (h *BoardAPI) handleRenameLabel(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	var in labelRenameReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := a.RenameLabel(in.From, in.To)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, labelOpResp{Touched: n})
}

func (h *BoardAPI) handleMergeLabels(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	var in labelRenameReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := a.MergeLabels(in.From, in.To)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, labelOpResp{Touched: n})
}

func (h *BoardAPI) handleDeleteLabel(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	n, err := a.DeleteLabel(r.PathValue("label"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, labelOpResp{Touched: n})
}

// ---------------------------------------------------------------------------
// /board
// ---------------------------------------------------------------------------

func (h *BoardAPI) handleGetBoard(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

func (h *BoardAPI) handlePutBoard(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	var b Board
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.SetBoard(&b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

// stateUpdateReq is the PATCH /board/states/{name} body. A non-nil Name that
// differs from the path segment triggers a cascading rename (issues in the
// column follow); the remaining fields are applied afterward.
type stateUpdateReq struct {
	Name     *string `json:"name,omitempty"`
	Display  *string `json:"display,omitempty"`
	Color    *string `json:"color,omitempty"`
	Eligible *bool   `json:"eligible,omitempty"`
	Terminal *bool   `json:"terminal,omitempty"`
}

type reorderReq struct {
	Order []string `json:"order"`
}

// stateDeleteConflictResp is the 409 body returned when a non-empty column is
// deleted without a migration target. `count` lets the UI prompt "move N
// issues to…".
type stateDeleteConflictResp struct {
	Error string `json:"error"`
	Count int    `json:"count"`
}

func (h *BoardAPI) handleAddState(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	var st State
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.AddState(st); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

func (h *BoardAPI) handleUpdateState(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var in stateUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	target := name
	if in.Name != nil && *in.Name != name {
		if _, err := a.RenameState(name, *in.Name); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		target = *in.Name
	}
	if in.Display != nil || in.Color != nil || in.Eligible != nil || in.Terminal != nil {
		if err := a.UpdateState(target, StatePatch{
			Display:  in.Display,
			Color:    in.Color,
			Eligible: in.Eligible,
			Terminal: in.Terminal,
		}); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.Board())
}

func (h *BoardAPI) handleDeleteState(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if _, err := a.DeleteState(name, r.URL.Query().Get("migrate_to")); err != nil {
		if errors.Is(err, ErrStateNotEmpty) {
			count := 0
			for _, iss := range mustList(s) {
				if iss.State == name {
					count++
				}
			}
			writeJSON(w, http.StatusConflict, stateDeleteConflictResp{Error: err.Error(), Count: count})
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

func (h *BoardAPI) handleReorderStates(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	var in reorderReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.ReorderStates(in.Order); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

// mustList returns the current issues for the 409 count; on error it returns
// nil (count falls back to 0, still a valid conflict response).
func mustList(s BoardStore) []*Issue {
	issues, err := s.List(ListFilter{})
	if err != nil {
		return nil
	}
	return issues
}

// fieldUpdateReq is the PATCH /board/fields/{name} body. A non-nil Name that
// differs from the path triggers a cascading rename across issues; remaining
// attributes are applied afterward.
type fieldUpdateReq struct {
	Name       *string    `json:"name,omitempty"`
	Display    *string    `json:"display,omitempty"`
	Type       *FieldType `json:"type,omitempty"`
	Required   *bool      `json:"required,omitempty"`
	EnumValues *[]string  `json:"enum_values,omitempty"`
}

func (h *BoardAPI) handleAddField(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	var f Field
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.AddField(f); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

func (h *BoardAPI) handleUpdateField(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var in fieldUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	target := name
	if in.Name != nil && *in.Name != name {
		if _, err := a.RenameField(name, *in.Name); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		target = *in.Name
	}
	if in.Display != nil || in.Type != nil || in.Required != nil || in.EnumValues != nil {
		if err := a.UpdateField(target, FieldPatch{
			Display:    in.Display,
			Type:       in.Type,
			Required:   in.Required,
			EnumValues: in.EnumValues,
		}); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.Board())
}

func (h *BoardAPI) handleDeleteField(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	if _, err := a.DeleteField(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

func (h *BoardAPI) handleReorderFields(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	var in reorderReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.ReorderFields(in.Order); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

func (h *BoardAPI) handleSaveView(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	var v View
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.SaveView(v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

func (h *BoardAPI) handleDeleteView(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	a, ok := h.admin(w, s)
	if !ok {
		return
	}
	if err := a.DeleteView(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func statusForErr(err error) int {
	switch {
	case errors.Is(err, tracker.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, tracker.ErrTransitionRejected),
		errors.Is(err, tracker.ErrClaimConflict):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpx.WriteJSON(w, status, v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
