// Package auxllm is the ONE credential channel a server-side auxiliary LLM
// surface resolves through: the merge-conflict resolver
// (pkg/runview/conflict_agent.go), a coordinator's supervisor evaluator
// (pkg/supervise/bot.go) and the session board's curation evaluator
// (pkg/sessionboard/bot.go). These surfaces send run-derived content from
// the SERVER process, outside any run's credential scope, so which
// credential funds them is a decision this package makes once — not once
// per surface.
//
// The decision today is the server process env: Registry returns exactly
// what model.NewRegistry builds. When the run's gateway credential becomes
// openable server-side (#2005 PR-C' reopens the sealed bundle through the
// stamped key id), that decision changes HERE — one site instead of three.
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
// the credential a helper's calls are funded with is this channel's
// decision, and this function is the one place that decision can change in.
func Registry() *model.Registry {
	return model.NewRegistry()
}
