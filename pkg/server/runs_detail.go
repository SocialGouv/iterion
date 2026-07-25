package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// --- Handlers ---

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := runview.ListFilter{
		Workflow: q.Get("workflow"),
		Repo:     q.Get("repo"),
		Bundle:   q.Get("bot"),
		Node:     q.Get("node"),
	}
	if status := q.Get("status"); status != "" {
		filter.Status = store.RunStatus(status)
	}
	if since := q.Get("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid since (want RFC3339): %v", err)
			return
		}
		filter.Since = t
	}
	if limit := q.Get("limit"); limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid limit")
			return
		}
		filter.Limit = n
	}
	out, err := s.runs.ListCtx(r.Context(), filter)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list runs: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"runs": out})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if xs, _, err := s.resolveCrossStore(r); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	} else if xs != nil {
		snap, err := runview.BuildSnapshot(r.Context(), xs, id)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusNotFound, "run not found in cross-store: %v", err)
			return
		}
		s.writeJSONFor(w, r, snap)
		return
	}
	snap, err := s.runs.SnapshotCtx(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrRunDeleted) {
			// 410, not 404: the run existed and was deliberately
			// deleted — the studio shows "run deleted" instead of a
			// stale-run banner and stops retrying.
			s.httpErrorFor(w, r, http.StatusGone, "run was deleted")
			return
		}
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	s.enrichRunLOC(r.Context(), &snap.Run)
	s.writeJSONFor(w, r, snap)
}

// handleListRunChildren returns the shard/child subtree of a run — every
// run whose ParentRunID equals {id}, ordered by created_at ascending
// (T4b, refs #125). The tree UI that renders these under a run is a
// separate follow-up; this endpoint is the data source.
func (s *Server) handleListRunChildren(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if xs, _, err := s.resolveCrossStore(r); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	} else if xs != nil {
		children, err := runview.BuildChildrenFromStore(r.Context(), xs, id)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "list run children from cross-store: %v", err)
			return
		}
		s.writeJSONFor(w, r, map[string]any{"runs": children})
		return
	}
	children, err := s.runs.ListChildren(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list run children: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"runs": children})
}

func (s *Server) handleGetRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	q := r.URL.Query()
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	if xs, _, err := s.resolveCrossStore(r); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	} else if xs != nil {
		events, err := xs.LoadEventsRange(r.Context(), id, from, to, runview.MaxEventsPerPage)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "load events from cross-store: %v", err)
			return
		}
		if events == nil {
			events = []*store.Event{}
		}
		s.writeJSONFor(w, r, map[string]any{"events": events})
		return
	}
	events, err := s.runs.LoadEventsCtx(r.Context(), id, from, to)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load events: %v", err)
		return
	}
	if events == nil {
		events = []*store.Event{}
	}
	s.writeJSONFor(w, r, map[string]any{"events": events})
}

func (s *Server) handleGetRunWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if xs, _, err := s.resolveCrossStore(r); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	} else if xs != nil {
		// Cross-store: re-use the IR-→-wire projection so the studio
		// receives the same shape it expects from the same-store path.
		// One-shot — no cache (the daemon serves cross-store reads
		// rarely; cache-hit ratio wouldn't justify the lock).
		wf, err := runview.BuildWireWorkflowFromStore(r.Context(), xs, id)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusNotFound, "load workflow from cross-store: %v", err)
			return
		}
		s.writeJSONFor(w, r, wf)
		return
	}
	wf, err := s.runs.LoadWireWorkflowCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "load workflow: %v", err)
		return
	}
	s.writeJSONFor(w, r, wf)
}

// handleListPlans returns the chronological plan snapshots captured for a
// run — the agents' TodoWrite/todo_write living TODO lists (filesystem
// runs/<id>/plans/ or the Mongo run_plans collection in cloud mode).
// Ascending seq order (chronological). Returns an empty array (not 404)
// for a valid run that captured no plans — e.g. an agent that never
// called TodoWrite, or a run predating the feature — so the studio's
// Plans panel renders a clean empty state. Tenant-scoped like the other
// run sub-resource handlers (load the run under the caller's context
// first so the mongo tenant filter rejects cross-tenant requests).
func (s *Server) handleListPlans(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	plans, err := s.runs.ListPlanSnapshotsCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list plans: %v", err)
		return
	}
	if plans == nil {
		plans = []store.PlanSnapshot{}
	}
	s.writeJSONFor(w, r, map[string]any{"plans": plans})
}

func (s *Server) handleListRunSkills(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	skills, err := s.runs.ListRunBundleSkills(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list skills: %v", err)
		return
	}
	s.writeJSONFor(w, r, skills)
}

// handleGetSessionBoard returns the LLM-curated Session-board spec for a
// run (the widgets shown beneath the task list on the Tasks tab). Returns
// a zero-value spec (version 0, no widgets) when curation never ran for
// this run — never a 404 — so the studio can render the deterministic
// task board alone.
func (s *Server) handleGetSessionBoard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	spec, err := s.runs.SessionBoard(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load session board: %v", err)
		return
	}
	s.writeJSONFor(w, r, spec)
}
