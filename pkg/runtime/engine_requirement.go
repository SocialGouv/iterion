package runtime

import (
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// A bundle and the engine that evaluates it are shipped by two different
// mechanisms — a bundle is pushed or checked out in a second, an engine is
// installed or deployed. When the bundle uses something the engine does not
// have, the workflow COMPILES (a builtin call parses generically) and dies at
// its first evaluation, which is how a production bot burned seven pods in
// ten minutes on 2026-09-06.
//
// A manifest may declare the build it needs (`requires.iterion`, pkg/bundle).
// This gate is where every NON-cloud launch surface honours it in ONE place:
// `iterion run` / `resume`, the studio, the dispatcher's direct engine path,
// and a subbot child all reach Engine.Run or Engine.Resume with the bundle
// attached. The cloud runner keeps its own gate ahead of this one — it must
// also decide the delivery (ack, never redeliver), which is knowledge the
// engine does not have.

// engineBuild names the build a bundle's requirement is held against. A var so
// a test can pin it: `go test` binaries carry appinfo.Version = "dev", which
// is deliberately unorderable, so the comparison would otherwise never run
// under test and every assertion would pass for the wrong reason.
var engineBuild = appinfo.FullVersion

// refuseBundleRequiringNewerEngine reports why this build must not execute the
// attached bundle, or nil.
//
// A run with no bundle, or a bundle declaring nothing, passes silently. A
// requirement this build cannot ORDER (a `dev` build, a fork's naming scheme)
// warns and passes: refusing would stop every developer build, and passing in
// silence would make the declaration a decoration.
func (e *Engine) refuseBundleRequiringNewerEngine() error {
	if e.bundle == nil || e.bundle.Manifest == nil {
		return nil
	}
	build := engineBuild()
	switch verdict, reason := bundle.CheckManifestEngine(e.bundle.Manifest, build); verdict {
	case bundle.EngineTooOld:
		return &RuntimeError{
			Code:    ErrCodeBotRequiresNewerEngine,
			Message: reason,
			Hint:    "upgrade iterion to the build the bot declares, or lower its manifest's requires.iterion",
		}
	case bundle.EngineUnknown:
		if e.logger != nil {
			e.logger.Warn("runtime: %s — the run proceeds unchecked", reason)
		}
	}
	return nil
}
