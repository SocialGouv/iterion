package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// queueGateClient answers PER SHA, which the shared listingGateClient cannot:
// this lane reads the pull request head and the queue revision in the same
// call, and a stub that returns one list for both cannot tell "already
// mirrored" from "the head's own verdict". Locked because a test observer fed
// by the code under test takes its mutex whatever today's call graph is.
type queueGateClient struct {
	mu        sync.Mutex
	headSHA   string
	getErr    error
	setErr    error
	listErr   error
	byRef     map[string][]forge.CommitStatus
	posted    map[string][]forge.CommitStatus
	setCalls  int
	listCalls int
}

func (q *queueGateClient) GetPullRequest(_ context.Context, _ string, _ int) (forge.PullRef, error) {
	return forge.PullRef{HeadSHA: q.headSHA, State: "open"}, q.getErr
}

func (q *queueGateClient) ListCommitStatuses(_ context.Context, _, sha string) ([]forge.CommitStatus, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.listCalls++
	if q.listErr != nil {
		return nil, q.listErr
	}
	return q.byRef[sha], nil
}

func (q *queueGateClient) SetCommitStatus(_ context.Context, _, sha string, st forge.CommitStatus) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.setErr != nil {
		return q.setErr
	}
	q.setCalls++
	if q.posted == nil {
		q.posted = map[string][]forge.CommitStatus{}
	}
	q.posted[sha] = append(q.posted[sha], st)
	return nil
}

func (q *queueGateClient) onto(sha string) []forge.CommitStatus {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.posted[sha]
}

func (q *queueGateClient) sets() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.setCalls
}

const (
	queuePRHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	queueSHA    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	queueGate   = "revi/review"
)

// mergeGroupBody is a merge_group delivery in GitHub's own wire shape.
func mergeGroupBody(t *testing.T, action, headRef, headSHA string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"action": action,
		"repository": map[string]any{
			"id": 1, "full_name": "o/r", "html_url": "https://github.com/o/r",
		},
		"merge_group": map[string]any{
			"head_sha": headSHA, "head_ref": headRef,
			"base_sha": "cccccccccccccccccccccccccccccccccccccccc", "base_ref": "refs/heads/main",
		},
		"sender": map[string]any{"login": "github-merge-queue[bot]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func queueRef(number int) string {
	return fmt.Sprintf("refs/heads/gh-readonly-queue/main/pr-%d-cccccccc", number)
}

// mergeGroupFixture wires the lane: a team connection covering github.com/o/r,
// a stub gate client, and an integration pinning the repo's gate context.
func mergeGroupFixture(t *testing.T, gc forgeGateClient, pinned map[string]string) (*Server, webhooks.Config) {
	t.Helper()
	s, _ := newForgePublishTestServer(t)
	// The audit row is where this lane says WHY it stopped, and several of its
	// refusals differ in nothing else: "the verdict could not be read" and
	// "there is no verdict" both post nothing, and collapsing them is the
	// defect. So the store is wired, and the reason is asserted.
	s.webhookDeliveries = webhooks.NewMemoryDeliveryStore()
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return gc, nil
	}
	cfg := webhooks.Config{
		ID: "wh1", TenantID: "team1", Provider: webhooks.ProviderGitHub,
		OperatorLaunchVars: pinned,
	}
	return s, cfg
}

func postMergeGroup(t *testing.T, s *Server, cfg webhooks.Config, body []byte) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleGitHubMergeGroup(context.Background(), rec, cfg, body, "hash", "203.0.113.7")
	var doc struct{ Status string }
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec.Code, doc.Status
}

// lastReason is the reason the lane recorded on its audit row.
func lastReason(t *testing.T, s *Server, cfg webhooks.Config) string {
	t.Helper()
	rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("recorded %d deliveries, want exactly 1", len(rows))
	}
	return rows[0].Error
}

// The defect: a merge-queue revision is not a pull request, so the review
// never runs on it and the required context is absent exactly where the merge
// decision is made. Measured 2026-07-28 on PR #307 — 62 minutes in
// AWAITING_CHECKS, every entry behind it stuck.
func TestMergeGroup_MirrorsTheEntryVerdictOntoTheQueueRevision(t *testing.T) {
	gc := &queueGateClient{
		headSHA: queuePRHead,
		byRef: map[string][]forge.CommitStatus{
			queuePRHead: {{Context: queueGate, State: forge.CommitStateSuccess, TargetURL: "https://iterion.test/runs/r1"}},
		},
	}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

	code, status := postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))

	if code != http.StatusOK || status != webhooks.StatusAccepted {
		t.Fatalf("code=%d status=%q, want 200/%s", code, status, webhooks.StatusAccepted)
	}
	got := gc.onto(queueSHA)
	if len(got) != 1 {
		t.Fatalf("posted %d statuses on the queue revision, want 1 — the queue waits for a context that cannot arrive", len(got))
	}
	if got[0].State != forge.CommitStateSuccess {
		t.Errorf("state = %q, want the head's own verdict", got[0].State)
	}
	if got[0].Context != queueGate {
		t.Errorf("context = %q, want the context the ruleset requires", got[0].Context)
	}
	if got[0].TargetURL != "https://iterion.test/runs/r1" {
		t.Errorf("target url = %q, want the review the verdict came from", got[0].TargetURL)
	}
	// Nothing is posted on the pull request head: its verdict is already there,
	// and re-posting would make this lane a second producer of the same fact.
	if len(gc.onto(queuePRHead)) != 0 {
		t.Errorf("the lane wrote on the pull request head too: %+v", gc.onto(queuePRHead))
	}
}

// It MIRRORS rather than asserting success. A head that is red must stay red
// on the queue revision, or the queue merges what the gate refused.
func TestMergeGroup_MirrorsAFailureRatherThanAssertingSuccess(t *testing.T) {
	gc := &queueGateClient{
		headSHA: queuePRHead,
		byRef: map[string][]forge.CommitStatus{
			queuePRHead: {{Context: queueGate, State: forge.CommitStateFailure}},
		},
	}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

	if _, status := postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA)); status != webhooks.StatusAccepted {
		t.Fatalf("status = %q, want %s", status, webhooks.StatusAccepted)
	}
	got := gc.onto(queueSHA)
	if len(got) != 1 || got[0].State != forge.CommitStateFailure {
		t.Fatalf("posted %+v, want one failure mirrored onto the queue revision", got)
	}
}

// A mirrored `pending` must NOT be readable as the server's in-flight claim.
// That marker means "a run owns this check and will replace it"; no run owns a
// queue revision, so the reconciler would be asked to repair a pending nothing
// can resolve.
func TestMergeGroup_TheMirrorIsNeverReadableAsAnInFlightClaim(t *testing.T) {
	gc := &queueGateClient{
		headSHA: queuePRHead,
		byRef: map[string][]forge.CommitStatus{
			queuePRHead: {{Context: queueGate, State: forge.CommitStatePending, Description: gateInFlightDescription}},
		},
	}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

	postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))

	got := gc.onto(queueSHA)
	if len(got) != 1 {
		t.Fatalf("posted %d statuses, want 1 — the queue must learn the entry is still being judged", len(got))
	}
	if isGateInFlight(got[0]) {
		t.Errorf("the mirrored status reads as an in-flight CLAIM (%q) — the reconciler would try to repair a revision no run owns", got[0].Description)
	}
}

// A head carrying no verdict for this repo's gate context gets NOTHING: a
// fabricated failure blocks what was never gated, and a fabricated success
// certifies a revision nobody audited.
func TestMergeGroup_AHeadWithNoVerdictPostsNothing(t *testing.T) {
	gc := &queueGateClient{
		headSHA: queuePRHead,
		byRef:   map[string][]forge.CommitStatus{queuePRHead: {{Context: "some/other", State: forge.CommitStateSuccess}}},
	}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

	_, status := postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))

	if status != webhooks.StatusFiltered {
		t.Fatalf("status = %q, want %s", status, webhooks.StatusFiltered)
	}
	if gc.sets() != 0 {
		t.Errorf("posted %d statuses on a head nothing gated", gc.sets())
	}
	if reason := lastReason(t, s, cfg); !strings.Contains(reason, "the gate is not in use for it") {
		t.Errorf("the audit row does not say the gate was never in use: %q", reason)
	}
}

// Unreadable is not absent. Collapsing the two turns an API hiccup into "this
// pull request was never gated" — and the queue is told nothing about a
// revision it is holding.
func TestMergeGroup_AnUnreadableHeadStatusPostsNothing(t *testing.T) {
	gc := &queueGateClient{headSHA: queuePRHead, listErr: fmt.Errorf("502 bad gateway")}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

	_, status := postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))

	if status != webhooks.StatusFiltered {
		t.Fatalf("status = %q, want %s", status, webhooks.StatusFiltered)
	}
	if gc.sets() != 0 {
		t.Errorf("posted %d statuses on an unreadable verdict", gc.sets())
	}
	// Posting nothing is right; saying "there is no verdict" about it is not.
	// An API hiccup must not be recorded as "this pull request was never
	// gated" — that is the sentence an operator acts on.
	if reason := lastReason(t, s, cfg); !strings.Contains(reason, "could not be read") || !strings.Contains(reason, "502 bad gateway") {
		t.Errorf("the audit row does not name the failed READ: %q", reason)
	}
}

// `destroyed` means the group is gone — merged, or the entry was removed. A
// status posted then decides about a revision nobody is deciding on.
func TestMergeGroup_OnlyChecksRequestedAsksForAVerdict(t *testing.T) {
	for _, action := range []string{"destroyed", ""} {
		t.Run("action="+action, func(t *testing.T) {
			gc := &queueGateClient{
				headSHA: queuePRHead,
				byRef:   map[string][]forge.CommitStatus{queuePRHead: {{Context: queueGate, State: forge.CommitStateSuccess}}},
			}
			s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

			_, status := postMergeGroup(t, s, cfg, mergeGroupBody(t, action, queueRef(42), queueSHA))

			if status != webhooks.StatusFiltered {
				t.Fatalf("status = %q, want %s", status, webhooks.StatusFiltered)
			}
			if gc.sets() != 0 {
				t.Errorf("action %q posted %d statuses", action, gc.sets())
			}
		})
	}
}

// A repository that pins no gate context has no repo-wide required check to
// satisfy, and the bot-default tier is deliberately out of reach here: a bot's
// own default would mirror onto a context the ruleset never required.
func TestMergeGroup_ARepositoryThatPinsNoGateContextIsLeftAlone(t *testing.T) {
	gc := &queueGateClient{
		headSHA: queuePRHead,
		byRef:   map[string][]forge.CommitStatus{queuePRHead: {{Context: queueGate, State: forge.CommitStateSuccess}}},
	}
	s, cfg := mergeGroupFixture(t, gc, nil)

	_, status := postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))

	if status != webhooks.StatusFiltered {
		t.Fatalf("status = %q, want %s", status, webhooks.StatusFiltered)
	}
	if gc.sets() != 0 {
		t.Errorf("posted %d statuses on a repository that pins nothing", gc.sets())
	}
	// The reason is where a default-context fallback would show: with one, the
	// lane reads a context the repo never pinned, finds nothing, and reports
	// "the gate is not in use" — the same silence for a different reason, and
	// the operator is told the wrong thing.
	if reason := lastReason(t, s, cfg); !strings.Contains(reason, "no gate_context is pinned") {
		t.Errorf("the audit row does not name the missing pin: %q", reason)
	}
}

// gate_enabled pinned off leaves the bots advisory-only: no verdict is owed
// anywhere, so mirroring one would manufacture the very required check the pin
// exists to avoid.
func TestMergeGroup_TheGateDisabledPinMirrorsNothing(t *testing.T) {
	for _, v := range []string{"false", "0", "off", ""} {
		gc := &queueGateClient{
			headSHA: queuePRHead,
			byRef:   map[string][]forge.CommitStatus{queuePRHead: {{Context: queueGate, State: forge.CommitStateSuccess}}},
		}
		s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate, "gate_enabled": v})

		postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))

		if gc.sets() != 0 {
			t.Errorf("gate_enabled=%q posted %d statuses", v, gc.sets())
		}
	}
}

// GitHub redelivers. Statuses APPEND on a revision, so a lane that re-posts an
// identical verdict stacks rows that carry no new fact.
func TestMergeGroup_AnAlreadyMirroredVerdictIsNotRePosted(t *testing.T) {
	gc := &queueGateClient{
		headSHA: queuePRHead,
		byRef: map[string][]forge.CommitStatus{
			queuePRHead: {{Context: queueGate, State: forge.CommitStateSuccess}},
			queueSHA: {{
				Context: queueGate, State: forge.CommitStateSuccess,
				Description: mirroredQueueVerdict(42, queuePRHead),
			}},
		},
	}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

	_, status := postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))

	if status != webhooks.StatusFiltered {
		t.Fatalf("status = %q, want %s on a redelivery", status, webhooks.StatusFiltered)
	}
	if gc.sets() != 0 {
		t.Errorf("a redelivery posted %d statuses", gc.sets())
	}
}

// A STALE mirror is the other half: the head moved and was re-reviewed, so the
// queue revision's status is about an older verdict and must be replaced.
func TestMergeGroup_AStaleMirrorIsReplaced(t *testing.T) {
	gc := &queueGateClient{
		headSHA: queuePRHead,
		byRef: map[string][]forge.CommitStatus{
			queuePRHead: {{Context: queueGate, State: forge.CommitStateFailure}},
			queueSHA: {{
				Context: queueGate, State: forge.CommitStateSuccess,
				Description: mirroredQueueVerdict(42, queuePRHead),
			}},
		},
	}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

	postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))

	got := gc.onto(queueSHA)
	if len(got) != 1 || got[0].State != forge.CommitStateFailure {
		t.Fatalf("posted %+v, want the newer verdict mirrored over the stale one", got)
	}
}

// Which pull request the revision carries decides WHOSE verdict certifies it.
// A ref that names none is refused, never guessed: mirroring the wrong entry
// would certify a commit nobody audited.
func TestMergeGroup_AQueueRefThatNamesNoPullRequestIsRefused(t *testing.T) {
	gc := &queueGateClient{headSHA: queuePRHead}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

	rec := httptest.NewRecorder()
	s.handleGitHubMergeGroup(context.Background(), rec, cfg,
		mergeGroupBody(t, "checks_requested", "refs/heads/gh-readonly-queue/main/no-pr-here", queueSHA), "hash", "203.0.113.7")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 on a ref naming no pull request\n%s", rec.Code, rec.Body.String())
	}
	if gc.sets() != 0 {
		t.Errorf("posted %d statuses from a ref nobody could read", gc.sets())
	}
}

// The BASE branch is part of the queue ref and may itself contain a `pr-<n>-`
// segment: `release/pr-12-abcdef0` is a legal branch name. An unanchored read
// answers with the base's digits and mirrors a stranger's verdict.
func TestMergeGroup_TheBaseBranchDigitsDoNotDecideThePullRequest(t *testing.T) {
	gc := &queueGateClient{
		headSHA: queuePRHead,
		byRef:   map[string][]forge.CommitStatus{queuePRHead: {{Context: queueGate, State: forge.CommitStateSuccess}}},
	}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

	postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested",
		"refs/heads/gh-readonly-queue/release/pr-12-abcdef0/pr-77-cccccccc", queueSHA))

	got := gc.onto(queueSHA)
	if len(got) != 1 {
		t.Fatalf("posted %d statuses, want 1", len(got))
	}
	if want := mirroredQueueVerdict(77, queuePRHead); got[0].Description != want {
		t.Errorf("description = %q, want %q — the base branch's digits decided which entry certifies this revision", got[0].Description, want)
	}
}

// A forge that refuses the write is REPORTED, never swallowed: the queue is now
// waiting on a context this server was supposed to produce, and the operator
// needs to reach for the documented fallback.
func TestMergeGroup_ARefusedWriteIsReportedNotSwallowed(t *testing.T) {
	gc := &queueGateClient{
		headSHA: queuePRHead,
		byRef:   map[string][]forge.CommitStatus{queuePRHead: {{Context: queueGate, State: forge.CommitStateSuccess}}},
		setErr:  fmt.Errorf("422 unprocessable"),
	}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": queueGate})

	code, status := postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))

	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200 — a 4xx makes GitHub disable the webhook", code)
	}
	if status != webhooks.StatusLaunchError {
		t.Errorf("status = %q, want %s so the refusal is auditable", status, webhooks.StatusLaunchError)
	}
}

// The DISPATCH, not the lane: a merge_group delivery used to land in the
// default arm of handleGitHubWebhook and be recorded as "unsupported
// X-GitHub-Event" — the event the merge queue depends on, filtered as noise
// alongside ping and push. The witness reads the audit row's reason, because
// both outcomes answer 200 (a 4xx makes GitHub disable the webhook) and the
// reason is the only place the two differ.
func TestGitHubWebhook_MergeGroupReachesTheGateLane(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)

	body := string(mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderMergeGroup, pt))

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200\n%s", w.Code, w.Body.String())
	}
	rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("recorded %d deliveries, want 1", len(rows))
	}
	if strings.Contains(rows[0].Error, "unsupported X-GitHub-Event") {
		t.Fatalf("merge_group was filtered as an unsupported event — the queue lane is never reached: %q", rows[0].Error)
	}
	// The lane WAS reached, and said why it stopped: this fixture pins no gate
	// context, which is the lane's own first refusal.
	if !strings.Contains(rows[0].Error, "no gate_context is pinned") {
		t.Errorf("the delivery does not carry the gate lane's own reason: %q", rows[0].Error)
	}
	if rows[0].EventKind != prforge.EventHeaderMergeGroup {
		t.Errorf("event kind = %q, want %q", rows[0].EventKind, prforge.EventHeaderMergeGroup)
	}
}

// The operator's pin outranks the manifest union on the queue revision too:
// a repo that renamed its shared context must not have the queue mirror onto
// the old one.
func TestMergeGroup_TheOperatorPinOutranksTheManifestUnion(t *testing.T) {
	gc := &queueGateClient{
		headSHA: queuePRHead,
		byRef:   map[string][]forge.CommitStatus{queuePRHead: {{Context: "operator/pin", State: forge.CommitStateSuccess}}},
	}
	s, cfg := mergeGroupFixture(t, gc, map[string]string{"gate_context": "operator/pin"})
	cfg.LaunchVars = map[string]string{"gate_context": queueGate}

	postMergeGroup(t, s, cfg, mergeGroupBody(t, "checks_requested", queueRef(42), queueSHA))

	got := gc.onto(queueSHA)
	if len(got) != 1 || got[0].Context != "operator/pin" {
		t.Fatalf("posted %+v, want the operator's pinned context", got)
	}
}
