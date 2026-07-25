export const meta = {
  name: 'lint-fix-loop',
  description: 'Triage lint findings, fix them in a bounded loop, then summarize',
  phases: [
    { title: 'Survey', detail: 'Collect and classify lint findings' },
    { title: 'Fix', detail: 'Fixer and checker alternate, max 4 rounds' },
    { title: 'Wrap', detail: 'Summarize what changed' },
  ],
}

const target = typeof args === 'string' ? args : (args && args.target) || ''
if (!target) log('No target provided.')

const SURVEY_SCHEMA = {
  type: 'object', required: ['severity', 'summary'],
  properties: {
    severity: { type: 'string', enum: ['none', 'minor', 'major'] },
    summary: { type: 'string', description: 'What the linter reports' },
    hotspots: { type: 'array', items: { type: 'string' } },
  },
}
const CHECK_SCHEMA = {
  type: 'object', required: ['verdict'],
  properties: {
    verdict: { type: 'string', enum: ['clean', 'dirty'] },
    remaining: { type: 'integer', description: 'Count of findings left' },
  },
}

phase('Survey')
const survey = await agent(
  `Run the linter on ${target} and classify the findings. Do not fix anything yet.`,
  { label: 'surveyor', phase: 'Survey', schema: SURVEY_SCHEMA }
)

if (!survey || survey.severity === 'none') {
  log('Nothing to fix.')
  return { survey, fixed: false }
}

phase('Fix')
const MAX_ROUNDS = 4
let lastReport = null
for (let round = 1; round <= MAX_ROUNDS; round++) {
  await agent(
    `Fix the lint findings on ${target}.\n\nSurvey summary:\n${survey.summary}` +
    (lastReport ? `\n\nChecker feedback from the previous round:\n${lastReport}` : ''),
    { label: `fixer#${round}`, phase: 'Fix' }
  )
  const check = await agent(
    'Re-run the linter and report the verdict.',
    { label: `checker#${round}`, phase: 'Fix', schema: CHECK_SCHEMA }
  )
  log(`Round ${round}: ${check && check.verdict}`)
  if (check && check.verdict === 'clean') { break }
  lastReport = (check && check.remaining) || 'unknown'
}

phase('Wrap')
const wrap = await agent(
  `Summarize the fixes applied to ${target} and commit them.`,
  { label: 'wrapper', phase: 'Wrap' }
)
return { survey, wrap }
