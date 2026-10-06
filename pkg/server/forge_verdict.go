// The publish endpoint's VERDICT block: the merge-gate GESTURES a gating bot
// executes through the deterministic publish surface. Same doctrine as the
// gate status: the LLM produced content; the server executes a declaration.
// Grant capabilities (minted from the bot's manifest scopes, strippable by
// the operator kill switch forge_publish_mutations) bound what a run may do;
// the gate status is the audit and ordering anchor; every mutation is pinned
// to the audited head and refused, never retargeted, when it moved.
package server

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// forgeMergeOps bundles the three merge-gate gesture capabilities one
// connection may carry. A nil capability is a provider that does not
// implement the gesture — a typed non-implementation, not an error: the
// refusal names the provider instead of a 501.
type forgeMergeOps struct {
	Approver forge.MergeApprover
	Armer    forge.AutoMergeArmer
	Setter   forge.PullReviewerSetter
}

// mergeOpsFor resolves a connection's gesture capabilities. The
// forgeMergeOpsFor field is a test seam; nil uses the real admin client,
// capability-asserted like every other publish-side resolver.
func (s *Server) mergeOpsFor(ctx context.Context, conn forge.Connection) (forgeMergeOps, error) {
	if s.forgeMergeOpsFor != nil {
		return s.forgeMergeOpsFor(ctx, conn)
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return forgeMergeOps{}, err
	}
	var ops forgeMergeOps
	if a, ok := admin.(forge.MergeApprover); ok {
		ops.Approver = a
	}
	if a, ok := admin.(forge.AutoMergeArmer); ok {
		ops.Armer = a
	}
	if a, ok := admin.(forge.PullReviewerSetter); ok {
		ops.Setter = a
	}
	return ops, nil
}

// validatePublishVerdict is the payload gate: a verdict carrying verbs must
// be fully well-formed or the whole publish is a 400 — a malformed verdict
// must be learned BEFORE the review and the gate land, not discovered as
// gestures that silently never ran.
func validatePublishVerdict(v *publishReviewVerdict) error {
	if v == nil || !v.Enabled {
		return nil
	}
	if !verdictHasVerbs(v) {
		return nil // mode echo only, nothing to validate
	}
	switch v.MergeMethod {
	case "", "merge", "squash":
	default:
		return fmt.Errorf("verdict.merge_method must be \"merge\" or \"squash\" (got %q)", v.MergeMethod)
	}
	audited := strings.TrimSpace(v.AuditedSHA)
	if audited == "" {
		return fmt.Errorf("verdict verbs require verdict.audited_sha — mutations are never unpinned")
	}
	if !isCommitID(audited) {
		return fmt.Errorf("verdict.audited_sha %q is not a commit id (7-40 hex)", boundedRunes(v.AuditedSHA, 24))
	}
	if len(v.RequestReviewers) > maxVerdictReviewers {
		return fmt.Errorf("verdict.request_reviewers carries %d logins, over the %d cap — an escalation names the reviewers it needs, not a broadcast list", len(v.RequestReviewers), maxVerdictReviewers)
	}
	for i, l := range v.RequestReviewers {
		if strings.TrimSpace(l) == "" {
			return fmt.Errorf("verdict.request_reviewers[%d] is empty", i)
		}
	}
	return nil
}

// maxVerdictReviewers bounds one verdict's reviewer list: each login costs a
// sequential forge read inside the publish request, on the connection's
// shared rate budget.
const maxVerdictReviewers = 50

// verdictHasVerbs reports whether the verdict asks for at least one gesture.
func verdictHasVerbs(v *publishReviewVerdict) bool {
	return v.Approve || v.ArmMergeWhenPipelineSucceeds || len(v.RequestReviewers) > 0
}

// refuseVerdict reports a verdict the endpoint will not execute, in band.
// nil when there was no verdict (or an enabled one with no verbs): the
// response field stays absent, exactly as for bots that never send the
// block.
func refuseVerdict(v *publishReviewVerdict, reason string) *verdictResult {
	if v == nil || !v.Enabled {
		return nil
	}
	// An enabled verb-less verdict is echoed here too, exactly as on the
	// success path: both branches must answer the same payload the same way,
	// or a bot reads "not asked" out of one and "asked and refused" out of
	// the other.
	return &verdictResult{Requested: true, Mode: strings.TrimSpace(v.Mode),
		Refused: reasonOrNoVerb(reason, verdictHasVerbs(v))}
}

// reasonOrNoVerb keeps the branch reason when verbs were requested, and the
// no-verb echo when they were not.
func reasonOrNoVerb(reason string, hasVerbs bool) string {
	if hasVerbs {
		return reason
	}
	return "verdict enabled but no verb recognized — nothing executed"
}

// applyPublishVerdict executes the verdict gestures ON the head the gate
// just posted onto, ordered by the gate decision that claimed the check:
// a gate that was refused (stale pin, closed PR, superseded) or superseded
// carries the gestures down with it, and a review that did not land means
// the caller refuses before reaching here.
func (s *Server) applyPublishVerdict(ctx context.Context, conn forge.Connection, grant ForgePublishGrant, number int, v *publishReviewVerdict, gateReq *publishReviewGate, gate gateOutcome) *verdictResult {
	if v == nil || !v.Enabled {
		return nil
	}
	out := &verdictResult{Requested: true, Mode: strings.TrimSpace(v.Mode)}
	if !verdictHasVerbs(v) {
		// An enabled verdict that names no verb is echoed as a refusal, never
		// silence: a bot that typo'd every verb must be able to tell "not
		// asked" from "asked and not executed" in the response.
		out.Refused = "verdict enabled but no verb recognized — nothing executed"
		return out
	}

	// (1) The gate is the anchor. An absent or disabled gate means the
	// caller has no check on the forge to explain the gestures; a gate that
	// did not post means the ordering authority refused it. Both refuse the
	// verbs: a merge a bot armed always has a check saying whose verdict
	// armed it, and a superseded verdict never mutates.
	if gateReq == nil || !gateReq.Enabled {
		out.Refused = "verdict gestures require an enabled gate block — the gate status is the audit and ordering anchor"
		return out
	}
	if !gate.posted {
		out.Refused = "the gate status did not post: " + gate.errText
		return out
	}
	sha := gate.sha

	// (2) The head, re-read NOW: the gate's snapshot is one status-write old,
	// and the GitHub approval rides the wire unpinned — a push landing in
	// that window must not collect an approval (or an arming) addressed to
	// the never-audited revision. One extra read per verdict-carrying
	// publish; both providers' arms keep their own sha pin under it.
	gc, gerr := s.gateClientFor(ctx, conn)
	if gerr != nil {
		out.Refused = "head re-read: " + gerr.Error()
		return out
	}
	if gc == nil {
		out.Refused = "provider " + string(conn.Provider) + " has no pull-request read to re-check the head"
		return out
	}
	pr, perr := gc.GetPullRequest(ctx, grant.Repo, number)
	if perr != nil {
		out.Refused = "head re-read: " + perr.Error()
		return out
	}
	if pr.State != "" && pr.State != "open" {
		out.Refused = "the pull request is " + pr.State + " — gestures never run on a revision that left the merge decision"
		return out
	}
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" {
		out.Refused = "the forge returned no head sha on the re-read"
		return out
	}
	if !equalSHA(head, sha) {
		out.Refused = "the head moved between the check and the gestures (check " + shortSHA(sha) + ", head is now " + shortSHA(head) + ")"
		return out
	}

	// (3) The bot's own pin, against the fresh head. Same refusal semantics
	// as the gate: stale is a refusal, nothing was written, a fresh verdict
	// must decide.
	audited := strings.TrimSpace(v.AuditedSHA)
	switch {
	case audited == "":
		out.Refused = "mutations are never unpinned: verdict.audited_sha is required"
		return out
	case !isCommitID(audited):
		out.Refused = "verdict.audited_sha " + strconv.Quote(boundedRunes(audited, 24)) + " is not a commit id"
		return out
	case !equalSHA(head, audited):
		out.Refused = "the head moved since the audit (audited " + audited + ", head is now " + head + ")"
		return out
	}
	// (4) Posture: dry_run is the bundle's declared no-act mode. A dry-run
	// verdict carrying verbs is by definition a mis-wired fold — refused
	// here, where the audit would otherwise only expose it after the merge
	// armed.
	normalized := strings.NewReplacer("-", "_").Replace(strings.TrimSpace(v.Mode))
	if strings.EqualFold(normalized, "dry_run") {
		out.Refused = "the verdict declares mode dry_run — a dry-run verdict never carries verbs; fix the bundle's fold"
		return out
	}
	// (5) Capabilities: what the grant may do, minted at launch from the
	// bot's manifest scopes. Missing capability is an in-band refusal — the
	// gate is already on the forge, and the caller's publish_health reads
	// the refusal from the response.
	if missing := verdictMissingCaps(grant.Capabilities, v); len(missing) > 0 {
		out.Refused = "the publish grant lacks capability " + strings.Join(missing, ", ") +
			" — declare the matching forge.token_scopes in the bot manifest (approvals/merge/reviewers)"
		s.logWarn("forge verdict: %s %s#%d: verbs refused: %s", conn.Provider, grant.Repo, number, out.Refused)
		return out
	}
	ops, err := s.mergeOpsFor(ctx, conn)
	if err != nil {
		out.Refused = "merge-gesture client: " + err.Error()
		return out
	}
	// (6) The check's color decides which verbs may follow it. Approval and
	// arming never follow a RED check — that is the fold's fail-closed
	// invariant enforced where no bundle bug can bypass it (stock GitLab does
	// not block a merge on external commit statuses, so a red check plus an
	// armed MWPS would merge). The ESCALATION verb — reviewers — stays
	// allowed on red: it is what a red verdict is FOR.
	green := gate.state == string(forge.CommitStateSuccess)

	// (7) Approve → reviewers → arm, in that order. Approval and arming run
	// only behind a GREEN check; the arming is skipped when the approval was
	// requested and failed: never arm on an unconfirmed approval.
	if v.Approve && !green {
		out.ApproveError = "skipped: the check posted red — approval and arming never follow a red gate"
	}
	if v.Approve && green {
		if ops.Approver == nil {
			out.ApproveError = "provider " + string(conn.Provider) + " implements no approval capability"
		} else if _, aerr := ops.Approver.ApprovePullRequest(ctx, grant.Repo, number, sha); aerr != nil {
			out.ApproveError = aerr.Error()
		} else {
			out.ApprovePosted = true
			if s.logger != nil {
				s.logger.Info("forge verdict: %s %s#%d @%s → approved (mode=%q)", conn.Provider, grant.Repo, number, sha, out.Mode)
			}
		}
	}
	if len(v.RequestReviewers) > 0 {
		if ops.Setter == nil {
			out.ReviewersError = "provider " + string(conn.Provider) + " implements no reviewer capability"
		} else if added, serr := ops.Setter.AddPullReviewers(ctx, grant.Repo, number, v.RequestReviewers); serr != nil {
			out.ReviewersError = serr.Error()
		} else {
			out.ReviewersAdded = added
		}
	}
	if v.ArmMergeWhenPipelineSucceeds {
		switch {
		case !green:
			out.MergeError = "skipped: the check posted red — approval and arming never follow a red gate"
		case v.Approve && !out.ApprovePosted:
			out.MergeError = "skipped: the approval was requested and failed — never arm on an unconfirmed approval"
		case ops.Armer == nil:
			out.MergeError = "provider " + string(conn.Provider) + " implements no auto-merge capability"
		default:
			method := forge.MergeMethod(v.MergeMethod)
			if strings.TrimSpace(v.MergeMethod) == "" {
				method = forge.MergeMerge
			}
			r, aerr := ops.Armer.ArmAutoMerge(ctx, grant.Repo, number, forge.AutoMergeOptions{
				Method:       method,
				SHA:          sha,
				DeleteBranch: v.RemoveSourceBranch,
			})
			if aerr != nil {
				out.MergeError = aerr.Error()
			} else {
				out.MergeArmed = true
				out.MergeState = r.State
			}
		}
	}
	// Every mutating verb is audited — approved, armed, reviewers added, and
	// the mode that claimed to drive them.
	s.auditSystem(grant.TeamID, "forge-verdict", auditActionVerdictGesture, grant.Repo, fmt.Sprintf("%s#%d@%s", grant.Repo, number, sha), map[string]any{
		"mode":            out.Mode,
		"approve":         out.ApprovePosted,
		"reviewers":       out.ReviewersAdded,
		"merge_armed":     out.MergeArmed,
		"merge_state":     out.MergeState,
		"approve_error":   out.ApproveError,
		"merge_error":     out.MergeError,
		"reviewers_error": out.ReviewersError,
	})
	return out
}

// verdictMissingCaps returns the capability names a verdict requests that the
// grant does not carry, sorted for a deterministic message.
func verdictMissingCaps(caps []string, v *publishReviewVerdict) []string {
	have := make(map[string]bool, len(caps))
	for _, c := range caps {
		have[c] = true
	}
	var missing []string
	if v.Approve && !have[publishCapApprove] {
		missing = append(missing, publishCapApprove)
	}
	if v.ArmMergeWhenPipelineSucceeds && !have[publishCapMerge] {
		missing = append(missing, publishCapMerge)
	}
	if len(v.RequestReviewers) > 0 && !have[publishCapReviewers] {
		missing = append(missing, publishCapReviewers)
	}
	sort.Strings(missing)
	return missing
}

// auditActionVerdictGesture is the audit action every executed merge-gate
// gesture records, with the mode that claimed to drive it.
const auditActionVerdictGesture = "forge.verdict.gesture"

// forgePublishCapabilitiesFor resolves the merge-gate capabilities a bot's
// manifest declares, as grant capability names. A manifest that cannot be
// resolved (an inline .bot, a loose bundle) declares nothing: empty is the
// fail-closed default, and a launch that expected gestures learns it from
// the verdict_result refusal instead of a launch failure.
func (s *Server) forgePublishCapabilitiesFor(ctx context.Context, teamID, botID string) []string {
	m := s.botManifestFor(ctx, teamID, strings.TrimSpace(botID))
	if m == nil || m.Forge == nil {
		return nil
	}
	scopes := m.Forge.TokenScopes
	atWrite := func(key string) bool {
		switch scopes[key] {
		case "write", "admin":
			return true
		default:
			return false
		}
	}
	var caps []string
	if atWrite(bundle.ForgeScopeApprovals) {
		caps = append(caps, publishCapApprove)
	}
	if atWrite(bundle.ForgeScopeMerge) {
		caps = append(caps, publishCapMerge)
	}
	if atWrite(bundle.ForgeScopeReviewers) {
		caps = append(caps, publishCapReviewers)
	}
	sort.Strings(caps)
	return caps
}

// publishMutationKillSwitchVar is the operator launch-var that strips every
// merge-gesture capability from a grant at mint: set it to anything but
// "true" (per integration launch_vars) and the repo's bots keep commenting
// and gating but can no longer mutate — the per-repo disarm that needs no
// release and no bot redeploy.
const publishMutationKillSwitchVar = "forge_publish_mutations"
