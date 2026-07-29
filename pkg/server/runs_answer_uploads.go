package server

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Operator uploads at a human pause ("late-bound attachments").
//
// The launch path already promotes staged uploads into run attachments
// (promoteStaged, driven by launchRunRequest.Attachments). A human gate
// needs the same thing at a different moment: the operator picks a file
// while ANSWERING, not while launching. Rather than invent a second
// storage lane, a gate upload becomes an ordinary run attachment —
// which means it inherits, for free, the sandbox bind-mount, the
// presign/serve endpoints, the SHA-256 + MIME validation, and
// {{attachments.*}} template resolution.
//
// Two shapes reach this file, matching the two things an operator does:
//
//  1. A DECLARED `file` field on the gate's output schema. The answer
//     value arrives as an upload envelope, `{"upload_id": "..."}`, and is
//     rewritten in place into a descriptor the workflow can dereference
//     (`{{outputs.<gate>.<field>.path}}`).
//  2. An AD-HOC attachment (the always-available 📎 button on any gate,
//     no DSL required — the "here's a diagram with my feedback" case).
//     These arrive out-of-band in resumeRunRequest.Attachments and land
//     on the reserved `_attachments` answer key as a list of descriptors.
//
// `path` is deliberately NOT filled here. The server knows the host
// filesystem; the RUNTIME knows whether the run is sandboxed and must
// hand the agent a container path instead. Leaving path to the resume
// path keeps that resolution in exactly one place (see
// engine.resolveFileAnswers).

// answerUploadsKey is the reserved answer key carrying ad-hoc gate
// attachments. Underscore-prefixed so it cannot collide with a
// schema-declared field name (the DSL parses field names as
// identifiers, which never start with '_' by convention and are always
// author-chosen — whereas this key is engine-owned).
const answerUploadsKey = "_attachments"

// uploadEnvelopeKey is the marker field identifying an answer value as
// "bytes I already staged" rather than literal JSON data.
const uploadEnvelopeKey = "upload_id"

// maxAdHocGateUploads bounds the ad-hoc attachments accepted on a single
// resume. promoteAnswerUploads separately enforces MaxUploadsPerRun
// against the run's EXISTING attachment count plus this batch (promotion
// runs on every resume, so a per-call check alone would let a looping
// gate grow the run without bound); this is the per-answer guard so one
// submission cannot consume the whole budget in a single shot.
const maxAdHocGateUploads = 10

// asUploadEnvelope reports whether val is an upload envelope and returns
// the staged upload ID it references.
//
// The envelope is recognised structurally rather than by consulting the
// compiled schema: the resume handler would otherwise have to compile
// the workflow just to learn which answer keys are `file`-typed, and it
// deliberately does not (the engine re-compiles on resume, and doing it
// twice invites the two copies to disagree). To keep the structural
// match from swallowing a legitimate `json` field, the envelope must
// carry `upload_id` AND nothing beyond the optional `filename` hint.
func asUploadEnvelope(val any) (string, bool) {
	m, ok := val.(map[string]any)
	if !ok {
		return "", false
	}
	raw, ok := m[uploadEnvelopeKey]
	if !ok {
		return "", false
	}
	id, ok := raw.(string)
	if !ok || strings.TrimSpace(id) == "" {
		return "", false
	}
	for k := range m {
		if k != uploadEnvelopeKey && k != "filename" {
			return "", false
		}
	}
	return id, true
}

// hasUploadEnvelope reports whether any answer value is an upload
// envelope. Lets the resume handler skip the promotion path entirely
// (and its store round-trip) on the overwhelmingly common no-upload
// answer.
func hasUploadEnvelope(answers map[string]any) bool {
	for _, v := range answers {
		if _, ok := asUploadEnvelope(v); ok {
			return true
		}
	}
	return false
}

// attachmentDescriptor projects a promoted attachment into the map a
// workflow reads off the answer. `path` is added later by the runtime.
func attachmentDescriptor(rec store.AttachmentRecord) map[string]any {
	return map[string]any{
		"attachment": rec.Name,
		"filename":   rec.OriginalFilename,
		"mime":       rec.MIME,
		"size":       rec.Size,
		"sha256":     rec.SHA256,
	}
}

// gateAttachmentName derives the run-unique attachment name for a gate
// upload. The name is author-predictable — `{{attachments.<node>.<field>}}`
// resolves without the author having to know an opaque upload id — but
// must stay unique across re-answers: a rejected verdict can send the
// workflow back through the same gate, and a second upload to the same
// field must not clobber the first (an in-flight prompt may still
// reference it). Collisions therefore get a numeric suffix, mirroring
// the worktree branch-name policy.
func gateAttachmentName(nodeID, field string, taken map[string]bool) string {
	sanitize := func(s string) string {
		s = strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				return r
			case r == '-', r == '_', r == '.':
				return r
			}
			return '-'
		}, s)
		return strings.Trim(s, "-.")
	}
	base := sanitize(field)
	if n := sanitize(nodeID); n != "" && base != "" {
		base = n + "." + base
	} else if base == "" {
		base = n
	}
	if base == "" {
		base = "upload"
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// promoteAnswerUploads moves every upload referenced by a resume — both
// declared `file` fields and ad-hoc gate attachments — out of the
// staging area and into the run's attachments, then rewrites the answers
// so downstream nodes see descriptors instead of upload ids.
//
// It is transactional in the same sense promoteStaged is: a failure
// anywhere rolls back every attachment written during THIS call, so a
// rejected resume never leaves the run half-populated. Attachments the
// run already had (from launch, or from an earlier pass through the same
// gate) are untouched.
//
// Returns the rewritten answers. When nothing was uploaded the input map
// is returned unchanged, so the ordinary no-upload resume pays nothing.
func (s *Server) promoteAnswerUploads(
	ctx context.Context,
	runID, nodeID string,
	answers map[string]any,
	adHoc []string,
) (map[string]any, error) {
	// Collect declared-field envelopes. Sorted for determinism: the
	// suffix a collision receives must not depend on Go's map iteration
	// order, or two identical resumes would name attachments differently.
	fields := make([]string, 0, len(answers))
	for k, v := range answers {
		if _, ok := asUploadEnvelope(v); ok {
			fields = append(fields, k)
		}
	}
	sort.Strings(fields)

	if len(fields) == 0 && len(adHoc) == 0 {
		return answers, nil
	}
	if len(adHoc) > maxAdHocGateUploads {
		return nil, fmt.Errorf("too many attachments on one answer: %d > %d", len(adHoc), maxAdHocGateUploads)
	}

	// Names already used by this run — promoting onto one of them would
	// overwrite bytes an earlier node may still be referencing. A failed
	// listing must NOT be read as "this run has no attachments yet": that
	// silently turns the collision guard off and clobbers those bytes on
	// the first name collision. Fail the resume instead; the operator
	// retries with the staging still intact.
	existing, err := s.runs.ListAttachments(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list existing attachments: %w", err)
	}
	taken := make(map[string]bool, len(existing))
	for _, a := range existing {
		taken[a.Name] = true
	}

	// promoteStaged enforces MaxUploadsPerRun per CALL, which was a
	// per-run ceiling back when promotion happened once, at launch. It now
	// runs on every resume, so a gate inside a loop could be answered N
	// times and grow the run's attachments without bound. Count what the
	// run already carries.
	if total := len(taken) + len(fields) + len(adHoc); total > s.cfg.MaxUploadsPerRun {
		return nil, fmt.Errorf("too many attachments on this run: %d > %d (--max-uploads-per-run)", total, s.cfg.MaxUploadsPerRun)
	}

	mapping := make(map[string]string, len(fields)+len(adHoc))
	fieldToName := make(map[string]string, len(fields))
	for _, f := range fields {
		id, _ := asUploadEnvelope(answers[f])
		name := gateAttachmentName(nodeID, f, taken)
		taken[name] = true
		mapping[name] = id
		fieldToName[f] = name
	}
	adHocNames := make([]string, 0, len(adHoc))
	for i, id := range adHoc {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("attachment %d: empty upload id", i)
		}
		name := gateAttachmentName(nodeID, fmt.Sprintf("attachment-%d", i+1), taken)
		taken[name] = true
		mapping[name] = id
		adHocNames = append(adHocNames, name)
	}

	promoted, _, err := s.promoteStaged(ctx, runID, mapping)
	if err != nil {
		return nil, err
	}

	// Rewrite in a COPY: on any later failure the caller still holds the
	// operator's original answers, and a partially-rewritten map (some
	// fields descriptors, some still envelopes) never escapes.
	out := make(map[string]any, len(answers)+1)
	for k, v := range answers {
		out[k] = v
	}
	for field, name := range fieldToName {
		rec, ok := promoted[name]
		if !ok {
			return nil, fmt.Errorf("attachment %q was not promoted", name)
		}
		out[field] = attachmentDescriptor(rec)
	}
	if len(adHocNames) > 0 {
		descriptors := make([]any, 0, len(adHocNames))
		for _, name := range adHocNames {
			rec, ok := promoted[name]
			if !ok {
				return nil, fmt.Errorf("attachment %q was not promoted", name)
			}
			descriptors = append(descriptors, attachmentDescriptor(rec))
		}
		out[answerUploadsKey] = descriptors
	}
	return out, nil
}
