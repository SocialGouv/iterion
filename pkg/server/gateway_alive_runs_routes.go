package server

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
)

// registerGatewayAliveRunsRoutes wires the shared-gateway status route
// (ADR-167 T2): per gateway host, the alive runs and subbot trees holding its
// capacity subject, and the trees alive on it whose subjects do not name it.
// Super-admin only: the reading spans every team. Registered with the key
// store, whose rows say which gateway a stamped fingerprint dials.
func (s *Server) registerGatewayAliveRunsRoutes() {
	if s.authSvc == nil || s.apiKeys == nil {
		return
	}
	s.mux.Handle("GET /api/admin/llm/gateway-alive-runs", s.requireSuperAdmin(http.HandlerFunc(s.handleGatewayAliveRuns)))
}

// gatewayAliveRunsView is the status route's answer. It carries counts and
// bot ids only — no run id, no team id: an aggregate over every team.
type gatewayAliveRunsView struct {
	ReadAt time.Time `json:"read_at"`
	// Hosts lists every gateway with an alive tree holding its subject or
	// alive on it unnamed, sorted by host.
	Hosts []gatewayHostAliveRuns `json:"hosts"`
	// UnresolvedTrees counts the alive trees that sealed a gateway key whose
	// host the reading cannot know: the key's row names no host, or the
	// tree's stamps were all written without the subjects and no row answers
	// for its key any more (rotated, deleted) while its content boundary
	// records one sealed. A tree whose latest stamp read the subjects
	// (store.Run.CapacitySubjectsRead) is never unresolved for want of a row:
	// its subjects are the gateways its latest resolution sealed a key on,
	// none included. A stamp an older replica rewrote no longer reads so.
	UnresolvedTrees int `json:"unresolved_trees"`
}

// gatewayHostAliveRuns is one gateway host's reading.
type gatewayHostAliveRuns struct {
	Host    string `json:"host"`
	Subject string `json:"subject"`
	// AliveTrees counts the alive subbot trees holding the subject — the
	// value of iterion_gateway_alive_runs{host}, a tree being one place
	// (ADR-167 §5).
	AliveTrees int `json:"alive_trees"`
	// AliveRuns counts the alive runs holding the subject — what the
	// run-admission ledger counts today. AliveRuns − AliveTrees is the subbot
	// children holding the subject on their own stamp (resumed after their
	// human gate).
	AliveRuns int `json:"alive_runs"`
	// UnnamedTrees counts the alive trees whose runs hold a key on this
	// gateway while no run of the tree names its subject: a run stamped
	// without the subjects — before they existed, or by an older replica
	// during a rolling deploy — or one whose stamp a write lost. An
	// activation of a bound must see it at zero.
	UnnamedTrees int `json:"unnamed_trees"`
	// Bots counts the trees holding the subject per bot of the tree's root.
	Bots []gatewayBotAliveTrees `json:"bots"`
}

// gatewayBotAliveTrees is one bot's share of a host's alive trees.
type gatewayBotAliveTrees struct {
	BotID      string `json:"bot_id"`
	AliveTrees int    `json:"alive_trees"`
}

// The 501 codes of the status route name the capability the instance lacks:
// a 5xx body carries its code, never its message.
const (
	gatewayAliveRunsNoListingCode   = "run_store_without_alive_capacity_listing"
	gatewayAliveRunsNoKeyLookupCode = "api_key_store_without_gateway_key_lookup"
)

func (s *Server) handleGatewayAliveRuns(w http.ResponseWriter, r *http.Request) {
	lister := store.AsAliveCapacityLister(s.cfg.Store)
	if lister == nil {
		s.httpErrorCode(w, r, http.StatusNotImplemented, gatewayAliveRunsNoListingCode, "the run store does not implement store.AliveCapacityLister (ListAliveRunsForCapacity): the gateway alive-run reading is unavailable on this instance")
		return
	}
	lookup, ok := s.apiKeys.(secrets.GatewayKeyLookup)
	if !ok {
		s.httpErrorCode(w, r, http.StatusNotImplemented, gatewayAliveRunsNoKeyLookupCode, "the api-key store does not implement secrets.GatewayKeyLookup (GatewayBaseURLsByFingerprint): the gateway a stamped key dials cannot be read on this instance")
		return
	}
	runs, err := lister.ListAliveRunsForCapacity(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "gateway alive runs: list the alive runs: %v", err)
		return
	}
	var fps []string
	for i := range runs {
		for _, fp := range runs[i].CredFingerprints {
			if fp != "" && !slices.Contains(fps, fp) {
				fps = append(fps, fp)
			}
		}
	}
	urls, err := lookup.GatewayBaseURLsByFingerprint(r.Context(), fps)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "gateway alive runs: join the stamped keys to their gateways: %v", err)
		return
	}
	subjectOf := make(map[string]string, len(urls))
	for fp, baseURL := range urls {
		subjectOf[fp] = cloudpublisher.GatewayCapacitySubjectOf(baseURL)
	}
	view := gatewayAliveRunsReport(runs, subjectOf)
	view.ReadAt = time.Now().UTC()
	s.writeJSONFor(w, r, view)
}

// gatewayAliveRunsReport is the reading over one alive listing. subjectOf
// maps every stamped fingerprint whose key is a stored gateway row to the
// subject of the row's URL ("" when the URL names no host); a fingerprint
// absent from it is no gateway key the store knows.
//
// A run is judged with its subbot tree (store.AliveSubbotTreeRoots): an
// in-process child carries its parent's content boundary and no stamp of its
// own, so per run it would read unnamed or unresolved under a parent that
// names its host. Per tree:
//   - the subjects it names are its runs' stamped subjects;
//   - the hosts it is on are those of its runs' gateway keys, read off the
//     key rows — never off content_boundary.gateway_host, frozen at launch;
//   - a host it is on and does not name counts it unnamed there;
//   - it is unresolved when a gateway key of its runs has a row naming no
//     host, or when no run's latest stamp read the subjects
//     (Run.CapacitySubjectsRead), no key resolves, and a run's content
//     boundary records a sealed gateway key (KeyID): the key is gone, the
//     host unknown. The boundary is frozen at launch, so it is read only for
//     a tree whose stamps cannot say otherwise: a stamp that read the
//     subjects and names none says the latest resolution sealed no gateway
//     key — the run left the gateway. A stamp a T1 or pre-T1 replica
//     rewrote over it during a rolling deploy does not read so: its marker
//     no longer matches the lists on the document.
//
// The counts per subject are store.CountCapacityHolders', so the trees here
// are the gauge's and the runs the ledger's. The answer does not depend on
// the order of runs.
func gatewayAliveRunsReport(runs []store.Run, subjectOf map[string]string) gatewayAliveRunsView {
	roots := store.AliveSubbotTreeRoots(runs)
	byID := make(map[string]*store.Run, len(runs))
	members := map[string][]*store.Run{}
	for i := range runs {
		byID[runs[i].ID] = &runs[i]
		members[roots[runs[i].ID]] = append(members[roots[runs[i].ID]], &runs[i])
	}
	view := gatewayAliveRunsView{Hosts: []gatewayHostAliveRuns{}}
	unnamed := map[string]int{}
	for _, tree := range members {
		var named, onHosts []string
		resolved, rowNamesNoHost, sealedKey, subjectsRead := false, false, false, false
		for _, r := range tree {
			named = append(named, r.CapacitySubjects...)
			if r.CapacitySubjectsRead() {
				subjectsRead = true
			}
			if r.ContentBoundary != nil && r.ContentBoundary.KeyID != "" {
				sealedKey = true
			}
			for _, fp := range r.CredFingerprints {
				subject, isGateway := subjectOf[fp]
				switch {
				case !isGateway:
				case subject == "":
					resolved, rowNamesNoHost = true, true
				default:
					resolved = true
					onHosts = append(onHosts, subject)
				}
			}
		}
		for _, subject := range dedupeSorted(onHosts) {
			if !slices.Contains(named, subject) {
				unnamed[subject]++
			}
		}
		if rowNamesNoHost || (len(named) == 0 && !resolved && sealedKey && !subjectsRead) {
			view.UnresolvedTrees++
		}
	}

	counts := store.CountCapacityHolders(runs, store.GatewayCapacitySubjectPrefix)
	subjects := make([]string, 0, len(counts)+len(unnamed))
	for subject := range counts {
		subjects = append(subjects, subject)
	}
	for subject := range unnamed {
		subjects = append(subjects, subject)
	}
	for _, subject := range dedupeSorted(subjects) {
		host, ok := store.GatewayHostOfSubject(subject)
		if !ok {
			continue
		}
		bots := map[string]map[string]bool{}
		for i := range runs {
			if !runs[i].SpendsCredential(subject) {
				continue
			}
			root := roots[runs[i].ID]
			bot := byID[root].BotID
			if bots[bot] == nil {
				bots[bot] = map[string]bool{}
			}
			bots[bot][root] = true
		}
		hostView := gatewayHostAliveRuns{
			Host: host, Subject: subject,
			AliveTrees: counts[subject].Trees, AliveRuns: counts[subject].Runs,
			UnnamedTrees: unnamed[subject],
			Bots:         []gatewayBotAliveTrees{},
		}
		for bot, trees := range bots {
			hostView.Bots = append(hostView.Bots, gatewayBotAliveTrees{BotID: bot, AliveTrees: len(trees)})
		}
		slices.SortFunc(hostView.Bots, func(a, b gatewayBotAliveTrees) int { return strings.Compare(a.BotID, b.BotID) })
		view.Hosts = append(view.Hosts, hostView)
	}
	slices.SortFunc(view.Hosts, func(a, b gatewayHostAliveRuns) int { return strings.Compare(a.Host, b.Host) })
	return view
}

// dedupeSorted returns the distinct values of in, sorted.
func dedupeSorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
