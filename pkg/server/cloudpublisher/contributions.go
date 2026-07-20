package cloudpublisher

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
	"github.com/SocialGouv/iterion/pkg/pluginsource"
	"github.com/SocialGouv/iterion/pkg/queue"
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
// Returns (nil, nil) when there is genuinely nothing to ship, so the field
// stays absent on the wire. A single unreadable plugin is logged and skipped —
// a broken plugin must not fail a launch — but a referenced library skill that
// is MISSING is only warned about, matching the local path where a DSL `skills:`
// reference is soft.
// resolveContributionsFor is the tenant-aware entry point. tenantID + sources are
// what make an ORG-PRIVATE plugin work: sources are team-scoped git-hosted
// plugins (pkg/pluginsource) whose authority is the durable store, not this
// pod's filesystem — so they survive a restart, unlike a plugin installed into
// the pod's iterion home. A nil resolver keeps the local-only behaviour.
func resolveContributionsFor(
	ctx context.Context,
	wf *ir.Workflow,
	projectStoreDir string,
	tenantID string,
	sources *pluginsource.Resolver,
	logger *iterlog.Logger,
) (*queue.Contributions, error) {
	out := &queue.Contributions{}

	// 0. Team-scoped git-hosted plugins. Resolved FIRST so a locally installed
	// plugin of the same name shadows it deterministically below.
	if sources != nil && tenantID != "" {
		files, err := sources.Resolve(ctx, tenantID)
		if err != nil {
			// Deliberate: a source the operator explicitly enabled that cannot
			// be resolved fails the launch. Shipping the run without its
			// platform skill is the silent-wrong-result failure this exists to
			// prevent.
			return nil, err
		}
		for _, f := range files {
			out.Plugin = append(out.Plugin, queue.ContributionFile{
				Kind: f.Kind, Name: f.Name, Content: f.Content,
			})
		}
	}

	// 1. Enabled plugins' markdown (skills / commands / agents).
	reg, err := plugin.Load()
	if err != nil {
		if logger != nil {
			logger.Warn("cloudpublisher: load plugins for contribution payload: %v — shipping none", err)
		}
	} else {
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
	if wf != nil {
		store := skilllib.LocalStoreForProject(projectStoreDir)
		for _, name := range collectWorkflowSkillRefs(wf) {
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

	if len(out.Plugin) == 0 && len(out.Library) == 0 {
		return nil, nil
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
	if logger != nil {
		logger.Debug("cloudpublisher: shipping %d plugin file(s) + %d library skill(s) (%d bytes) to the runner", len(out.Plugin), len(out.Library), total)
	}
	return out, nil
}

// collectWorkflowSkillRefs returns the deduplicated union of the workflow-level
// `skills:` default and every LLM node's `skills:` list. Mirrors
// runtime.collectSkillRefs — kept here so the publisher does not import the
// engine (the same split as queue.BudgetOverrides vs ir.BudgetOverrides).
func collectWorkflowSkillRefs(wf *ir.Workflow) []string {
	seen := map[string]bool{}
	var out []string
	add := func(names []string) {
		for _, n := range names {
			if n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	add(wf.Skills)
	for _, node := range wf.Nodes {
		if ln, ok := node.(ir.LLMNode); ok {
			add(ln.GetSkills())
		}
	}
	return out
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
