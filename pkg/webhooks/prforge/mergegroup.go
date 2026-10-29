package prforge

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// EventHeaderMergeGroup is the X-GitHub-Event value for a merge-queue group.
// GitHub-only: no other forge this server speaks to has a merge queue.
//
// A merge-queue branch is NOT a pull request. GitHub builds
// `gh-readonly-queue/<base>/pr-<n>-<base-sha>` from the entry and fires
// `merge_group`, so a bot that answers `pull_request` events never sees the
// revision the queue actually wants a verdict on — the required check is
// simply absent there, the group waits for a context that cannot arrive, and
// the entry is dequeued on timeout with every entry behind it.
const EventHeaderMergeGroup = "merge_group"

// MergeGroupActionChecksRequested is the one action that asks for a verdict.
// `destroyed` means the group is gone (merged, or the entry was removed), and
// a status posted then lands on a revision nobody is deciding about.
const MergeGroupActionChecksRequested = "checks_requested"

// MergeGroupEvent is the subset of the merge_group payload this server reads.
type MergeGroupEvent struct {
	Action     string     `json:"action"`
	Repository Repository `json:"repository"`
	MergeGroup struct {
		HeadSHA string `json:"head_sha"`
		HeadRef string `json:"head_ref"`
		BaseSHA string `json:"base_sha"`
		BaseRef string `json:"base_ref"`
	} `json:"merge_group"`
	Sender Sender `json:"sender"`
}

// ParsedMergeGroup is the normalized view the gate lane consumes: the
// revision the queue built, and the pull request it was built from.
type ParsedMergeGroup struct {
	RepoID      int64
	ProjectPath string // "owner/repo"
	RepoURL     string // html_url — the host the connection is resolved on
	Action      string
	QueueSHA    string // the revision the queue wants a verdict ON
	HeadRef     string // refs/heads/gh-readonly-queue/<base>/pr-<n>-<base-sha>
	BaseRef     string
	PRNumber    int
	SenderLogin string
}

// ChecksRequested reports whether this delivery is the one asking for a
// verdict on the queue revision.
func (p ParsedMergeGroup) ChecksRequested() bool {
	return p.Action == MergeGroupActionChecksRequested
}

// queueRefPR reads the entry's pull request out of the queue ref.
//
// Anchored at the END and nowhere else: the BASE branch is part of the ref and
// may itself contain `/pr-12-abc` (a branch named `release/pr-12-abcdef0` is
// legal), so an unanchored search would answer with the base's digits. The
// base-sha tail is hex and of no fixed width across forges, hence `+`.
var queueRefPR = regexp.MustCompile(`/pr-([0-9]+)-[0-9a-fA-F]+$`)

// ParseMergeGroup decodes a merge_group delivery.
//
// A ref whose pull request cannot be read is an ERROR, never a guess: the
// number decides which PR's verdict is mirrored onto this revision, and
// mirroring the wrong one would certify an unaudited commit.
func ParseMergeGroup(body []byte) (ParsedMergeGroup, error) {
	var e MergeGroupEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return ParsedMergeGroup{}, fmt.Errorf("decode merge_group: %w", err)
	}
	out := ParsedMergeGroup{
		RepoID:      e.Repository.ID,
		ProjectPath: strings.TrimSpace(e.Repository.FullName),
		RepoURL:     strings.TrimSpace(e.Repository.HTMLURL),
		Action:      strings.TrimSpace(e.Action),
		QueueSHA:    strings.TrimSpace(e.MergeGroup.HeadSHA),
		HeadRef:     strings.TrimSpace(e.MergeGroup.HeadRef),
		BaseRef:     strings.TrimSpace(e.MergeGroup.BaseRef),
		SenderLogin: strings.TrimSpace(e.Sender.Login),
	}
	if out.ProjectPath == "" {
		return ParsedMergeGroup{}, fmt.Errorf("merge_group carries no repository full_name")
	}
	if out.QueueSHA == "" {
		return ParsedMergeGroup{}, fmt.Errorf("merge_group for %s carries no head_sha — there is no revision to decide about", out.ProjectPath)
	}
	m := queueRefPR.FindStringSubmatch(out.HeadRef)
	if m == nil {
		return ParsedMergeGroup{}, fmt.Errorf("merge_group head_ref %q on %s names no pull request (expected a %s tail) — which PR's verdict this revision carries is not decidable",
			out.HeadRef, out.ProjectPath, "…/pr-<number>-<base-sha>")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return ParsedMergeGroup{}, fmt.Errorf("merge_group head_ref %q on %s carries an unusable pull request number %q", out.HeadRef, out.ProjectPath, m[1])
	}
	out.PRNumber = n
	return out, nil
}
