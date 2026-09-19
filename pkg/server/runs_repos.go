// Package server — distinct repositories surface.
//
// The run-list "by repo" filter (studio) needs the set of repositories
// that have runs, with a count per repo, independent of whatever filter
// the list view currently applies (so selecting one repo doesn't make
// the other chips vanish). This file exposes that as
// GET /api/v1/runs/repos.
//
// "Repository" here is store.Run.ProjectPath — the stable forge slug
// ("group/project") stamped on inbound-webhook launches in cloud mode.
// Local/manual runs leave it empty and are skipped (the studio derives
// folder chips client-side from work_dir/repo_root in that mode).
//
// Like the stats surface, this aggregates over runsSvc.ListCtx (which
// the mongo store scopes to the caller's tenant); the
// (tenant_id, project_path, created_at) index backs that scoping. Cheap
// for hundreds of runs; if a tenant grows past low thousands we'll push
// the distinct down to a native mongo aggregation.

package server

import (
	"net/http"
	"sort"

	"github.com/SocialGouv/iterion/pkg/runview"
)

// RepoBucket is one row of the distinct-repos response: a repository
// slug and how many of the caller's runs target it.
type RepoBucket struct {
	ProjectPath string `json:"project_path"`
	Count       int    `json:"count"`
}

// ReposResponse is the JSON shape returned by GET /api/v1/runs/repos.
type ReposResponse struct {
	Repos []RepoBucket `json:"repos"`
}

func (s *Server) registerRunsReposRoutes() {
	if s.runs == nil {
		return
	}
	s.mux.HandleFunc("GET /api/v1/runs/repos", s.handleRunsRepos)
}

func (s *Server) handleRunsRepos(w http.ResponseWriter, r *http.Request) {
	// Snapshot the hot-swappable run service so a concurrent project
	// switch can't pair this request with the other project's store.
	s.stateMu.RLock()
	runsSvc := s.runs
	s.stateMu.RUnlock()

	if runsSvc == nil {
		httpError(w, http.StatusServiceUnavailable, "no run store configured on this server")
		return
	}
	// Same scoping contract as /api/v1/runs/stats (#1419): honour
	// ?team_id= (or X-Iterion-Team) when authorised, active team otherwise.
	// A repo chip strip is tenant-scoped like the aggregation.
	_, ctx, ok := s.resolveTenantScope(w, r)
	if !ok {
		return
	}

	// No filter: we want every repo that has ever had a run, not just a
	// recent window — the chip strip should expose the full set.
	runs, err := runsSvc.ListCtx(ctx, runview.ListFilter{})
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list runs: %v", err)
		return
	}

	s.writeJSONFor(w, r, aggregateRepos(runs))
}

// aggregateRepos collapses run summaries into per-ProjectPath counts.
// Empty ProjectPath (local/manual runs) is skipped. Sorted by count
// desc then slug asc so the studio renders the busiest repos first.
// Pulled out of the handler so it can be unit-tested without a server.
func aggregateRepos(runs []runview.RunSummary) ReposResponse {
	counts := map[string]int{}
	for i := range runs {
		p := runs[i].ProjectPath
		if p == "" {
			continue
		}
		counts[p]++
	}
	out := make([]RepoBucket, 0, len(counts))
	for path, n := range counts {
		out = append(out, RepoBucket{ProjectPath: path, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].ProjectPath < out[j].ProjectPath
	})
	return ReposResponse{Repos: out}
}
