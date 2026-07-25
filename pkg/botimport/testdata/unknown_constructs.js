export const meta = { name: 'oddities' }

function helper(x) { return x.toUpperCase() }

const [a, b] = ['x', 'y']

class Thing { constructor() { this.n = 1 } }

for (const f of ['a.md', 'b.md']) {
  log(`would process ${f}`)
}

try {
  const r = await agent('Do the risky thing.', { label: 'risky' })
} finally {
  log('cleanup')
}

switch (1) { case 1: break }

const total = 1 + 2

await agent(`Finish up. Helper says ${helper('hi')}.`, { label: 'finisher' })
