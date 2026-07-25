export const meta = {
  name: 'doc-sweep',
  description: 'Fan a doc rewrite out over modules, then run two independent checks',
}

const PLAN_SCHEMA = {
  type: 'object',
  properties: {
    modules: { type: 'array', items: { type: 'string' }, description: 'Module names to sweep' },
  },
}

const plan = await agent('List the modules whose docs need a sweep.', { label: 'planner', schema: PLAN_SCHEMA })

const rewrites = await Promise.all(plan.modules.map(m => agent(`Rewrite the docs for module ${m}. Keep code samples compiling.`, { label: 'rewriter' })))

await parallel([
  () => agent('Check internal links across the rewritten docs.', { label: 'linkcheck' }),
  () => agent('Check spelling and terminology consistency.', { label: 'spellcheck' }),
])

return { plan, rewrites }
