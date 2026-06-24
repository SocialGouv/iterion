package native

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// RegisterRoutes mounts the native tracker's REST surface on mux under
// prefix. Pass "" to mount at the mux root.
//
// We register one pattern per (method, path) so Go 1.22's ServeMux
// doesn't flag ambiguities against other catch-all method routes
// (e.g. the server's `OPTIONS /api/` CORS preflight).
func (s *Store) RegisterRoutes(mux *http.ServeMux, prefix string) {
	s.RegisterRoutesWithMiddleware(mux, prefix, nil)
}

// RegisterRoutesWithMiddleware mounts the routes through a caller-
// supplied wrapper (typically the studio server's requireAuth). nil
// wraps each handler in the identity — same behaviour as RegisterRoutes.
// Used so the studio server can gate every native-tracker call behind
// JWT auth without introducing a single bare-path catch-all that
// would conflict with the server's existing method-specific patterns.
func (s *Store) RegisterRoutesWithMiddleware(mux *http.ServeMux, prefix string, wrap func(http.Handler) http.Handler) {
	p := strings.TrimSuffix(prefix, "/")
	if wrap == nil {
		wrap = func(h http.Handler) http.Handler { return h }
	}
	mux.Handle("GET "+p+"/issues", wrap(http.HandlerFunc(s.handleListIssues)))
	mux.Handle("POST "+p+"/issues", wrap(http.HandlerFunc(s.handleCreateIssue)))
	mux.Handle("GET "+p+"/issues/{id}", wrap(http.HandlerFunc(s.handleGetIssue)))
	mux.Handle("PATCH "+p+"/issues/{id}", wrap(http.HandlerFunc(s.handlePatchIssue)))
	mux.Handle("DELETE "+p+"/issues/{id}", wrap(http.HandlerFunc(s.handleDeleteIssue)))
	mux.Handle("POST "+p+"/issues/{id}/transition", wrap(http.HandlerFunc(s.handleTransitionIssue)))
	mux.Handle("POST "+p+"/issues/{id}/comments", wrap(http.HandlerFunc(s.handleAddComment)))
	mux.Handle("GET "+p+"/labels", wrap(http.HandlerFunc(s.handleListLabels)))
	mux.Handle("POST "+p+"/labels/rename", wrap(http.HandlerFunc(s.handleRenameLabel)))
	mux.Handle("POST "+p+"/labels/merge", wrap(http.HandlerFunc(s.handleMergeLabels)))
	mux.Handle("DELETE "+p+"/labels/{label}", wrap(http.HandlerFunc(s.handleDeleteLabel)))
	mux.Handle("GET "+p+"/board", wrap(http.HandlerFunc(s.handleGetBoard)))
	mux.Handle("PUT "+p+"/board", wrap(http.HandlerFunc(s.handlePutBoard)))
	mux.Handle("POST "+p+"/board/states", wrap(http.HandlerFunc(s.handleAddState)))
	mux.Handle("POST "+p+"/board/states/reorder", wrap(http.HandlerFunc(s.handleReorderStates)))
	mux.Handle("PATCH "+p+"/board/states/{name}", wrap(http.HandlerFunc(s.handleUpdateState)))
	mux.Handle("DELETE "+p+"/board/states/{name}", wrap(http.HandlerFunc(s.handleDeleteState)))
	mux.Handle("POST "+p+"/board/fields", wrap(http.HandlerFunc(s.handleAddField)))
	mux.Handle("POST "+p+"/board/fields/reorder", wrap(http.HandlerFunc(s.handleReorderFields)))
	mux.Handle("PATCH "+p+"/board/fields/{name}", wrap(http.HandlerFunc(s.handleUpdateField)))
	mux.Handle("DELETE "+p+"/board/fields/{name}", wrap(http.HandlerFunc(s.handleDeleteField)))
	mux.Handle("POST "+p+"/board/views", wrap(http.HandlerFunc(s.handleSaveView)))
	mux.Handle("DELETE "+p+"/board/views/{name}", wrap(http.HandlerFunc(s.handleDeleteView)))
}

// ---------------------------------------------------------------------------
// /issues
// ---------------------------------------------------------------------------

type issueCreateReq struct {
	Title    string            `json:"title"`
	Body     string            `json:"body,omitempty"`
	State    string            `json:"state,omitempty"`
	Labels   []string          `json:"labels,omitempty"`
	Priority int               `json:"priority,omitempty"`
	Assignee string            `json:"assignee,omitempty"`
	Blockers []string          `json:"blockers,omitempty"`
	Fields   map[string]any    `json:"fields,omitempty"`
	Bot      string            `json:"bot,omitempty"`
	BotArgs  map[string]string `json:"bot_args,omitempty"`
}

// handleListLabels returns every label currently in use on the board
// with usage count + last-used timestamp. Sorted by count desc.
// Read-only; no auth check beyond the wrap-middleware applied at mount.
func (s *Store) handleListLabels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.AggregateLabels())
}

type labelRenameReq struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type labelOpResp struct {
	Touched int `json:"touched"`
}

// handleRenameLabel POST /labels/rename {from, to}: rewrites every
// occurrence of `from` to `to` across the board. Returns the number
// of issues whose label set actually changed.
func (s *Store) handleRenameLabel(w http.ResponseWriter, r *http.Request) {
	var in labelRenameReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.RenameLabel(in.From, in.To)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, labelOpResp{Touched: n})
}

// handleMergeLabels POST /labels/merge {from, to}: every issue
// carrying `from` ends up carrying `to` (de-duped) and no longer
// `from`. Audit-trail twin of rename.
func (s *Store) handleMergeLabels(w http.ResponseWriter, r *http.Request) {
	var in labelRenameReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.MergeLabels(in.From, in.To)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, labelOpResp{Touched: n})
}

// handleDeleteLabel DELETE /labels/{label}: strips the label from every
// issue. The label name is URL-path-encoded by the client; the
// router unescapes it via PathValue.
func (s *Store) handleDeleteLabel(w http.ResponseWriter, r *http.Request) {
	label := r.PathValue("label")
	n, err := s.DeleteLabel(label)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, labelOpResp{Touched: n})
}

func (s *Store) handleListIssues(w http.ResponseWriter, r *http.Request) {
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

func (s *Store) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	var in issueCreateReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	iss := Issue{
		Title:    in.Title,
		Body:     in.Body,
		State:    in.State,
		Labels:   in.Labels,
		Priority: in.Priority,
		Assignee: in.Assignee,
		Blockers: in.Blockers,
		Fields:   in.Fields,
		Bot:      in.Bot,
		BotArgs:  in.BotArgs,
	}
	out, err := s.Create(iss)
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
	Title    *string            `json:"title,omitempty"`
	Body     *string            `json:"body,omitempty"`
	Labels   *[]string          `json:"labels,omitempty"`
	Priority *int               `json:"priority,omitempty"`
	Assignee *string            `json:"assignee,omitempty"`
	Blockers *[]string          `json:"blockers,omitempty"`
	Fields   map[string]any     `json:"fields,omitempty"`
	Bot      *string            `json:"bot,omitempty"`
	BotArgs  *map[string]string `json:"bot_args,omitempty"`
}

type transitionReq struct {
	To string `json:"to"`
}

// resolvePathID extracts the {id} segment, runs prefix resolution, and
// returns the full ID. On miss/ambiguity writes the appropriate HTTP
// error and returns "" + false.
func (s *Store) resolvePathID(w http.ResponseWriter, r *http.Request) (string, bool) {
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

func (s *Store) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolvePathID(w, r)
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

func (s *Store) handlePatchIssue(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolvePathID(w, r)
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
		Bot:      in.Bot,
		BotArgs:  in.BotArgs,
	})
	if err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Store) handleDeleteIssue(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolvePathID(w, r)
	if !ok {
		return
	}
	if err := s.Delete(id); err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleTransitionIssue(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolvePathID(w, r)
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
	iss, err := s.SetState(id, in.To)
	if err != nil {
		writeErr(w, statusForErr(err), err)
		return
	}
	writeJSON(w, http.StatusOK, iss)
}

// commentReq is the body of POST /issues/{id}/comments. Body is the
// comment text. The optional Bot / BotArgs / TransitionTo fields let a
// caller (the studio comment box, which knows the bot catalogue and has
// already parsed a `/command`) both record the comment AND dispatch a
// run in one request: stamp the bot + per-run args and move the issue to
// a dispatch-eligible state, which the polling dispatcher then runs. The
// native store stays decoupled from the bot registry — command→bot
// resolution happens in the caller, not here.
type commentReq struct {
	Author       string            `json:"author,omitempty"`
	Body         string            `json:"body"`
	Bot          *string           `json:"bot,omitempty"`
	BotArgs      map[string]string `json:"bot_args,omitempty"`
	TransitionTo string            `json:"transition_to,omitempty"`
}

func (s *Store) handleAddComment(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolvePathID(w, r)
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
	// Auto-resolve a leading "/command" when the caller didn't pre-resolve a bot
	// (the API/curl twin of the studio comment box, which resolves it client-side
	// and posts explicit bot/bot_args). The server installs the resolver; a bare
	// store leaves it nil and just records the comment, as before.
	if bot == nil && botArgs == nil && updated != nil {
		if d := s.getCommentDispatcher(); d != nil {
			if rbot, rargs, rto, rok := d(*updated, in.Body); rok {
				bot = &rbot
				botArgs = rargs
				if transitionTo == "" {
					transitionTo = rto
				}
			}
		}
	}
	// Optional one-shot dispatch: stamp bot + args, then move to the
	// requested state so the dispatcher picks the issue up.
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
	if transitionTo != "" {
		if _, err := s.SetState(id, transitionTo); err != nil {
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
// /board
// ---------------------------------------------------------------------------

func (s *Store) handleGetBoard(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Board())
}

func (s *Store) handlePutBoard(w http.ResponseWriter, r *http.Request) {
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

// stateUpdateReq is the PATCH /board/states/{name} body. A non-nil Name
// that differs from the path segment triggers a cascading rename (issues
// in the column follow); the remaining fields are applied afterward.
type stateUpdateReq struct {
	Name     *string `json:"name,omitempty"`
	Display  *string `json:"display,omitempty"`
	Color    *string `json:"color,omitempty"`
	Eligible *bool   `json:"eligible,omitempty"`
	Terminal *bool   `json:"terminal,omitempty"`
}

type reorderStatesReq struct {
	Order []string `json:"order"`
}

// stateDeleteConflictResp is the 409 body returned when a non-empty
// column is deleted without a migration target. `count` lets the UI
// prompt "move N issues to…".
type stateDeleteConflictResp struct {
	Error string `json:"error"`
	Count int    `json:"count"`
}

// handleAddState POST /board/states: appends a new column. Body is a
// State. Returns the refreshed board.
func (s *Store) handleAddState(w http.ResponseWriter, r *http.Request) {
	var st State
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.AddState(st); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

// handleUpdateState PATCH /board/states/{name}: edits a column's
// display/color/flags and, when the body carries a different `name`,
// renames it first (cascading to issues). Returns the refreshed board.
func (s *Store) handleUpdateState(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in stateUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	target := name
	if in.Name != nil && *in.Name != name {
		if _, err := s.RenameState(name, *in.Name); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		target = *in.Name
	}
	if in.Display != nil || in.Color != nil || in.Eligible != nil || in.Terminal != nil {
		if err := s.UpdateState(target, StatePatch{
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

// handleDeleteState DELETE /board/states/{name}?migrate_to=X: removes a
// column. A non-empty column without a migration target returns 409 with
// the issue count so the UI can prompt for a destination.
func (s *Store) handleDeleteState(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	migrateTo := r.URL.Query().Get("migrate_to")
	_, err := s.DeleteState(name, migrateTo)
	if err != nil {
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

// handleReorderStates POST /board/states/reorder {order:[...]}: rewrites
// the column order. Returns the refreshed board.
func (s *Store) handleReorderStates(w http.ResponseWriter, r *http.Request) {
	var in reorderStatesReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ReorderStates(in.Order); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

// mustList returns the current issues for the 409 count; on error it
// returns nil (count falls back to 0, still a valid conflict response).
func mustList(s *Store) []*Issue {
	issues, err := s.List(ListFilter{})
	if err != nil {
		return nil
	}
	return issues
}

// fieldUpdateReq is the PATCH /board/fields/{name} body. A non-nil Name
// that differs from the path triggers a cascading rename across issues;
// remaining attributes are applied afterward.
type fieldUpdateReq struct {
	Name       *string    `json:"name,omitempty"`
	Display    *string    `json:"display,omitempty"`
	Type       *FieldType `json:"type,omitempty"`
	Required   *bool      `json:"required,omitempty"`
	EnumValues *[]string  `json:"enum_values,omitempty"`
}

// handleAddField POST /board/fields: appends a custom-field definition.
func (s *Store) handleAddField(w http.ResponseWriter, r *http.Request) {
	var f Field
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.AddField(f); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

// handleUpdateField PATCH /board/fields/{name}: edits a field definition
// and, when the body carries a different `name`, renames it first
// (cascading across issue.Fields maps).
func (s *Store) handleUpdateField(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in fieldUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	target := name
	if in.Name != nil && *in.Name != name {
		if _, err := s.RenameField(name, *in.Name); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		target = *in.Name
	}
	if in.Display != nil || in.Type != nil || in.Required != nil || in.EnumValues != nil {
		if err := s.UpdateField(target, FieldPatch{
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

// handleDeleteField DELETE /board/fields/{name}: removes a field
// definition and strips its key from every issue.
func (s *Store) handleDeleteField(w http.ResponseWriter, r *http.Request) {
	if _, err := s.DeleteField(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

// handleReorderFields POST /board/fields/reorder {order:[...]}.
func (s *Store) handleReorderFields(w http.ResponseWriter, r *http.Request) {
	var in reorderStatesReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ReorderFields(in.Order); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

// handleSaveView POST /board/views: upserts a named filter/sort/group
// preset (body = View). Returns the refreshed board.
func (s *Store) handleSaveView(w http.ResponseWriter, r *http.Request) {
	var v View
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.SaveView(v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Board())
}

// handleDeleteView DELETE /board/views/{name}: removes a saved view.
func (s *Store) handleDeleteView(w http.ResponseWriter, r *http.Request) {
	if err := s.DeleteView(r.PathValue("name")); err != nil {
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
