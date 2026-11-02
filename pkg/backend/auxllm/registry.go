// Package auxllm is the ONE credential channel a server-side auxiliary LLM
// surface resolves through: the merge-conflict resolver
// (pkg/runview/conflict_agent.go), a coordinator's supervisor evaluator
// (pkg/supervise/bot.go) and the session board's curation evaluator
// (pkg/sessionboard/bot.go). These surfaces send run-derived content to a
// model resolved outside the run's own execution, so the registry they
// resolve through is a decision this package makes once — not once per
// surface.
//
// The registry decision today is the server process env: Registry returns
// exactly what model.NewRegistry builds. The funding is not always this
// package's alone: the conflict resolver and the session board resolve
// through plain Resolve, so the env alone funds them, while the supervisor
// evaluator resolves through ResolveWithContext on the run's ctx — where
// the ctx carries the run's sealed credentials (a declared supervisor on a
// runner pod), those credentials fund it and the env is only the fallback,
// a funding its call site chooses.
//
// When the run's gateway credential becomes openable server-side (#2005
// PR-C' reopens the sealed bundle through the stamped key id), the funding
// change lands HERE for the two Resolve surfaces, and at the supervisor's
// ResolveWithContext call with it: its ctx-first precedence keeps spending
// the run's ctx credentials whatever this package funds.
// The HOST/ACCOUNT proof #2645 asks of the pool guard lands with it: the
// guard's openai_compatible check is spelling-only while the env funds the
// call, so a run stamped to a pool is still served from the deployment's
// env, `none` included (docs/sovereign-pools.md, server-side auxiliary
// surfaces).
//
// The construction sweep (channel_test.go) holds the channel's contract:
// outside pkg/backend/model, only the run's own execution planes may build
// a model.Registry directly; every auxiliary surface resolves through
// this package.
package auxllm

import (
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// Registry returns the model registry an auxiliary server-side LLM surface
// resolves its model clients through. Call it INSTEAD of model.NewRegistry:
// the registry an auxiliary surface resolves through is this channel's
// decision, and this function is the one place that decision can change
// in. Funding follows the package doc: env-only for the Resolve surfaces,
// the run's ctx first for the supervisor evaluator.
func Registry() *model.Registry {
	return model.NewRegistry()
}
