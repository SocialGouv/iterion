package runtime

import (
	"context"
	"path"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/store"
)

// attachmentsContainerPath is where the per-run attachments directory is
// bind-mounted inside a sandbox. Declared here (and consumed by
// startSandbox) so the mount target and the paths handed to agents can
// never drift apart — the class of bug this file exists to close.
const attachmentsContainerPath = "/run/iterion/attachments"

// answerAttachmentKey is the descriptor field naming the run attachment
// an operator-uploaded answer was promoted into. Must match the key the
// HTTP layer writes (pkg/server/runs_answer_uploads.go).
const answerAttachmentKey = "attachment"

// answerUploadsKey is the reserved answer key holding ad-hoc gate
// attachments (the 📎 button, no DSL involved). Mirrors the constant in
// pkg/server/runs_answer_uploads.go.
const answerUploadsKey = "_attachments"

// resolveFileAnswers fills in the `path` of every file descriptor an
// operator's answers carry, mutating answers in place.
//
// The server promoted the uploaded bytes to a run attachment and wrote a
// descriptor WITHOUT a path, because the correct path depends on
// something only the engine knows: whether the nodes that will read the
// file run on the host or inside a sandbox container. Resolution is
// deliberately by attachment NAME rather than by trusting anything
// client-supplied — a descriptor naming an attachment the run does not
// have resolves to no path at all rather than to an arbitrary filesystem
// location.
//
// Covers both shapes: a declared `file` field (descriptor at the top
// level of answers) and the reserved `_attachments` list.
func (e *Engine) resolveFileAnswers(ctx context.Context, runID string, answers map[string]any) {
	if e == nil || len(answers) == 0 || e.store == nil {
		return
	}
	// Single store round-trip, indexed by name: an answer may carry
	// several files, and each would otherwise re-list.
	var byName map[string]store.AttachmentRecord
	index := func() map[string]store.AttachmentRecord {
		if byName != nil {
			return byName
		}
		byName = make(map[string]store.AttachmentRecord)
		list, err := e.store.ListAttachments(ctx, runID)
		if err != nil {
			if e.logger != nil {
				e.logger.Warn("runtime: resolve file answers: list attachments for run %s: %v", runID, err)
			}
			return byName
		}
		for _, rec := range list {
			byName[rec.Name] = rec
		}
		return byName
	}

	resolve := func(v any) {
		desc, ok := v.(map[string]any)
		if !ok {
			return
		}
		name, _ := desc[answerAttachmentKey].(string)
		if name == "" {
			return
		}
		rec, found := index()[name]
		if !found {
			if e.logger != nil {
				e.logger.Warn("runtime: answer references attachment %q which run %s does not have; leaving path unset", name, runID)
			}
			return
		}
		if p := e.attachmentPath(rec); p != "" {
			desc["path"] = p
		}
	}

	for key, val := range answers {
		if key == answerUploadsKey {
			if list, ok := val.([]any); ok {
				for _, item := range list {
					resolve(item)
				}
			}
			continue
		}
		resolve(val)
	}
}

// attachmentsDir returns the directory the RUNNING NODES open this run's
// attachments under, or "" when that is the host filesystem.
//
// Once startSandbox has run this is a fact: it reports the mount that
// actually landed, so a sandbox-by-default run that degraded to
// unsandboxed (no container runtime on the host) and a driver that drops
// host bind mounts (kubernetes) both correctly answer "host".
//
// Before that point — only the resume path, which resolves operator file
// answers while rebuilding state, ahead of the bootstrap — the answer is
// a forecast. It runs the same three checks the bootstrap will, in the
// same order, so the two agree except when the host changes underneath
// (a docker daemon dying mid-resume), which the settled value then
// corrects for every later reader.
func (e *Engine) attachmentsDir() string {
	if e == nil {
		return ""
	}
	if e.sandboxSettled {
		return e.attachmentsContainerDir
	}
	return e.predictAttachmentsDir()
}

// predictAttachmentsDir mirrors resolveAndStartSandbox's own decision
// chain without touching a daemon or starting anything: the spec must
// resolve to an active mode, a driver must be selectable for it (this is
// where a host with no docker/podman drops out — resolveAndStartSandbox
// degrades to unsandboxed at the very same call), and that driver must
// support the host bind mount the attachments dir rides on.
func (e *Engine) predictAttachmentsDir() string {
	if e.workflow == nil {
		return ""
	}
	spec, _, _, err := resolveSandboxSpec(
		e.workflow,
		e.repoRoot,
		e.sandboxOverride,
		e.sandboxDefault,
		resolveDefaultSandboxImage(e.sandboxDefaultImage),
	)
	if err != nil || spec == nil || !spec.Mode.IsActive() {
		return ""
	}
	driver, err := selectSandboxDriver(spec, nil)
	if err != nil || driver == nil {
		return ""
	}
	// noop is "opted into a sandbox that isn't one" — it starts no
	// container, so nothing is mounted anywhere.
	if driver.Name() == "noop" || !driver.Capabilities().SupportsHostBindMounts {
		return ""
	}
	return attachmentsContainerPath
}

// attachmentPath returns the path at which the RUNNING NODES can open an
// attachment's bytes — which is not always the path the server wrote them
// to.
//
// A run attachment lives on the host under
// `<store-root>/runs/<id>/attachments/<name>/<filename>`. When the run is
// sandboxed that directory is bind-mounted read-only at
// attachmentsContainerPath, and the host path does not exist inside the
// container. Handing an agent the host path there produces the worst kind
// of failure: the agent gets a plausible, well-formed path, tries to read
// it, gets ENOENT, and improvises — usually by inventing a substitute
// file and reporting success.
//
// Returns "" when the store has no filesystem root (cloud/S3-backed
// stores); those callers fall back to the presigned URL accessor, which
// is why AttachmentInfo carries both.
func (e *Engine) attachmentPath(rec store.AttachmentRecord) string {
	if e == nil || e.store == nil || rec.StorageRef == "" {
		return ""
	}
	root := e.store.Root()
	if root == "" {
		// Non-FS store: nothing is bind-mounted and there is no host
		// path to hand out. URL accessor only.
		return ""
	}
	if dir := e.attachmentsDir(); dir != "" {
		// The mount is the attachments DIRECTORY, so the in-container
		// path mirrors the on-disk layout below it: <name>/<filename>.
		// Built with path (not filepath) — the container is POSIX even
		// when the host is Windows.
		name := rec.Name
		file := rec.OriginalFilename
		if file == "" {
			file = name
		}
		return path.Join(dir, name, file)
	}
	return filepath.Join(root, filepath.FromSlash(rec.StorageRef))
}
