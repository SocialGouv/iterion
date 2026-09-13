import { readFileSync } from 'node:fs';

const [manifestPath, ...results] = process.argv.slice(2);
if (!manifestPath || results.length === 0) {
  throw new Error('usage: node scripts/verify-port-tests.mjs MANIFEST GO_TEST_JSON...');
}
const { packages } = JSON.parse(readFileSync(manifestPath, 'utf8'));
const observed = new Map();
const problems = [];
for (const file of results) {
  for (const line of readFileSync(file, 'utf8').split('\n')) {
    if (!line.trim()) continue;
    const event = JSON.parse(line);
    if (!packages[event.Package]) continue;
    if (event.Action === 'fail') problems.push(`failed: ${event.Package}/${event.Test ?? '(package)'}`);
    if (event.Test && ['pass', 'skip', 'fail'].includes(event.Action)) {
      const key = `${event.Package}/${event.Test}`;
      const statuses = observed.get(key) ?? new Set();
      statuses.add(event.Action);
      observed.set(key, statuses);
      if (event.Test.startsWith('TestNative') && event.Action === 'skip') problems.push(`skipped: ${key}`);
    }
  }
}
let required = 0;
for (const [pkg, cases] of Object.entries(packages)) {
  for (const name of cases) {
    required += 1;
    const key = `${pkg}/${name}`;
    const statuses = observed.get(key);
    if (!statuses?.has('pass') || statuses.has('skip') || statuses.has('fail')) {
      problems.push(`required case did not pass without skips: ${key}`);
    }
  }
}
if (required === 0) problems.push('manifest contains no required cases');
if (problems.length) throw new Error(problems.join('\n'));
console.log(`${required} required port-storage cases passed without skips`);
