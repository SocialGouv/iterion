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
	mux.Handle("GET "+p+"/board", wrap(http.HandlerFunc(s.handleGetBoard)))
	mux.Handle("PUT "+p+"/board", wrap(http.HandlerFunc(s.handlePutBoard)))
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
