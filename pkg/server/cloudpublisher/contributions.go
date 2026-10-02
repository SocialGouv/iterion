package cloudpublisher

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
	"github.com/SocialGouv/iterion/pkg/pluginsource"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/skilllib"
)

// maxContributionsBytes caps the contribution payload carried inline on the
// queue message. NATS' default max payload is 1 MiB and the compiled IR shares
// the same envelope, so keep markdown well under it.
//
// Exceeding the cap is an EXPLICIT launch error, never a silent truncation: a
// run that quietly loses its deploy-target skill still "succeeds" while doing
// the wrong thing — exactly the façade this whole channel exists to prevent.
const maxContributionsBytes = 256 * 1024

// resolveContributions reads, from THIS instance's iterion home, the plugin
// markdown contributions of every enabled plugin plus the skill-library skills
// the workflow references, and returns them for the queue message.
//
// It exists because the runner pod that will execute the run has an ephemeral,
// EMPTY iterion home: without shipping the payload, mirrorPluginContributions
// and mirrorLibrarySkills there resolve nothing (only compiled-in builtins),
// so an operator-installed org plugin's skill silently never reaches the
// workspace. The launching instance is the only place that can see them.
//
// Returns the resolved payload — possibly EMPTY but never nil when resolution
// succeeds. An empty payload is a statement ("the launching instance has
// nothing enabled"), and the runner must be able to tell it from a lost field:
// a nil Contributions on the wire is reserved for "no payload arrived", which
// the runner treats as an unverifiable declaration, never as "nothing
// enabled". A single unreadable plugin is logged and skipped — a broken plugin
// must not fail a launch — but a referenced library skill that is MISSING is
// only warned about, matching the local path where a DSL `skills:` reference
// is soft.
// resolveContributionsFor is the tenant-aware entry point. tenantID + sources are
// what make an ORG-PRIVATE plugin work: sources are team-scoped git-hosted
// plugins (pkg/pluginsource) whose authority is the durable store, not this
// pod's filesystem — so they survive a restart, unlike a plugin installed into
// the pod's iterion home. A nil resolver keeps the local-only behaviour. runID
// names the launch in the log line a skipped source produces.
func resolveContributionsFor(
	ctx context.Context,
	wf *ir.Workflow,
	projectStoreDir string,
	tenantID string,
	runID string,
	sources *pluginsource.Resolver,
	logger *iterlog.Logger,
) (*queue.Contributions, error) {
	out := &queue.Contributions{}

	// 0. Team-scoped git-hosted sources. Resolved FIRST so a locally installed
	// plugin of the same name shadows it deterministically below.
	//
	// degraded records that the payload is an AMPUTATION: the enumeration
	// below could not read every declared contribution, so entries the launch
	// pass mirrored may be missing from the wire. The pod cannot detect that
	// by inspecting the payload (the missing entries are missing), so it
	// travels as a fact (queue.Contributions.Degraded) and the runner's
	// mirror pass reads it as "declaration partial → pruner skipped".
	degraded := false
	if sources != nil && tenantID != "" {
		files, skipped, err := sources.Resolve(ctx, tenantID)
		if err != nil {
			// The team's source LIST could not be read: nothing here can tell
			// a healthy team from one whose sources are all lost, so the
			// launch fails with the cause rather than shipping a run that may
			// lack the platform skill it was given.
			return nil, err
		}
		if len(skipped) > 0 {
			degraded = true
		}
		// A source that failed to materialise is skipped for THIS launch and
		// degraded=true rides the payload; the run proceeds without its
		// contributions. One team's broken plugin.yaml must not take every
		// launch of the team down with it — but the skip is never quiet: here
		// against the run, on the record for the studio and the API, and now
		// on the wire to the pod.
		for _, sk := range skipped {
			if logger != nil {
				logger.Warn("cloudpublisher: run %s launches WITHOUT plugin source %q (team %s): %v — the source is flagged degraded; fix it and re-register it, or disable it",
					runID, sk.Source.Name, tenantID, sk.Err)
			}
		}
		// Two enabled team sources shipping <kind>/<same-name>.md would
		// otherwise both ride the payload — one destination on the runner,
		// and the runner mirror's write order would silently pick the winner.
		// Dedup on (kind, name): the LATER source's content replaces the
		// incumbent (Resolver.Resolve iterates ListEnabledByTenant, which
		// has no natural cross-source precedence — the write order is what
		// a redelivery replays, so making it stable here is the point of
		// dedup). The substitution is not silent: the run's log names what
		// got shadowed so an operator can rename. Local plugins of the same
		// name shadow BOTH deterministically in step 1.
		for _, f := range files {
			if replaceContribution(out.Plugin, f.Kind, f.Name, f.Content) {
				if logger != nil {
					logger.Warn("cloudpublisher: run %s: %s %q is contributed by more than one enabled team source (team %s) — one destination, so one of them is shadowed; rename one",
						runID, f.Kind, f.Name, tenantID)
				}
				continue
			}
			out.Plugin = append(out.Plugin, queue.ContributionFile{
				Kind: f.Kind, Name: f.Name, Content: f.Content,
			})
		}
	}

	// 1. Enabled plugins' markdown (skills / commands / agents).
	reg, err := plugin.Load()
	if err != nil {
		// The whole registry is unreadable: whatever a prior pass mirrored on
		// the pod's behalf may be missing from the payload — degraded, and
		// the pod's mirror pass must not bless a prune.
		degraded = true
		if logger != nil {
			logger.Warn("cloudpublisher: load plugins for contribution payload: %v — shipping none, payload flagged degraded", err)
		}
	} else {
		// A broken plugin.yaml makes loadInstalled skip the plugin SILENTLY —
		// err above is nil and the plugin never enters Enabled(), so its
		// files never reach the payload while it is still enabled. The local
		// mirror path vetoes the prune on the same signal
		// (Registry.LoadSkips); the wire carries it as Degraded for the pod.
		if len(reg.LoadSkips()) > 0 {
			degraded = true
			if logger != nil {
				for _, skip := range reg.LoadSkips() {
					logger.Warn("cloudpublisher: plugin load skipped (%s) — payload flagged degraded; the pod will not prune on resumes of this run", skip)
				}
			}
		}
		for _, p := range reg.Enabled() {
			for _, kind := range plugin.MirrorKinds {
				files, ferr := p.MirrorFiles(kind)
				if ferr != nil {
					if logger != nil {
						logger.Warn("cloudpublisher: plugin %q %ss: %v — skipping", p.Name(), kind.Name, ferr)
					}
					continue
				}
				for _, f := range files {
					// A locally installed plugin file shadows a same-named
					// git-hosted one: one (kind, name) must resolve to exactly
					// one payload entry, or the runner's mirror order would
					// decide the winner non-deterministically.
					if replaceContribution(out.Plugin, kind.Dir, f.Name, f.Content) {
						continue
					}
					out.Plugin = append(out.Plugin, queue.ContributionFile{
						Kind:    kind.Dir,
						Name:    f.Name,
						Content: f.Content,
					})
				}
			}
		}
	}

	// 2. Skill-library skills the workflow references via DSL `skills:`.
	// runtime.CollectSkillRefs is the ONE declaration collector: the runner's
	// injected-payload veto verifies exactly the names this ships, so the two
	// must not be able to disagree (a copy here drifted → stationary
	// prune-veto on healthy cloud runs).
	if wf != nil {
		store := skilllib.LocalStoreForProject(projectStoreDir)
		for _, name := range runtime.CollectSkillRefs(wf) {
			sk, gerr := store.Get(name)
			if gerr != nil {
				if logger != nil {
					logger.Warn("cloudpublisher: skill %q referenced by the workflow is not in the skill library — not shipped: %v", name, gerr)
				}
				continue
			}
			out.Library = append(out.Library, queue.LibrarySkillFile{
				Name:        sk.Name,
				Description: sk.Description,
				Content:     []byte(sk.Body),
			})
		}
	}

	total := 0
	for _, f := range out.Plugin {
		total += len(f.Content) + len(f.Name)
	}
	for _, s := range out.Library {
		total += len(s.Content) + len(s.Name) + len(s.Description)
	}
	if total > maxContributionsBytes {
		return nil, fmt.Errorf(
			"cloudpublisher: contribution payload is %d bytes, over the %d-byte queue limit (%d plugin file(s), %d library skill(s)) — disable an unused plugin or trim a large skill",
			total, maxContributionsBytes, len(out.Plugin), len(out.Library))
	}
	out.Degraded = degraded
	if logger != nil {
		logger.Debug("cloudpublisher: shipping %d plugin file(s) + %d library skill(s) (%d bytes, degraded=%t) to the runner", len(out.Plugin), len(out.Library), total, out.Degraded)
	}
	return out, nil
}

// botSourceTenantOf extracts the stored-bundle tenant persisted on the run
// doc ("" for baked/loose bots) — what resume uses to re-resolve the SAME
// tier instead of re-deriving it from a path.
func botSourceTenantOf(ref *runview.BotBundleRef) string {
	if ref == nil {
		return ""
	}
	return ref.TenantID
}

// queueBotBundleRef converts the launch-resolved bundle snapshot/origin to its
// wire mirror. A stored bot's FULL bundle (skills, prompts, devbox,
// attachments) is rebuilt runner-side from this ref — the successor of the
// old appendTenantBotSkills partial transport, which shipped flat skills
// only and let the stale baked bundle's copies shadow them by mirror
// precedence.
func queueBotBundleRef(ref *runview.BotBundleRef) *queue.BotBundleRef {
	if ref == nil {
		return nil
	}
	return &queue.BotBundleRef{TenantID: ref.TenantID, Slug: ref.Slug, Version: ref.Version,
		Snapshot: append([]byte(nil), ref.Snapshot...), SnapshotDigest: ref.SnapshotDigest}
}

// effectiveSandboxImage resolves the deployment's `sandbox: auto` fallback
// image at PUBLISH time (platform runtime setting over the env default) so
// the pinned value rides the message and a redelivery reruns in the same
// environment. Empty when no resolver is wired or no override is set — the
// runner then keeps its own env/built-in resolution.
func (p *Publisher) effectiveSandboxImage(ctx context.Context) string {
	if p.sandboxImage == nil {
		return ""
	}
	return p.sandboxImage(ctx)
}

// replaceContribution overwrites an existing (kind, name) entry in place and
// reports whether it did. Used so a locally installed plugin deterministically
// shadows a same-named git-hosted one, instead of both riding the payload and
// letting the runner's mirror order pick a winner.
func replaceContribution(files []queue.ContributionFile, kind, name string, content []byte) bool {
	for i := range files {
		if files[i].Kind == kind && files[i].Name == name {
			files[i].Content = content
			return true
		}
	}
	return false
}
