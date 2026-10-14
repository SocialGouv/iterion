package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// gitlabMRDiffRefs is the subset of GET /merge_requests/:iid needed to
// anchor positioned discussions.
type gitlabMRDiffRefs struct {
	WebURL   string `json:"web_url"`
	DiffRefs struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
}

// renderGitLabCommentBody renders a finding as the note body GitLab is sent:
// the finding text with its quick-action lines made inert, then GitLab's
// one-click suggestion fence. The ```suggestion:-0+N syntax extends the
// applied range N lines below the anchored line, which maps a Line..LineEnd
// span with new_line = Line. A replacement with a line GitLab could run as a
// quick action cannot be fenced as given — making it inert would change the
// code the click applies — so it is shown as text, inert, and fenced reports
// false: no one-click suggestion was posted. The finding text can carry
// suggestion fences of its own, which GitLab makes applicable too: when the
// guard rewrites a line of that text, the suggestion fences it opens at the
// top level become `text` blocks, so no rewritten code is one click away.
func renderGitLabCommentBody(c forge.ReviewComment) (body string, fenced bool) {
	body = forge.InertGitLabQuickActions(c.Body)
	if forge.GitLabRunsQuickActionLine(c.Body) {
		body = suggestionFenceInfo.ReplaceAllString(body, "${1}text")
	}
	replacement := strings.TrimRight(strings.ReplaceAll(c.Suggestion, "\r", ""), "\n")
	if strings.TrimSpace(replacement) == "" {
		return body, false
	}
	if forge.GitLabRunsQuickActionLine(replacement) {
		return body + "\n\nProposed replacement — not applicable in one click, a line of it reads as a GitLab quick action:\n```\n" +
			forge.InertGitLabQuickActions(replacement) + "\n```", false
	}
	below := 0
	if c.LineEnd > c.Line {
		below = c.LineEnd - c.Line
	}
	return body + "\n\n```suggestion:-0+" + strconv.Itoa(below) + "\n" + replacement + "\n```", true
}

// CreatePullReview publishes a review onto a merge request. GitLab has no
// bot-postable single "review" object, so the equivalent is: one positioned
// discussion per inline comment (each an independent API call — naturally
// partial-tolerant) plus one summary note carrying the review body and any
// comments the merge request does not hold.
//
// What the merge request holds is READ BACK after the creates, and that
// read decides CommentsPosted and what the summary folds — not the create
// answers. Measured on a GitLab 17 instance: two positioned discussions
// persisted behind an error answer; counted from the answers alone the
// review reported 0 inline comments, folded both findings into the summary a
// second time, and raised a "publish failed" banner over a merge request
// that held every thread. A create that answered 2xx for a thread the
// read-back does not find is treated the same way in reverse: folded, so a
// lost finding still reaches the reader. A thread is recognised by the
// discussion id the create answered with, else — the create having answered
// nothing usable — by an unresolved note on the anchored head carrying the
// comment's file, line and body; a merge request that holds fewer such
// notes than the publish wrote folds the rest. The read is retried a bounded
// number of times while a comment is still missing (a forge can persist a
// create after the client gave up on it). Verified is true when the
// read-back answered; when it did not, the create answers are reported as
// the best count there is, unverified, with the read-back's failure in
// Diagnostics. Nothing in the read-back fails a publish whose creates
// succeeded.
func (c *AdminClient) CreatePullReview(ctx context.Context, repo string, number int, in forge.NewReview) (forge.ReviewResult, error) {
	mrPath := "/projects/" + projectID(repo) + "/merge_requests/" + strconv.Itoa(number)

	var mr gitlabMRDiffRefs
	code, err := c.do(ctx, http.MethodGet, mrPath, nil, &mr)
	if err != nil {
		return forge.ReviewResult{}, err
	}
	if code != http.StatusOK {
		return forge.ReviewResult{}, statusErr("get merge request", code)
	}

	outcomes := make([]threadOutcome, 0, len(in.Comments))
	for _, rc := range in.Comments {
		o := threadOutcome{rc: rc}
		o.body, o.suggestion = renderGitLabCommentBody(rc)
		req := map[string]any{
			"body": o.body,
			"position": map[string]any{
				"position_type": "text",
				"base_sha":      mr.DiffRefs.BaseSHA,
				"head_sha":      mr.DiffRefs.HeadSHA,
				"start_sha":     mr.DiffRefs.StartSHA,
				"new_path":      rc.Path,
				"new_line":      rc.Line,
			},
		}
		var created struct {
			ID string `json:"id"`
		}
		dcode, errBody, derr := c.http().DoErrBody(ctx, http.MethodPost, mrPath+"/discussions", req, &created)
		switch {
		case derr != nil:
			// A rate-limited answer arrives typed from the transport, never
			// as a status: it is a 4xx the forge pronounced, not an answer
			// that was lost.
			var se *forge.StatusError
			if errors.As(derr, &se) {
				o.refused = se.Code/100 == 4
			}
			o.errText = oneLine(derr.Error())
		case dcode/100 != 2:
			o.errText = "HTTP " + strconv.Itoa(dcode)
			if head := forge.TrimBody(errBody); head != "" {
				o.errText += " " + head
			}
			o.refused = dcode/100 == 4
		default:
			o.created = true
			o.id = created.ID
		}
		outcomes = append(outcomes, o)
	}

	// The read-back: only when there is something to verify. A review of
	// nothing but a summary (a budget note, a guard's verdict) has no thread
	// to look for, and a read it does not need could only delay or
	// discredit it. The read runs under its own budget, so the summary note
	// and the gate keep the caller's remaining time whatever the forge does
	// to the discussion list.
	verified := true
	var readErr error
	var diags []string
	if len(outcomes) > 0 {
		budget, budgetDiag := readBackBudgetFromEnv()
		if budgetDiag != "" {
			diags = append(diags, budgetDiag)
		}
		rbCtx, cancel := context.WithTimeout(ctx, budget)
		readErr = c.matchThreads(rbCtx, mrPath, mr.DiffRefs.HeadSHA, outcomes)
		if readErr != nil && rbCtx.Err() != nil && ctx.Err() == nil {
			readErr = fmt.Errorf("%w (read-back budget of %s exhausted)", readErr, budget)
		}
		cancel()
		verified = readErr == nil
	}
	for i := range outcomes {
		o := &outcomes[i]
		if !verified {
			o.landed = o.created
		}
		// The path is the bot's own output: bounded and quoted, it cannot
		// write a line of its own into the operator's log nor flood it.
		pathHead := forge.TrimBody([]byte(o.rc.Path))
		switch {
		case o.errText != "" && o.landed:
			diags = append(diags, fmt.Sprintf("inline comment %q:%d: %s — the merge request holds it anyway", pathHead, o.rc.Line, o.errText))
		case o.errText != "":
			diags = append(diags, fmt.Sprintf("inline comment %q:%d: %s", pathHead, o.rc.Line, o.errText))
		case verified && !o.landed:
			diags = append(diags, fmt.Sprintf("inline comment %q:%d: the create answered 2xx but the merge request does not hold it — folded into the summary", pathHead, o.rc.Line))
		}
	}
	if readErr != nil {
		diags = append(diags, "discussion read-back: "+oneLine(readErr.Error())+" — the counts are the create answers, unverified")
	}

	posted, suggestions := 0, 0
	var failed []forge.ReviewComment
	for _, o := range outcomes {
		if !o.landed {
			failed = append(failed, o.rc)
			continue
		}
		posted++
		if o.suggestion {
			suggestions++
		}
	}

	summary := in.Body
	if len(failed) > 0 {
		summary += "\n\n" + forge.FoldCommentsMarkdown(failed)
	}
	summary = forge.InertGitLabQuickActions(summary)
	ncode, nerr := c.do(ctx, http.MethodPost, mrPath+"/notes", map[string]any{"body": summary}, &struct{}{})
	noteOK := nerr == nil && ncode/100 == 2
	if !noteOK && posted == 0 {
		// Nothing landed at all — surface the failure, never fake success.
		// The diagnostics travel with the error: what each create answered
		// is the operator's only lead on a review that landed nowhere.
		if nerr != nil {
			return forge.ReviewResult{Diagnostics: diags}, fmt.Errorf("create merge request summary note: %w", nerr)
		}
		return forge.ReviewResult{Diagnostics: diags}, statusErr("create merge request summary note", ncode)
	}

	res := forge.ReviewResult{URL: mr.WebURL, CommentsPosted: posted, SuggestionsPosted: suggestions, Verified: verified, Diagnostics: diags}
	if len(failed) > 0 || !noteOK {
		res.Fallback = "partial"
		if posted == 0 {
			res.Fallback = "summary"
		}
	}
	return res, nil
}

// suggestionFenceInfo matches the word `suggestion` opening the info string
// of a fence line — backticks or tildes, up to three blanks of indent — so
// that word alone becomes `text` (`suggestion:-0+1` reads as `text:-0+1`,
// GitLab taking the language before the colon). Every other byte of the line
// stays: whether the line opens or closes a block, or opens none at all, is
// decided by bytes the rename does not touch. A fence nested deeper (in a
// list item, an alert, a footnote) is not matched and needs no demotion: a
// line the guard rewrites starts at column 0, which ends such a container.
var suggestionFenceInfo = regexp.MustCompile("(?mi)^([ \t]{0,3}(?:`{3,}|~{3,})[ \t]*)suggestion\\b")

// threadOutcome is one inline comment's fate: what the create answered, and
// what the read-back found.
type threadOutcome struct {
	rc         forge.ReviewComment
	body       string
	suggestion bool   // body carries a one-click suggestion block
	id         string // the discussion id a 2xx create answered with ("" otherwise)
	created    bool   // the create answered 2xx
	refused    bool   // the create answered 4xx: a verdict, not a lost answer
	landed     bool   // the merge request holds it (the read-back's verdict)
	errText    string
}

// positionedNoteKey identifies a positioned note the way the publish wrote
// it: the file, the line and the body. GitLab stores a note body with every
// carriage return deleted and trailing whitespace stripped (its quick-action
// extractor runs on every note), so both sides go through noteBodyKey before
// comparing. A quick-action line (`/rebase` alone on a line) is removed by
// the same extractor and is not replicated here: a 2xx create is recognised
// by its discussion id regardless, and only a create that answered nothing
// usable depends on the body.
type positionedNoteKey struct {
	path string
	line int
	body string
}

// noteBodyKey normalises a note body the way GitLab stores it, as far as a
// comparison can: no carriage returns, no surrounding whitespace.
func noteBodyKey(body string) string {
	return strings.TrimSpace(strings.ReplaceAll(body, "\r", ""))
}

// oneLine flattens a forge's error text onto one line for a diagnostic: a
// typed answer carries the forge's own message, line breaks included.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// heldThreads is what one read-back found on the merge request: every
// discussion by id (with the key of its first positioned note, consumed when
// the id matches), and the unresolved positioned notes on the anchored
// head, COUNTED per key — two identical comments need two threads, so one
// persisted thread cannot vouch for both.
type heldThreads struct {
	byID  map[string]positionedNoteKey
	notes map[positionedNoteKey]int
}

func (h *heldThreads) takeID(id string) bool {
	k, ok := h.byID[id]
	if !ok {
		return false
	}
	if h.notes[k] > 0 {
		h.notes[k]--
	}
	return true
}

func (h *heldThreads) takeNote(k positionedNoteKey) bool {
	if h.notes[k] <= 0 {
		return false
	}
	h.notes[k]--
	return true
}

// readBackBudget bounds the whole read-back (every read and pause): the
// summary note and the merge gate follow it on the caller's clock, and a
// forge slow to list its discussions must cost the publish a verification,
// never the note. Past the budget the counts are the create answers,
// unverified, and the diagnostic says so. ReadBackBudgetEnv overrides it.
var readBackBudget = 15 * time.Second

// ReadBackBudgetEnv names the operator's override of readBackBudget, a Go
// duration (`45s`): the escape hatch for an instance whose discussion list
// pages slower than the default allows, so its publishes verify instead of
// reporting the create answers.
const ReadBackBudgetEnv = "ITERION_FORGE_READBACK_BUDGET"

// readBackBudgetFromEnv is the budget in force, and a diagnostic when the
// override is set to something that is not a positive duration (the
// default then applies — said, never silent).
func readBackBudgetFromEnv() (time.Duration, string) {
	raw := strings.TrimSpace(os.Getenv(ReadBackBudgetEnv))
	if raw == "" {
		return readBackBudget, ""
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return readBackBudget, fmt.Sprintf("%s=%q is not a positive duration — the default of %s applies", ReadBackBudgetEnv, forge.TrimBody([]byte(raw)), readBackBudget)
	}
	return d, ""
}

// readBackRetries is how many times the read-back is repeated, after a
// pause of readBackRetryDelay, while a comment whose fate is still open is
// missing from the merge request: a forge can finish persisting a create
// after the client gave up on the answer, and a snapshot taken at once would
// fold a thread the reader will see twice. A read that FAILED is repeated
// only while no read has answered and a create's fate is open — the one
// shape where a second read can still turn a duplicate into a count, and
// the shape the measured miscount has: a create persisted behind an error
// answer on an instance whose list endpoint hiccups too. A create the forge
// REFUSED (4xx) has its fate: nothing will appear, and it does not earn a
// second read. Bounded by the count here and by readBackBudget: a thread
// that never appears is folded after the last read, with its diagnostic.
const readBackRetries = 2

var readBackRetryDelay = time.Second

// matchThreads reads the merge request back and sets landed on every
// outcome: first by discussion id (2xx creates), then by note key (the
// rescue of a create whose answer was lost), each read starting the match
// over on a fresh snapshot. When a comment has no id to be taken by, the
// merge request's head as it is NOW is read alongside: a push during the
// publish makes GitLab trace a fresh note's position onto the new head (the
// API exposes the traced position only), so a note on either head keys on
// what the publish wrote. A head read that fails keeps the last head read
// (never narrowing what an earlier read accepted), and is reported only when
// the budget ran out on it — the one failure the list's own error would
// otherwise be blamed for. A later read that fails, or a pause the context
// cuts, keeps the earlier verdict; with no verdict yet, the last failure is
// reported.
func (c *AdminClient) matchThreads(ctx context.Context, mrPath, headSHA string, outcomes []threadOutcome) error {
	answered := false
	var lastErr error
	currentHead := ""
	for attempt := 0; attempt <= readBackRetries; attempt++ {
		if attempt > 0 {
			pause := time.NewTimer(readBackRetryDelay)
			select {
			case <-ctx.Done():
				pause.Stop()
				if answered {
					return nil
				}
				return lastErr
			case <-pause.C:
			}
		}
		if anyWithoutID(outcomes) {
			h, herr := c.currentHead(ctx, mrPath)
			if herr != nil && ctx.Err() != nil {
				if answered {
					return nil
				}
				return fmt.Errorf("gitlab: merge request head read: %w", herr)
			}
			if h != "" {
				currentHead = h
			}
		}
		held, err := c.listHeldThreads(ctx, mrPath, headSHA, currentHead)
		if err != nil {
			if answered {
				return nil
			}
			lastErr = err
			if !anyOpenFate(outcomes) || ctx.Err() != nil {
				return err
			}
			continue
		}
		answered = true
		open := false // a comment missing whose create was not refused
		for i := range outcomes {
			o := &outcomes[i]
			o.landed = o.id != "" && held.takeID(o.id)
		}
		for i := range outcomes {
			o := &outcomes[i]
			if !o.landed {
				o.landed = held.takeNote(positionedNoteKey{path: o.rc.Path, line: o.rc.Line, body: noteBodyKey(o.body)})
			}
			if !o.landed && !o.refused {
				open = true
			}
		}
		if !open {
			return nil
		}
	}
	if answered {
		return nil
	}
	return lastErr
}

// anyOpenFate reports a create whose answer was lost: neither 2xx nor a
// refusal — the thread may be on the merge request, and only a read can
// tell.
func anyOpenFate(outcomes []threadOutcome) bool {
	for _, o := range outcomes {
		if !o.created && !o.refused {
			return true
		}
	}
	return false
}

// anyWithoutID reports a comment the read-back can only take by key: its
// create answered no discussion id (the answer was lost, refused, or
// carried none).
func anyWithoutID(outcomes []threadOutcome) bool {
	for _, o := range outcomes {
		if o.id == "" {
			return true
		}
	}
	return false
}

// currentHead is the merge request's head sha as it is now. A failed read
// answers "" with its error: the caller keeps the last head it read (best
// effort — a push during the publish is the rare case this serves) and
// reports the failure only when the budget ran out on it.
func (c *AdminClient) currentHead(ctx context.Context, mrPath string) (string, error) {
	var mr gitlabMRDiffRefs
	code, err := c.do(ctx, http.MethodGet, mrPath, nil, &mr)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", statusErr("get merge request", code)
	}
	return mr.DiffRefs.HeadSHA, nil
}

// readBackPageCap bounds the discussion read-back: a merge request past this
// many pages of 100 is not one a review lands on, and a forge that keeps
// announcing a next page would otherwise hold the publish forever. Past the
// cap the read-back fails (reported unverified), never silently truncates.
const readBackPageCap = 100

// listHeldThreads reads back every discussion on the merge request,
// following GitLab's X-Next-Page pagination, and indexes the unresolved
// positioned notes anchored on headSHA — or on currentHead, the merge
// request's head as read alongside, when a push moved it meanwhile. A
// resolved thread is one the reader has closed and the view collapses; a
// thread on an earlier head is one the current diff does not show: neither
// vouches for a finding raised now. The error names the page and the status
// so the operator reads WHY the counts went unverified.
func (c *AdminClient) listHeldThreads(ctx context.Context, mrPath, headSHA, currentHead string) (*heldThreads, error) {
	held := &heldThreads{byID: map[string]positionedNoteKey{}, notes: map[positionedNoteKey]int{}}
	for page := 1; ; page++ {
		if page > readBackPageCap {
			return nil, fmt.Errorf("gitlab: discussion read-back: more than %d pages", readBackPageCap)
		}
		var discussions []struct {
			ID    string `json:"id"`
			Notes []struct {
				Body     string `json:"body"`
				System   bool   `json:"system"`
				Resolved bool   `json:"resolved"`
				Position *struct {
					NewPath string `json:"new_path"`
					NewLine int    `json:"new_line"`
					HeadSHA string `json:"head_sha"`
				} `json:"position"`
			} `json:"notes"`
		}
		code, hdr, err := c.http().DoFull(ctx, http.MethodGet, mrPath+"/discussions?per_page=100&page="+strconv.Itoa(page), nil, &discussions)
		if err != nil {
			return nil, fmt.Errorf("gitlab: discussion read-back page %d: %w", page, err)
		}
		if code/100 != 2 {
			return nil, fmt.Errorf("gitlab: discussion read-back page %d: HTTP %d", page, code)
		}
		for _, d := range discussions {
			var first positionedNoteKey
			for _, n := range d.Notes {
				if n.System || n.Resolved || n.Position == nil {
					continue
				}
				if n.Position.HeadSHA != headSHA && (currentHead == "" || n.Position.HeadSHA != currentHead) {
					continue
				}
				k := positionedNoteKey{path: n.Position.NewPath, line: n.Position.NewLine, body: noteBodyKey(n.Body)}
				held.notes[k]++
				if first == (positionedNoteKey{}) {
					first = k
				}
			}
			if d.ID != "" {
				held.byID[d.ID] = first
			}
		}
		next := strings.TrimSpace(hdr.Get("X-Next-Page"))
		if next == "" || next == strconv.Itoa(page) {
			return held, nil
		}
	}
}

// AddSelfAsPullReviewer puts the token's own account on the MR's reviewer
// set (forge.ReviewerAssigner). GitLab's `reviewer_ids` PUT REPLACES the
// whole set, so this is a read-modify-write: fetch the current reviewers,
// no-op when already present, else append and write the union back — never
// dropping the humans already on it.
func (c *AdminClient) AddSelfAsPullReviewer(ctx context.Context, repo string, number int) error {
	me, err := c.WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("resolve own account: %w", err)
	}
	// A 200 whose body lacks `id` (an SSO interstitial, an error envelope)
	// decodes to 0 — and GitLab treats a 0 in reviewer_ids as "add nobody",
	// so the PUT would be a silent no-op reported here as success. Refuse
	// loudly instead (one branch: WhoAmI formats the id itself, so a
	// non-numeric string is the same "not a usable id" failure).
	myID, err := strconv.ParseInt(me.ID, 10, 64)
	if err != nil || myID <= 0 {
		return fmt.Errorf("own account id %q is not a usable GitLab user id", me.ID)
	}

	mrPath := "/projects/" + projectID(repo) + "/merge_requests/" + strconv.Itoa(number)
	var mr struct {
		Reviewers *[]struct {
			ID int64 `json:"id"`
		} `json:"reviewers"`
	}
	code, err := c.do(ctx, http.MethodGet, mrPath, nil, &mr)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return statusErr("get merge request reviewers", code)
	}
	if mr.Reviewers == nil {
		// The PUT below REPLACES the reviewer set. A response that carries
		// no `reviewers` field at all means the read half of the
		// read-modify-write saw nothing — writing would replace reviewers we
		// could not see. Refuse rather than risk a destructive write.
		return fmt.Errorf("merge request response carries no reviewers field — refusing a replace-write that cannot prove itself additive")
	}
	ids := make([]int64, 0, len(*mr.Reviewers)+1)
	for _, r := range *mr.Reviewers {
		if r.ID == myID {
			return nil // already a reviewer — the re-request button is up
		}
		ids = append(ids, r.ID)
	}
	ids = append(ids, myID)

	code, err = c.do(ctx, http.MethodPut, mrPath, map[string]any{"reviewer_ids": ids}, &struct{}{})
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return statusErr("set merge request reviewers", code)
	}
	return nil
}
